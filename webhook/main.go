// agent-webhook authenticates requests with protocol and binding CRs, evaluates CEL, and
// publishes bounded normalized events to the operator's RabbitMQ intake.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// amqpURL is the broker address without credentials; the receiver logs in
// with its projected ServiceAccount token.
func amqpURL() string {
	if value := os.Getenv("AMQP_URL"); value != "" {
		return value
	}
	return fmt.Sprintf("amqp://%s/%s", net.JoinHostPort(envOrDefault("AMQP_HOST", "localhost"), envOrDefault("AMQP_PORT", "5672")), url.PathEscape(envOrDefault("AMQP_VHOST", "/")))
}

func readToken(path string) (string, error) {
	token, err := os.ReadFile(path)
	return strings.TrimSpace(string(token)), err
}

func main() {
	resolver, err := newKubeHookResolver()
	if err != nil {
		slog.Error("Kubernetes receiver resolver unavailable", "err", err)
		os.Exit(1)
	}
	pub := &publisher{url: amqpURL(), tokenFile: envOrDefault("AGENT_RABBITMQ_TOKEN_FILE", "/var/run/agent-rabbitmq/token"),
		userID: os.Getenv("AMQP_USER_ID"), exchange: envOrDefault("AMQP_EXCHANGE", "agent.intake"), resolver: newCachedResolver(resolver)}
	if pub.userID == "" {
		slog.Error("AMQP_USER_ID is required")
		os.Exit(1)
	}
	pub.mu.Lock()
	_, err = pub.channel()
	pub.mu.Unlock()
	if err != nil {
		slog.Error("RabbitMQ unavailable", "err", err)
		os.Exit(1)
	}
	go pub.refreshToken(5 * time.Minute)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /hooks/{source}", logHook(pub.receiverHandler))
	mux.HandleFunc("POST /hooks/{namespace}/{binding}", logHook(pub.receiverHandler))
	addr := envOrDefault("LISTEN_ADDR", ":8080")
	slog.Info("webhook handler listening", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}

// hookRecorder captures the outcome of a /hooks request for its log line.
type hookRecorder struct {
	http.ResponseWriter
	status    int
	published int
}

func (r *hookRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// logHook logs every hit before parsing and its outcome afterwards. Only
// route names, status and counts are logged: never bodies, headers, query
// strings or keys.
func logHook(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		attrs := []any{"protocol", r.PathValue("source"), "namespace", r.PathValue("namespace"), "binding", r.PathValue("binding")}
		slog.Info("webhook request", attrs...)
		recorder := &hookRecorder{ResponseWriter: w, status: http.StatusOK}
		started := time.Now()
		next(recorder, r)
		slog.Info("webhook response", append(attrs, "status", recorder.status, "published", recorder.published, "duration", time.Since(started))...)
	}
}

type publisher struct {
	mu        sync.Mutex
	url       string
	tokenFile string
	// userID is stamped on every event. RabbitMQ rejects it unless it equals
	// the token subject, so the operator can trust it as the publisher.
	userID   string
	conn     *amqp.Connection
	ch       *amqp.Channel
	exchange string
	resolver receiverResolver
}

func dialAMQP(brokerURL, tokenFile string) (*amqp.Connection, *amqp.Channel, error) {
	token, err := readToken(tokenFile)
	if err != nil {
		return nil, nil, err
	}
	conn, err := amqp.DialConfig(brokerURL, amqp.Config{SASL: []amqp.Authentication{&amqp.PlainAuth{Password: token}}})
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
		return nil, nil, err
	}
	return conn, ch, nil
}
func (p *publisher) channel() (*amqp.Channel, error) {
	if p.ch != nil && !p.ch.IsClosed() {
		return p.ch, nil
	}
	if p.conn != nil {
		p.conn.Close()
	}
	conn, ch, err := dialAMQP(p.url, p.tokenFile)
	if err != nil {
		return nil, err
	}
	p.conn, p.ch = conn, ch
	return ch, nil
}

// refreshToken hands kubelet's rotated token to the open connection before
// the broker stops accepting the old one.
func (p *publisher) refreshToken(interval time.Duration) {
	for range time.Tick(interval) {
		token, err := readToken(p.tokenFile)
		p.mu.Lock()
		if err == nil && p.conn != nil && !p.conn.IsClosed() {
			err = p.conn.UpdateSecret(token, "token refresh")
		}
		p.mu.Unlock()
		if err != nil {
			slog.Error("RabbitMQ token refresh failed", "err", err)
		}
	}
}

// publishEnvelope publishes as the receiver's broker user. The operator
// accepts these events only with that verified user_id. The routing key names
// the binding and x-key carries the resource, so both stay readable in the broker.
func (p *publisher) publishEnvelope(event normalizedEvent, messageID string, payload []byte) error {
	routingKey := "webhook." + event.Namespace + "." + event.Binding
	headers := amqp.Table{"x-namespace": event.Namespace, "x-agent": event.Agent, "x-binding-uid": event.BindingUID, "x-source": event.Protocol, "x-event": event.Type, "x-key": event.Key}
	p.mu.Lock()
	defer p.mu.Unlock()
	ch, err := p.channel()
	if err != nil {
		return err
	}
	confirms := ch.NotifyPublish(make(chan amqp.Confirmation, 1))
	returns := ch.NotifyReturn(make(chan amqp.Return, 1))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = ch.PublishWithContext(ctx, p.exchange, routingKey, true, false, amqp.Publishing{ContentType: "application/json", DeliveryMode: amqp.Persistent, MessageId: messageID, UserId: p.userID, Timestamp: time.Now(), Headers: headers, Body: payload})
	if err != nil {
		ch.Close()
		p.ch = nil
		return err
	}
	select {
	case confirmation, ok := <-confirms:
		if !ok || !confirmation.Ack {
			return fmt.Errorf("RabbitMQ did not confirm event")
		}
		select {
		case returned := <-returns:
			return fmt.Errorf("RabbitMQ returned event: %s", returned.ReplyText)
		default:
		}
		return nil
	case <-ctx.Done():
		ch.Close()
		p.ch = nil
		return ctx.Err()
	}
}
