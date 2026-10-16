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

// PromptMessage is the wire format for messages published to task queues.
type PromptMessage struct {
	// Type is "objective" for the initial task objective or "event" for follow-up events.
	Type       string `json:"type"`
	Body       string `json:"body"`
	RoutingKey string `json:"routingKey,omitempty"`
	MessageID  string `json:"messageId,omitempty"`
	ReceivedAt int64  `json:"receivedAt,omitempty"`
}

// Manager holds a lazily-established, auto-reconnecting AMQP connection.
// All exported methods are safe for concurrent use.
type Manager struct {
	url string
	mu  sync.Mutex
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
	return ch.PublishWithContext(ctx,
		"",    // default exchange — routes directly to queue by name
		queue, // routing key = queue name
		true,  // mandatory: fail if queue does not exist
		false,
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			Timestamp:    time.Now(),
			Body:         body,
		},
	)
}
