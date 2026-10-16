// agent-webhook: authenticated webhook ingestion handler.
// Validates HMAC signatures, extracts a routing key, and publishes to RabbitMQ.
// Supports GitHub, Gitea (identical HMAC scheme), Slack and Alertmanager.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

func amqpURL() string {
	if u := os.Getenv("AMQP_URL"); u != "" {
		return u
	}
	host := envOrDefault("AMQP_HOST", "localhost")
	port := envOrDefault("AMQP_PORT", "5672")
	user := envOrDefault("AMQP_USERNAME", "guest")
	pass := envOrDefault("AMQP_PASSWORD", "guest")
	vhost := envOrDefault("AMQP_VHOST", "/")
	if vhost != "/" {
		vhost = "/" + vhost
	}
	return fmt.Sprintf("amqp://%s:%s@%s:%s%s", user, pass, host, port, vhost)
}

func main() {
	addr := envOrDefault("LISTEN_ADDR", ":8080")
	exchange := envOrDefault("AMQP_EXCHANGE", "agent.webhook.events")

	url := amqpURL()
	conn, ch, err := dialAMQP(url)
	if err != nil {
		slog.Error("amqp dial failed", "err", err)
		os.Exit(1)
	}
	pub := &publisher{url: url, conn: conn, ch: ch, exchange: exchange}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("POST /hooks/github/{agent}", pub.githubHandler)
	mux.HandleFunc("POST /hooks/gitea/{agent}", pub.giteaHandler)
	mux.HandleFunc("POST /hooks/alertmanager/{agent}", pub.alertmanagerHandler)
	mux.HandleFunc("POST /hooks/slack/{agent}", pub.slackHandler)

	slog.Info("webhook handler listening", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}

// publisher holds the AMQP connection and publishes events with a routing key.
// It reconnects transparently on channel or connection failure.
type publisher struct {
	url      string
	conn     *amqp.Connection
	ch       *amqp.Channel
	exchange string
}

func (p *publisher) githubHandler(w http.ResponseWriter, r *http.Request) {
	agent := r.PathValue("agent")
	secret := secretForAgent(agent)
	deliveryID := r.Header.Get("X-GitHub-Delivery")
	event := r.Header.Get("X-GitHub-Event")
	sig := r.Header.Get("X-Hub-Signature-256")

	body, ok := readAndVerifyHMAC(w, r, secret, sig, "sha256=")
	if !ok {
		return
	}

	enterpriseHost := r.Header.Get("X-GitHub-Enterprise-Host")
	routingKey, err := githubRoutingKey(enterpriseHost, event, body)
	if err != nil {
		slog.Warn("routing key extraction failed", "event", event, "err", err)
		routingKey = fmt.Sprintf("github.%s.unknown", event)
	}

	p.respond(w, p.publish(routingKey, deliveryID, "github", event, body))
}

func (p *publisher) giteaHandler(w http.ResponseWriter, r *http.Request) {
	agent := r.PathValue("agent")
	secret := secretForAgent(agent)
	deliveryID := r.Header.Get("X-Gitea-Delivery")
	event := r.Header.Get("X-Gitea-Event")
	sig := r.Header.Get("X-Hub-Signature-256")

	body, ok := readAndVerifyHMAC(w, r, secret, sig, "sha256=")
	if !ok {
		return
	}

	// Gitea mirrors GitHub payload shapes for issues/PRs.
	routingKey, err := githubRoutingKey("", event, body)
	if err != nil {
		routingKey = fmt.Sprintf("gitea.%s.unknown", event)
	}

	p.respond(w, p.publish(routingKey, deliveryID, "gitea", event, body))
}

// alertmanagerHandler authenticates with a bearer token and publishes one
// message per alert fingerprint so each alert routes to its own Task.
func (p *publisher) alertmanagerHandler(w http.ResponseWriter, r *http.Request) {
	agent := r.PathValue("agent")
	token := secretForAgentKey(agent, "alertmanager-token")
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if len(token) == 0 || subtle.ConstantTimeCompare(token, []byte(got)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	events, err := splitAlerts(body)
	if err != nil {
		http.Error(w, "malformed payload", http.StatusBadRequest)
		return
	}
	for _, e := range events {
		if status := p.publish(e.key, agent+"."+e.id, "alertmanager", "webhook", e.body); status != http.StatusAccepted {
			http.Error(w, "publish error", status)
			return
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

type alertEvent struct {
	key, id string
	body    []byte
}

// splitAlerts turns a grouped Alertmanager batch into one event per alert.
// Routing key: alertmanager:{fingerprint}. The delivery ID includes status
// and startsAt/endsAt so a firing and its resolution deduplicate separately.
// Group metadata (groupKey, commonLabels, receiver) is retained in each body.
func splitAlerts(body []byte) ([]alertEvent, error) {
	var batch map[string]json.RawMessage
	if err := json.Unmarshal(body, &batch); err != nil {
		return nil, err
	}
	var alerts []map[string]json.RawMessage
	if err := json.Unmarshal(batch["alerts"], &alerts); err != nil || len(alerts) == 0 {
		return nil, fmt.Errorf("no alerts")
	}
	var out []alertEvent
	for _, a := range alerts {
		var fp, status, starts, ends string
		json.Unmarshal(a["fingerprint"], &fp)
		json.Unmarshal(a["status"], &status)
		json.Unmarshal(a["startsAt"], &starts)
		json.Unmarshal(a["endsAt"], &ends)
		if fp == "" {
			return nil, fmt.Errorf("alert without fingerprint")
		}
		single := map[string]json.RawMessage{}
		for k, v := range batch {
			if k != "alerts" {
				single[k] = v
			}
		}
		single["alerts"], _ = json.Marshal([]map[string]json.RawMessage{a})
		b, _ := json.Marshal(single)
		out = append(out, alertEvent{key: "alertmanager:" + fp, id: fmt.Sprintf("%s.%s.%s.%s", fp, status, starts, ends), body: b})
	}
	return out, nil
}

// slackHandler verifies X-Slack-Signature (v0 HMAC over "v0:ts:body"), answers
// url_verification, and publishes event_callback payloads keyed by thread.
func (p *publisher) slackHandler(w http.ResponseWriter, r *http.Request) {
	agent := r.PathValue("agent")
	secret := secretForAgentKey(agent, "slack-signing")
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	ts := r.Header.Get("X-Slack-Request-Timestamp")
	tsInt, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(secret) == 0 || time.Since(time.Unix(tsInt, 0)).Abs() > 5*time.Minute {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("v0:" + ts + ":"))
	mac.Write(body)
	want := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(want), []byte(r.Header.Get("X-Slack-Signature"))) != 1 {
		http.Error(w, "signature mismatch", http.StatusUnauthorized)
		return
	}
	key, eventID, challenge, err := slackRoutingKey(body)
	if err != nil {
		http.Error(w, "malformed payload", http.StatusBadRequest)
		return
	}
	if challenge != "" {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, challenge)
		return
	}
	if key == "" {
		w.WriteHeader(http.StatusAccepted) // unroutable event types are ignored
		return
	}
	if status := p.publish(key, eventID, "slack", "event_callback", body); status != http.StatusAccepted {
		http.Error(w, "publish error", status)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// slackRoutingKey: slack:{team}:{channel}:{thread-ts} for channel threads
// (root ts when the message starts a thread) and slack:{team}:dm:{user} for DMs.
func slackRoutingKey(body []byte) (key, eventID, challenge string, err error) {
	var p struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
		TeamID    string `json:"team_id"`
		EventID   string `json:"event_id"`
		Event     struct {
			Type        string `json:"type"`
			User        string `json:"user"`
			BotID       string `json:"bot_id"`
			Channel     string `json:"channel"`
			ChannelType string `json:"channel_type"`
			TS          string `json:"ts"`
			ThreadTS    string `json:"thread_ts"`
		} `json:"event"`
	}
	if err = json.Unmarshal(body, &p); err != nil {
		return
	}
	if p.Type == "url_verification" {
		return "", "", p.Challenge, nil
	}
	e := p.Event
	if p.Type != "event_callback" || p.TeamID == "" || e.BotID != "" {
		return "", p.EventID, "", nil // ignore bot chatter and unknown types
	}
	eventID = p.EventID
	if e.ChannelType == "im" {
		return fmt.Sprintf("slack:%s:dm:%s", p.TeamID, e.User), eventID, "", nil
	}
	thread := e.ThreadTS
	if thread == "" {
		thread = e.TS
	}
	if e.Channel == "" || thread == "" {
		return "", eventID, "", nil
	}
	return fmt.Sprintf("slack:%s:%s:%s", p.TeamID, e.Channel, thread), eventID, "", nil
}

func (p *publisher) respond(w http.ResponseWriter, status int) {
	if status != http.StatusAccepted {
		http.Error(w, http.StatusText(status), status)
		return
	}
	w.WriteHeader(status)
}

// channel returns a usable AMQP channel, reconnecting if the connection is broken.
func (p *publisher) channel() (*amqp.Channel, error) {
	if p.conn == nil || p.conn.IsClosed() {
		conn, ch, err := dialAMQP(p.url)
		if err != nil {
			return nil, err
		}
		p.conn = conn
		p.ch = ch
	}
	if p.ch == nil {
		ch, err := p.conn.Channel()
		if err != nil {
			return nil, err
		}
		p.ch = ch
	}
	return p.ch, nil
}

func (p *publisher) publish(routingKey, messageID, source, event string, body []byte) int {
	headers := amqp.Table{
		"x-source":   source,
		"x-event":    event,
		"x-routing":  routingKey,
		"x-msg-id":   messageID,
		"x-received": time.Now().UTC().Format(time.RFC3339),
	}

	ch, err := p.channel()
	if err != nil {
		slog.Error("amqp channel failed", "err", err)
		return http.StatusServiceUnavailable
	}
	err = ch.PublishWithContext(
		context.Background(),
		p.exchange,
		routingKey,
		false, false,
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			MessageId:    messageID,
			Timestamp:    time.Now(),
			Headers:      headers,
			Body:         body,
		},
	)
	if err != nil {
		// Channel may be broken; reset so next request reconnects.
		p.ch = nil
		slog.Error("publish failed", "routingKey", routingKey, "err", err)
		return http.StatusInternalServerError
	}
	slog.Info("published", "routingKey", routingKey, "messageId", messageID)
	return http.StatusAccepted
}

// githubRoutingKey builds a deterministic routing key from the event type and payload.
//
// Examples:
//
//	github:codemowers/agent#issue:8
//	github.ghe.myco.com:myorg/myrepo#pr:42
//	github:codemowers/agent#push
func githubRoutingKey(enterpriseHost, event string, body []byte) (string, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}

	prefix := "github"
	if enterpriseHost != "" {
		prefix = "github." + enterpriseHost
	}

	repo := ""
	if raw, ok := payload["repository"]; ok {
		var repoObj struct {
			FullName string `json:"full_name"`
		}
		if err := json.Unmarshal(raw, &repoObj); err == nil {
			repo = repoObj.FullName
		}
	}

	base := prefix
	if repo != "" {
		base = prefix + ":" + repo
	}

	switch event {
	case "issues", "issue_comment":
		if raw, ok := payload["issue"]; ok {
			var issue struct {
				Number int `json:"number"`
			}
			if err := json.Unmarshal(raw, &issue); err == nil && issue.Number > 0 {
				return fmt.Sprintf("%s#issue:%d", base, issue.Number), nil
			}
		}
	case "pull_request", "pull_request_review", "pull_request_review_comment":
		if raw, ok := payload["pull_request"]; ok {
			var pr struct {
				Number int `json:"number"`
			}
			if err := json.Unmarshal(raw, &pr); err == nil && pr.Number > 0 {
				return fmt.Sprintf("%s#pr:%d", base, pr.Number), nil
			}
		}
	case "workflow_run", "check_run", "check_suite":
		if raw, ok := payload["workflow_run"]; ok {
			var wr struct {
				HeadBranch string `json:"head_branch"`
			}
			if err := json.Unmarshal(raw, &wr); err == nil && wr.HeadBranch != "" {
				return fmt.Sprintf("%s#ci:%s", base, wr.HeadBranch), nil
			}
		}
		return base + "#ci", nil
	case "push":
		return base + "#push", nil
	case "ping":
		return base + "#ping", nil
	}

	return base + "#" + event, nil
}

// readAndVerifyHMAC reads the request body and verifies HMAC-SHA256 against the secret.
func readAndVerifyHMAC(w http.ResponseWriter, r *http.Request, secret []byte, sig, prefix string) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return nil, false
	}
	if len(secret) == 0 {
		http.Error(w, "webhook secret not configured", http.StatusUnauthorized)
		return nil, false
	}
	if !strings.HasPrefix(sig, prefix) {
		http.Error(w, "missing or malformed signature", http.StatusUnauthorized)
		return nil, false
	}
	sigBytes, err := hex.DecodeString(strings.TrimPrefix(sig, prefix))
	if err != nil {
		http.Error(w, "invalid signature encoding", http.StatusUnauthorized)
		return nil, false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), sigBytes) {
		http.Error(w, "signature mismatch", http.StatusUnauthorized)
		return nil, false
	}
	return body, true
}

// secretForAgent reads the HMAC secret for the named agent.
// Secrets are mounted at /secrets/{agent}/token (from agent-hook-* Secrets).
// Falls back to WEBHOOK_SECRET env var for single-agent deployments.
func secretForAgent(agent string) []byte {
	return secretForAgentKey(agent, "token")
}

// secretForAgentKey reads /secrets/{agent}/{key}.
func secretForAgentKey(agent, key string) []byte {
	path := fmt.Sprintf("/secrets/%s/%s", agent, key)
	if data, err := os.ReadFile(path); err == nil {
		return []byte(strings.TrimSpace(string(data)))
	}
	if s := os.Getenv("WEBHOOK_SECRET"); s != "" {
		return []byte(s)
	}
	return nil
}

func dialAMQP(url string) (*amqp.Connection, *amqp.Channel, error) {
	conn, err := amqp.Dial(url)
	if err != nil {
		return nil, nil, err
	}
	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	if err := ch.Confirm(false); err != nil {
		ch.Close()
		conn.Close()
		return nil, nil, fmt.Errorf("confirm mode: %w", err)
	}
	return conn, ch, nil
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
