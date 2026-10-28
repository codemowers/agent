// Package rabbitmq provides a thread-safe RabbitMQ connection manager.
package rabbitmq

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// PromptMessage is the wire format for prompts and runtime configuration.
type PromptMessage struct {
	// Type is "objective" for the initial task objective or "event" for follow-up events.
	Type        string `json:"type"`
	Body        string `json:"body"`
	RoutingKey  string `json:"routingKey,omitempty"`
	MessageID   string `json:"messageId,omitempty"`
	PodUID      string `json:"podUid,omitempty"`
	ReceivedAt  int64  `json:"receivedAt,omitempty"`
	Generation  int64  `json:"generation,omitempty"`
	IdleSeconds *int32 `json:"idleSeconds,omitempty"`
	Model       string `json:"model"`
	// DisabledMCPs is the Task's complete list on every runtime configuration
	// message; omitted when empty, which the worker reads as none disabled.
	DisabledMCPs []string `json:"disabledMcps,omitempty"`
	// Objective is the Task's current objective on every runtime
	// configuration message, so edits reach the workspace spec.
	Objective string `json:"objective,omitempty"`
}

// Manager holds a lazily-established, auto-reconnecting AMQP connection.
// All exported methods are safe for concurrent use.
type Manager struct {
	url  string
	mu   sync.Mutex
	conn *amqp.Connection
}

func New(url string) *Manager {
	return &Manager{url: url}
}

// channel returns a fresh AMQP channel, re-establishing the connection if necessary.
func (m *Manager) channel() (*amqp.Channel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn == nil || m.conn.IsClosed() {
		conn, err := amqp.Dial(m.url)
		if err != nil {
			return nil, fmt.Errorf("amqp dial: %w", err)
		}
		m.conn = conn
	}
	ch, err := m.conn.Channel()
	if err != nil {
		// Connection silently broken; reset and retry once.
		m.conn.Close()
		m.conn = nil
		conn, err2 := amqp.Dial(m.url)
		if err2 != nil {
			return nil, fmt.Errorf("amqp redial: %w", err2)
		}
		m.conn = conn
		ch, err = m.conn.Channel()
		if err != nil {
			return nil, fmt.Errorf("amqp channel: %w", err)
		}
	}
	return ch, nil
}

// DeclareQueue ensures a durable queue named after the task exists.
// Queue name convention: "{namespace}/{taskName}".
func (m *Manager) DeclareQueue(name string) error {
	ch, err := m.channel()
	if err != nil {
		return err
	}
	defer ch.Close()
	_, err = ch.QueueDeclare(name, true, false, false, false, nil)
	return err
}

// ProbeQueue names the short-lived control queue for one worker incarnation.
func ProbeQueue(taskQueue, podUID string) string {
	return taskQueue + ".mcp-probe." + podUID
}

// DeclareProbeQueue creates a non-durable queue that expires after the worker
// disappears, including when a Pod exits without cleaning up its queue.
func (m *Manager) DeclareProbeQueue(name string) error {
	ch, err := m.channel()
	if err != nil {
		return err
	}
	defer ch.Close()
	_, err = ch.QueueDeclare(name, false, false, false, false, amqp.Table{"x-expires": int32(86400000)})
	return err
}

// QueueDepth returns the number of ready messages waiting for a worker.
func (m *Manager) QueueDepth(name string) (int, error) {
	ch, err := m.channel()
	if err != nil {
		return 0, err
	}
	defer ch.Close()
	queue, err := ch.QueueInspect(name)
	if err != nil {
		return 0, err
	}
	return queue.Messages, nil
}

// DeleteQueue removes the task queue. Silently succeeds if the queue does not exist.
func (m *Manager) DeleteQueue(name string) error {
	ch, err := m.channel()
	if err != nil {
		return err
	}
	defer ch.Close()
	_, err = ch.QueueDelete(name, false, false, false)
	return err
}

// Publish serialises msg and delivers it persistently to the named queue via
// the default exchange. The queue must already exist (call DeclareQueue first).
func (m *Manager) Publish(ctx context.Context, queue string, msg PromptMessage) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal prompt: %w", err)
	}
	ch, err := m.channel()
	if err != nil {
		return err
	}
	defer ch.Close()
	if err := ch.Confirm(false); err != nil {
		return fmt.Errorf("enable publisher confirms: %w", err)
	}
	confirms := ch.NotifyPublish(make(chan amqp.Confirmation, 1))
	returns := ch.NotifyReturn(make(chan amqp.Return, 1))
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := ch.PublishWithContext(ctx,
		"",    // default exchange — routes directly to queue by name
		queue, // routing key = queue name
		true,  // mandatory: fail if queue does not exist
		false,
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			MessageId:    msg.MessageID,
			Timestamp:    time.Now(),
			Body:         body,
		},
	); err != nil {
		return err
	}
	select {
	case confirmation, ok := <-confirms:
		if !ok || !confirmation.Ack {
			return fmt.Errorf("broker did not confirm prompt delivery")
		}
		select {
		case returned := <-returns:
			return fmt.Errorf("prompt was returned by broker: %s", returned.ReplyText)
		default:
			return nil
		}
	case <-ctx.Done():
		return fmt.Errorf("prompt confirmation: %w", ctx.Err())
	}
}
