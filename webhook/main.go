// agent-webhook: authenticated webhook ingestion handler.
// Validates HMAC signatures, extracts a routing key, and publishes to RabbitMQ.
// Supports GitHub, Slack and Alertmanager.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
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
	return fmt.Sprintf("amqp://%s@%s/%s", url.UserPassword(user, pass),
		net.JoinHostPort(host, port), url.PathEscape(vhost))
}

func main() {
	addr := envOrDefault("LISTEN_ADDR", ":8080")
	exchange := envOrDefault("AMQP_EXCHANGE", "agent.webhook.events")
	resolver, err := newKubeHookResolver()
	if err != nil {
		slog.Error("Kubernetes Agent resolver unavailable", "err", err)
		os.Exit(1)
	}

	url := amqpURL()
	conn, ch, err := dialAMQP(url)
	if err != nil {
		slog.Error("amqp dial failed", "err", err)
		os.Exit(1)
	}
	pub := &publisher{url: url, conn: conn, ch: ch, exchange: exchange, namespace: os.Getenv("POD_NAMESPACE"), resolver: resolver}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("POST /hooks/github", pub.githubHandler)
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
	mu        sync.Mutex
	url       string
	conn      *amqp.Connection
	ch        *amqp.Channel
	confirms  <-chan amqp.Confirmation
	exchange  string
	namespace string
	resolver  hookResolver
}

