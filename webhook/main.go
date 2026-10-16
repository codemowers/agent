// agent-webhook: authenticated webhook ingestion handler.
// Validates HMAC signatures, extracts a routing key, and publishes to RabbitMQ.
// Supports GitHub, Gitea (identical HMAC scheme) and Alertmanager.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
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

	p.publish(w, routingKey, deliveryID, "github", event, body)
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

	p.publish(w, routingKey, deliveryID, "gitea", event, body)
}

func (p *publisher) alertmanagerHandler(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	agent := r.PathValue("agent")
	routingKey, deliveryID := alertmanagerRoutingKey(body)
	p.publish(w, routingKey, agent+"."+deliveryID, "alertmanager", "webhook", body)
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

func (p *publisher) publish(w http.ResponseWriter, routingKey, messageID, source, event string, body []byte) {
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
		http.Error(w, "broker unavailable", http.StatusServiceUnavailable)
		return
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
		http.Error(w, "publish error", http.StatusInternalServerError)
		return
	}
	slog.Info("published", "routingKey", routingKey, "messageId", messageID)
	w.WriteHeader(http.StatusAccepted)
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

// alertmanagerRoutingKey extracts a stable routing key from the first alert's fingerprint.
// Format: alertmanager:{fingerprint}
func alertmanagerRoutingKey(body []byte) (routingKey, deliveryID string) {
	var payload struct {
		Alerts []struct {
			Fingerprint string `json:"fingerprint"`
		} `json:"alerts"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Alerts) == 0 {
		ts := fmt.Sprintf("%d", time.Now().UnixNano())
		return "alertmanager.unknown", ts
	}
	fp := payload.Alerts[0].Fingerprint
	return "alertmanager:" + fp, fp
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
	path := fmt.Sprintf("/secrets/%s/token", agent)
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
