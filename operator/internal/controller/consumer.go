// Package controller implements the Agent and Task reconcilers.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	"github.com/codemowers/agent/operator/internal/rabbitmq"
)

// AMQPManager is the subset of rabbitmq.Manager used by the reconcilers.
// Defined as an interface so tests can substitute a stub and main.go can pass nil.
type AMQPManager interface {
	DeclareQueue(name string) error
	QueueDepth(name string) (int, error)
	DeleteQueue(name string) error
	Publish(ctx context.Context, queue string, msg rabbitmq.PromptMessage) error
}

// EventConsumer reads webhook events from a RabbitMQ queue and wakes matched Tasks.
type EventConsumer struct {
	Client     client.Client
	APIReader  client.Reader
	HTTPClient *http.Client
	AMQPURL    string
	Queue      string
	Exchange   string
	AMQP       AMQPManager // publishes follow-up prompts to per-task queues; nil = disabled
	// WebhookPublisher is the broker user of the webhook receiver.
	WebhookPublisher string
}

// Start connects to RabbitMQ and runs the consume loop until ctx is done.
// Reconnects with exponential backoff on failure.
// Implements manager.Runnable so it can be added to the controller-runtime manager.
func (c *EventConsumer) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("consumer")
	backoff := time.Second

	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		conn, ch, err := c.dial()
		if err != nil {
			logger.Error(err, "amqp dial failed, retrying", "backoff", backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
				if backoff < 60*time.Second {
					backoff *= 2
				}
				continue
			}
		}
		backoff = time.Second

		logger.Info("amqp connected", "queue", c.Queue)
		if err := c.consume(ctx, ch); err != nil {
			logger.Error(err, "consume loop exited, reconnecting")
		}
		ch.Close()
		conn.Close()
	}
}

func (c *EventConsumer) dial() (*amqp.Connection, *amqp.Channel, error) {
	conn, err := amqp.Dial(c.AMQPURL)
	if err != nil {
		return nil, nil, err
	}
	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	// QoS: process one message at a time so we don't pile up unacked work.
	if err := ch.Qos(1, 0, false); err != nil {
		ch.Close()
		conn.Close()
		return nil, nil, fmt.Errorf("qos: %w", err)
	}
	return conn, ch, nil
}

func (c *EventConsumer) consume(ctx context.Context, ch *amqp.Channel) error {
	deliveries, err := ch.Consume(c.Queue, "", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("consume: %w", err)
	}
	logger := log.FromContext(ctx).WithName("consumer")
	for {
		select {
		case <-ctx.Done():
			return nil
		case d, ok := <-deliveries:
			if !ok {
				return fmt.Errorf("delivery channel closed")
			}
			if err := c.handle(ctx, d); err != nil {
				logger.Error(err, "handle message", "routingKey", d.RoutingKey, "messageId", d.MessageId)
				// API or broker failures are transient. Preserve the event for a retry.
				d.Nack(false, true)
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(time.Second):
				}
				continue
			}
			d.Ack(false)
		}
	}
}

func (c *EventConsumer) handle(ctx context.Context, d amqp.Delivery) error {
	if d.Headers["x-source"] == "imap" {
		return c.handleMail(ctx, d)
	}
	return c.handleNormalized(ctx, d)
}

func ownedByAgent(task *agentsv1alpha1.Task, name string, uid types.UID) bool {
	for _, owner := range task.OwnerReferences {
		if owner.APIVersion == agentsv1alpha1.GroupVersion.String() && owner.Kind == "Agent" && owner.Name == name && owner.UID == uid {
			return true
		}
	}
	return false
}

// wakeTaskWithContent records an authenticated event and wakes its Task.
func (c *EventConsumer) wakeTaskWithContent(ctx context.Context, task *agentsv1alpha1.Task, routingKey, messageID, source, from, content, sourceURL string, coalesceFor time.Duration) error {
	now := time.Now().UnixMilli()
	until, _ := strconv.ParseInt(task.Annotations["codemowers.cloud/coalesce-until"], 10, 64)
	firstInWindow := coalesceFor == 0 || now >= until
	body := fmt.Sprintf("New event on %s — read the latest updates and decide what to do next.", routingKey)
	if content != "" {
		body = content
	}
	if firstInWindow && c.AMQP != nil {
		var agent agentsv1alpha1.Agent
		if err := c.Client.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: task.Spec.AgentRef}, &agent); err != nil {
			return err
		}
		queue := agentsv1alpha1.TaskQueue(task, &agent)
		msg := rabbitmq.PromptMessage{
			Type:       "event",
			Body:       body,
			RoutingKey: routingKey,
			MessageID:  messageID,
			ReceivedAt: now,
		}
		if err := c.AMQP.Publish(ctx, queue, msg); err != nil {
			return err
		}
	}
	if messageID != "" {
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var current agentsv1alpha1.Task
			if err := c.Client.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: task.Name}, &current); err != nil {
				return fmt.Errorf("get conversation Task %s/%s: %w", task.Namespace, task.Name, err)
			}
			transport := "webhook/" + source
			if source == "imap" {
				transport = "imap"
			}
			if !current.Status.AppendMessage(agentsv1alpha1.TaskMessage{ID: "event-" + messageID, Role: "external", Source: transport,
				Actor: from, Content: body, URL: sourceURL, CreatedAt: metav1.NewTime(time.UnixMilli(now))}) {
				return nil
			}
			return c.Client.Status().Update(ctx, &current)
		}); err != nil {
			return err
		}
	}
	patch := client.MergeFrom(task.DeepCopy())
	if task.Annotations == nil {
		task.Annotations = make(map[string]string)
	}
	task.Annotations["codemowers.cloud/event-routing-key"] = routingKey
	task.Annotations["codemowers.cloud/event-message-id"] = messageID
	if messageID != "" {
		var receipts []string
		_ = json.Unmarshal([]byte(task.Annotations["codemowers.cloud/delivery-receipts"]), &receipts)
		receipts = append(receipts, messageID)
		if len(receipts) > 128 {
			receipts = receipts[len(receipts)-128:]
		}
		encoded, err := json.Marshal(receipts)
		if err != nil {
			return err
		}
		task.Annotations["codemowers.cloud/delivery-receipts"] = string(encoded)
	}
	task.Annotations["codemowers.cloud/event-received-at"] = strconv.FormatInt(now, 10)
	if firstInWindow {
		// The first event wakes immediately. Later events receive a trailing wake.
		task.Annotations["codemowers.cloud/wake-pending"] = "true"
		if coalesceFor > 0 {
			task.Annotations["codemowers.cloud/coalesce-until"] = strconv.FormatInt(now+coalesceFor.Milliseconds(), 10)
			task.Annotations["codemowers.cloud/coalesce-trailing"] = "false"
		}
	} else {
		task.Annotations["codemowers.cloud/coalesce-trailing"] = "true"
	}
	if err := c.Client.Patch(ctx, task, patch); err != nil {
		return err
	}
	return nil
}

func taskHasDelivery(task *agentsv1alpha1.Task, messageID string) bool {
	if messageID == "" {
		return false
	}
	if task.Annotations["codemowers.cloud/initial-delivery-id"] == messageID ||
		task.Annotations["codemowers.cloud/event-message-id"] == messageID {
		return true
	}
	var receipts []string
	if json.Unmarshal([]byte(task.Annotations["codemowers.cloud/delivery-receipts"]), &receipts) != nil {
		return false
	}
	for _, receipt := range receipts {
		if receipt == messageID {
			return true
		}
	}
	return false
}
