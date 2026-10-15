// Package controller implements the Agent and Task reconcilers.
package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
)

// EventConsumer reads webhook events from a RabbitMQ queue and wakes matched Tasks.
type EventConsumer struct {
	Client   client.Client
	AMQPURL  string
	Queue    string
	Exchange string
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
				// Nack without requeue so the message goes to the dead-letter queue
				// rather than looping indefinitely on a bad payload.
				d.Nack(false, false)
				continue
			}
			d.Ack(false)
		}
	}
}

// parsedEvent holds the structured fields extracted from a routing key.
type parsedEvent struct {
	source    string // github, gitea, alertmanager
	serverURL string // non-empty for GitHub Enterprise
	owner     string
	repo      string
	kind      string // issue, pull_request, alert; empty for push/ping/ci
	id        string
}

// parseRoutingKey converts a routing key into structured fields.
//
// Formats:
//
//	github:org/repo#issue:N        → {source:github, owner:org, repo:repo, kind:issue, id:N}
//	github:org/repo#pr:N           → {source:github, ..., kind:pull_request, id:N}
//	github.ghe.host:org/repo#pr:N  → {source:github, serverURL:ghe.host, ...}
//	alertmanager:fingerprint        → {source:alertmanager, kind:alert, id:fingerprint}
//	github:org/repo#push           → {kind:""} — ignored
func parseRoutingKey(key string) (parsedEvent, bool) {
	ev := parsedEvent{}

	// alertmanager:fingerprint
	if strings.HasPrefix(key, "alertmanager:") {
		ev.source = "alertmanager"
		ev.kind = "alert"
		ev.id = strings.TrimPrefix(key, "alertmanager:")
		return ev, ev.id != ""
	}

	// github[.host]:org/repo[#kind:id]
	sourcePart, rest, hasSep := strings.Cut(key, ":")
	if !hasSep {
		return ev, false
	}

	// sourcePart is "github" or "github.ghe.example.com"
	if strings.HasPrefix(sourcePart, "github.") {
		ev.source = "github"
		ev.serverURL = strings.TrimPrefix(sourcePart, "github.")
	} else if sourcePart == "github" || sourcePart == "gitea" {
		ev.source = sourcePart
	} else {
		return ev, false
	}

	// rest is "org/repo#kind:id" or "org/repo#push"
	repoPart, fragment, hasFragment := strings.Cut(rest, "#")
	parts := strings.SplitN(repoPart, "/", 2)
	if len(parts) == 2 {
		ev.owner = parts[0]
		ev.repo = parts[1]
	}

	if !hasFragment {
		return ev, false // no kind — not actionable
	}

	switch {
	case strings.HasPrefix(fragment, "issue:"):
		ev.kind = "issue"
		ev.id = strings.TrimPrefix(fragment, "issue:")
	case strings.HasPrefix(fragment, "pr:"):
		ev.kind = "pull_request"
		ev.id = strings.TrimPrefix(fragment, "pr:")
	default:
		return ev, false // push, ping, ci — nothing to wake
	}

	return ev, ev.id != "" && ev.owner != "" && ev.repo != ""
}

func (c *EventConsumer) handle(ctx context.Context, d amqp.Delivery) error {
	logger := log.FromContext(ctx).WithName("consumer")

	ev, actionable := parseRoutingKey(d.RoutingKey)
	if !actionable {
		logger.Info("ignoring non-actionable routing key", "routingKey", d.RoutingKey)
		return nil
	}

	// Find Tasks whose externalReferences match this event.
	var taskList agentsv1alpha1.TaskList
	if err := c.Client.List(ctx, &taskList); err != nil {
		return fmt.Errorf("list tasks: %w", err)
	}

	matched := 0
	for i := range taskList.Items {
		task := &taskList.Items[i]
		if !refMatches(task, ev) {
			continue
		}
		matched++
		if err := c.wakeTask(ctx, task, d.RoutingKey, d.MessageId); err != nil {
			logger.Error(err, "wake task failed", "task", task.Name, "namespace", task.Namespace)
		} else {
			logger.Info("woke task", "task", task.Name, "namespace", task.Namespace, "routingKey", d.RoutingKey)
		}
	}

	if matched == 0 {
		logger.Info("no task matched", "routingKey", d.RoutingKey, "source", ev.source, "kind", ev.kind, "id", ev.id)
	}
	return nil
}

// refMatches returns true if any of the Task's externalReferences match the event.
func refMatches(task *agentsv1alpha1.Task, ev parsedEvent) bool {
	for _, ref := range task.Spec.ExternalReferences {
		if ref.Source != ev.source {
			continue
		}
		// ServerURL must match when set (GitHub Enterprise).
		if ev.serverURL != "" && ref.ServerURL != ev.serverURL {
			continue
		}
		// For alertmanager, match by kind and id only.
		if ev.source == "alertmanager" {
			if ref.Kind == "alert" && ref.ID == ev.id {
				return true
			}
			continue
		}
		// For git providers, match owner/repo/kind/id.
		if ref.RepositoryOwner == ev.owner &&
			ref.RepositoryName == ev.repo &&
			ref.Kind == ev.kind &&
			ref.ID == ev.id {
			return true
		}
	}
	return false
}

// wakeTask annotates the Task to record the latest received event, which triggers
// the TaskReconciler and ensures a worker pod is started or resumed.
func (c *EventConsumer) wakeTask(ctx context.Context, task *agentsv1alpha1.Task, routingKey, messageID string) error {
	patch := client.MergeFrom(task.DeepCopy())
	if task.Annotations == nil {
		task.Annotations = make(map[string]string)
	}
	task.Annotations["codemowers.cloud/event-routing-key"] = routingKey
	task.Annotations["codemowers.cloud/event-message-id"] = messageID
	task.Annotations["codemowers.cloud/event-received-at"] = strconv.FormatInt(time.Now().UnixMilli(), 10)
	return c.Client.Patch(ctx, task, patch)
}
