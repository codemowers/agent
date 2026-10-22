// Package controller implements the Agent and Task reconcilers.
package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
		logger.Info("amqp connected", "queue", c.Queue)
		connected := time.Now()
		err = c.consume(ctx, ch)
		ch.Close()
		conn.Close()
		if err == nil || time.Since(connected) > time.Minute {
			backoff = time.Second
		}
		if err == nil {
			continue
		}
		// A missing queue fails immediately after connecting, so back off here
		// too instead of reconnecting in a tight loop.
		logger.Error(err, "consume loop exited, reconnecting", "backoff", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
			if backoff < 60*time.Second {
				backoff *= 2
			}
		}
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
			handled, err := retryEvent(ctx, func(ctx context.Context) error {
				err := c.handle(ctx, d)
				if err != nil {
					logger.Error(err, "handle message", "routingKey", d.RoutingKey, "messageId", d.MessageId)
				}
				return err
			}, sleepContext)
			if ctx.Err() != nil {
				return nil // The broker redelivers the unacknowledged event.
			}
			if !handled {
				// The intake queue's policy dead-letters it to agent.intake.dead.
				logger.Error(err, "dead-lettering event", "routingKey", d.RoutingKey, "messageId", d.MessageId)
				d.Nack(false, false)
				continue
			}
			d.Ack(false)
		}
	}
}

// maxEventFailures bounds attempts at an event that fails for a reason other
// than an unavailable API server or broker, so it cannot block the shared
// intake queue.
const maxEventFailures = 6

// retryEvent handles an event in place with backoff. Outages retry until they
// end, preserving order; other failures give up after maxEventFailures.
func retryEvent(ctx context.Context, handle func(context.Context) error, sleep func(context.Context, time.Duration) bool) (bool, error) {
	backoff, failures := time.Second, 0
	for {
		err := handle(ctx)
		if err == nil {
			return true, nil
		}
		if !transientEventError(err) {
			if failures++; failures >= maxEventFailures {
				return false, err
			}
		}
		if !sleep(ctx, backoff) {
			return false, ctx.Err()
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func transientEventError(err error) bool {
	var netErr net.Error
	var amqpErr *amqp.Error
	return apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsInternalError(err) || apierrors.IsUnexpectedServerError(err) ||
		apierrors.IsConflict(err) || errors.As(err, &netErr) || errors.As(err, &amqpErr) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, amqp.ErrClosed)
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
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

// taskArrival is an authenticated event accepted for one Task.
type taskArrival struct {
	Key, Receipt, Source, Actor, Text, URL string
	// Binding is the UID of the WebhookBinding or MailBinding that delivered it.
	Binding string
	// Wake publishes the event to the worker, subject to coalescing.
	Wake bool
	// Complete completes the Task with this reason instead of waking it.
	Complete string
}

// recordArrival records an authenticated event on its Task and wakes or
// completes the Task. The first event on a key wakes at once and opens a
// window: another binding's event within it notifies of the same activity,
// such as forge mail after its webhook, and is only recorded; the same
// binding's events receive one trailing wake. The prompt is published before
// the status write, so a failed write redelivers the event instead of losing
// the wake.
func (c *EventConsumer) recordArrival(ctx context.Context, task *agentsv1alpha1.Task, agent *agentsv1alpha1.Agent, a taskArrival) error {
	now := time.Now()
	window := agentsv1alpha1.CoalesceWindow(task, agent)
	open, inWindow := task.Status.Events.Windows[a.Key]
	inWindow = inWindow && now.Before(open.Until.Time)
	if a.Wake && !inWindow && c.AMQP != nil {
		// A Task that never ran a worker has no queue yet.
		queue := agentsv1alpha1.TaskQueue(task, agent)
		if err := trackTaskQueue(ctx, c.Client, c.AMQP, task, queue); err != nil {
			return err
		}
		if err := c.AMQP.DeclareQueue(queue); err != nil {
			return err
		}
		if err := c.AMQP.Publish(ctx, queue, rabbitmq.PromptMessage{
			Type: "event", Body: a.Text, RoutingKey: a.Key, MessageID: a.Receipt, ReceivedAt: now.UnixMilli(),
		}); err != nil {
			return err
		}
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current agentsv1alpha1.Task
		if err := c.Client.Get(ctx, client.ObjectKeyFromObject(task), &current); err != nil {
			return fmt.Errorf("get Task %s/%s: %w", task.Namespace, task.Name, err)
		}
		if current.UID != task.UID || taskHasDelivery(&current, a.Receipt) {
			return nil
		}
		transport := "webhook/" + a.Source
		if a.Source == "imap" || a.Source == "wake" {
			transport = a.Source
		}
		at := metav1.NewTime(now)
		current.Status.AppendMessage(agentsv1alpha1.TaskMessage{ID: "event-" + a.Receipt, Role: "external", Source: transport,
			Actor: a.Actor, Content: a.Text, URL: a.URL, CreatedAt: at})
		events := &current.Status.Events
		events.Receipts = append(events.Receipts, a.Receipt)
		if len(events.Receipts) > 128 {
			events.Receipts = events.Receipts[len(events.Receipts)-128:]
		}
		for key, w := range events.Windows {
			if !w.Trailing && !now.Before(w.Until.Time) {
				delete(events.Windows, key)
			}
		}
		switch {
		case a.Complete != "":
			if !current.Status.Completed() {
				current.Status.Completion = &agentsv1alpha1.TaskCompletion{Reason: a.Complete, Actor: a.Actor, CompletedAt: &at}
				events.WakePending, events.Windows = false, nil
				current.Status.AppendMessage(agentsv1alpha1.TaskMessage{ID: "completion:" + a.Receipt, Role: "system", Source: "operator",
					Actor: a.Actor, Content: "Completed: " + a.Complete, CreatedAt: at})
			}
		case !a.Wake:
		case !inWindow:
			events.WakePending = true
			if window > 0 {
				if events.Windows == nil {
					events.Windows = map[string]agentsv1alpha1.EventWindow{}
				}
				events.Windows[a.Key] = agentsv1alpha1.EventWindow{Binding: a.Binding, Until: metav1.NewTime(now.Add(window))}
			}
		case open.Binding == a.Binding:
			open.Trailing = true
			events.Windows[a.Key] = open
		}
		return c.Client.Status().Update(ctx, &current)
	})
}

func taskHasDelivery(task *agentsv1alpha1.Task, receipt string) bool {
	return receipt != "" && (task.Status.Events.InitialDeliveryID == receipt || slices.Contains(task.Status.Events.Receipts, receipt))
}