func (p *publisher) githubHandler(w http.ResponseWriter, r *http.Request) {
	hookID := r.Header.Get("X-GitHub-Hook-ID")
	if !validHookID(hookID) {
		http.Error(w, "invalid GitHub hook ID", http.StatusBadRequest)
		return
	}
	hooks, err := p.resolver.Resolve(r.Context(), hookID)
	if err != nil {
		slog.Error("resolve GitHub webhook subscribers", "hookID", hookID, "err", err)
		http.Error(w, "GitHub webhook subscribers unavailable", http.StatusServiceUnavailable)
		return
	}
	deliveryID := r.Header.Get("X-GitHub-Delivery")
	event := r.Header.Get("X-GitHub-Event")
	sig := r.Header.Get("X-Hub-Signature-256")
	if deliveryID == "" || len(deliveryID) > 160 || event == "" {
		http.Error(w, "missing GitHub delivery identity", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 25<<20))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "read error", http.StatusBadRequest)
		}
		return
	}
	if len(hooks) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	enterpriseHost := r.Header.Get("X-GitHub-Enterprise-Host")
	sourceHost := "github.com"
	if enterpriseHost != "" {
		sourceHost = strings.ToLower(enterpriseHost)
	}
	var verified []agentHook
	for _, hook := range hooks {
		if serverHost(hook.ServerURL) == sourceHost && validHMAC(hook.Secret, sig, "sha256=", body) {
			verified = append(verified, hook)
		}
	}
	if len(verified) == 0 {
		http.Error(w, "no valid GitHub webhook subscription", http.StatusUnauthorized)
		return
	}
	if !actionableGitHubEvent(event) {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	routingKey, err := githubRoutingKey(enterpriseHost, event, body)
	if err != nil {
		slog.Warn("routing key extraction failed", "event", event, "err", err)
		http.Error(w, "invalid signed repository identity", http.StatusUnprocessableEntity)
		return
	}

	matched := 0
	for _, hook := range verified {
		if !hook.allowsRepository("github", body) {
			continue
		}
		matched++
		if status := p.publishForAgent(hook.Namespace, hook.Agent, hook.HookID, hook.Secret, routingKey, deliveryID, "github", event, body); status != http.StatusAccepted {
			p.respond(w, status)
			return
		}
	}
	if matched == 0 {
		http.Error(w, "no valid GitHub webhook subscription", http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func actionableGitHubEvent(event string) bool {
	switch event {
	case "issues", "issue_comment", "pull_request", "pull_request_review", "pull_request_review_comment":
		return true
	default:
		return false
	}
}

func giteaRoutingKey(event string, body []byte) (string, error) {
	var payload struct {
		Repository struct {
			HTMLURL string `json:"html_url"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}
	repoURL, err := url.Parse(payload.Repository.HTMLURL)
	if err != nil || repoURL.Hostname() == "" {
		return "", fmt.Errorf("repository html_url has no server host")
	}
	key, err := githubRoutingKey("", event, body)
	if err != nil {
		return "", err
	}
	_, rest, ok := strings.Cut(key, ":")
	if !ok {
		return "", fmt.Errorf("repository routing key has no separator")
	}
	return "gitea." + strings.ToLower(repoURL.Hostname()) + ":" + rest, nil
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
		if status := p.publish(agent, e.key, agent+"."+e.id, "alertmanager", "webhook", e.body); status != http.StatusAccepted {
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
	if status := p.publish(agent, key, eventID, "slack", "event_callback", body); status != http.StatusAccepted {
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
		if err := ch.Confirm(false); err != nil {
			ch.Close()
			return nil, err
		}
		p.ch = ch
	}
	if p.confirms == nil {
		p.confirms = p.ch.NotifyPublish(make(chan amqp.Confirmation, 1))
	}
	return p.ch, nil
}

func (p *publisher) publish(agent, routingKey, messageID, source, event string, body []byte) int {
	return p.publishScoped(p.namespace, agent, "", nil, routingKey, messageID, source, event, body)
}

func (p *publisher) publishForAgent(namespace, agent, hookID string, secret []byte, routingKey, messageID, provider, event string, body []byte) int {
	return p.publishScoped(namespace, agent, hookID, secret, routingKey, messageID, provider, event, body)
}

func (p *publisher) publishScoped(namespace, agent, hookID string, secret []byte, routingKey, messageID, source, event string, body []byte) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if namespace == "" || agent == "" {
		slog.Error("webhook routing scope is missing")
		return http.StatusServiceUnavailable
	}
	if messageID == "" {
		sum := sha256.Sum256(append([]byte(namespace+"\x00"+agent+"\x00"+source+"\x00"+event+"\x00"), body...))
		messageID = hex.EncodeToString(sum[:])
	}
	headers := amqp.Table{
		"x-agent":     agent,
		"x-namespace": namespace,
		"x-source":    source,
		"x-hook-id":   hookID,
		"x-event":     event,
		"x-routing":   routingKey,
		"x-msg-id":    messageID,
		"x-received":  time.Now().UTC().Format(time.RFC3339),
	}
	if len(secret) > 0 {
		mac := hmac.New(sha256.New, secret)
		for _, field := range []string{namespace, agent, hookID, source, event, routingKey, messageID} {
			mac.Write([]byte(field))
			mac.Write([]byte{0})
		}
		mac.Write(body)
		headers["x-source-signature"] = hex.EncodeToString(mac.Sum(nil))
	}

	ch, err := p.channel()
	if err != nil {
		slog.Error("amqp channel failed", "err", err)
		return http.StatusServiceUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = ch.PublishWithContext(
		ctx,
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
		ch.Close()
		p.ch = nil
		p.confirms = nil
		slog.Error("publish failed", "routingKey", routingKey, "err", err)
		return http.StatusInternalServerError
	}
	select {
	case confirmation, ok := <-p.confirms:
		if !ok || !confirmation.Ack {
			ch.Close()
			p.ch = nil
			p.confirms = nil
			return http.StatusServiceUnavailable
		}
	case <-ctx.Done():
		ch.Close()
		p.ch = nil
		p.confirms = nil
		return http.StatusServiceUnavailable
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
	repo := ""
	if raw, ok := payload["repository"]; ok {
		var repoObj struct {
			FullName string `json:"full_name"`
			HTMLURL  string `json:"html_url"`
		}
		if err := json.Unmarshal(raw, &repoObj); err == nil {
			repo = repoObj.FullName
			if repoObj.HTMLURL != "" {
				repoURL, err := url.Parse(repoObj.HTMLURL)
				if err != nil || repoURL.Hostname() == "" {
					return "", fmt.Errorf("repository html_url has no server host")
				}
				host := strings.ToLower(repoURL.Hostname())
				if enterpriseHost != "" && !strings.EqualFold(enterpriseHost, host) {
					return "", fmt.Errorf("enterprise host does not match signed repository URL")
				}
				if host != "github.com" {
					prefix = "github." + host
				}
			} else if enterpriseHost != "" {
				return "", fmt.Errorf("enterprise repository URL missing from signed payload")
			}
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
				Number      int             `json:"number"`
				PullRequest json.RawMessage `json:"pull_request"`
			}
			if err := json.Unmarshal(raw, &issue); err == nil && issue.Number > 0 {
				if event == "issue_comment" && len(issue.PullRequest) > 0 && string(issue.PullRequest) != "null" {
					return fmt.Sprintf("%s#pr:%d", base, issue.Number), nil
				}
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
	if !validHMAC(secret, sig, prefix, body) {
		http.Error(w, "signature mismatch", http.StatusUnauthorized)
		return nil, false
	}
	return body, true
}

func validHMAC(secret []byte, sig, prefix string, body []byte) bool {
	if len(secret) == 0 || !strings.HasPrefix(sig, prefix) {
		return false
	}
	sigBytes, err := hex.DecodeString(strings.TrimPrefix(sig, prefix))
	if err != nil || len(sigBytes) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), sigBytes)
}

// secretForAgent reads the HMAC secret mounted for a webhook source.
func secretForAgent(agent string) []byte {
	return secretForAgentKey(agent, "token")
}

var agentNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// secretForAgentKey reads /secrets/{agent}/{key}.
func secretForAgentKey(agent, key string) []byte {
	if len(agent) > 63 || !agentNamePattern.MatchString(agent) {
		return nil
	}
	path := fmt.Sprintf("/secrets/%s/%s", agent, key)
	if data, err := os.ReadFile(path); err == nil {
		return []byte(strings.TrimSpace(string(data)))
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
