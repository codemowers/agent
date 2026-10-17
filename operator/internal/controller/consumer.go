// Package controller implements the Agent and Task reconcilers.
package controller

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
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

// taskQueue returns the canonical RabbitMQ queue name for a Task.
// Format: "{namespace}/{taskName}" — human-readable and inspectable in the management UI.
func taskQueue(namespace, name string) string {
	return namespace + "/" + name
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
}

var errSourceCredentialUnavailable = errors.New("webhook source credential unavailable")

func (c *EventConsumer) verifyAgentEvent(ctx context.Context, d amqp.Delivery, ev parsedEvent) (*agentsv1alpha1.Agent, error) {
	namespace, _ := d.Headers["x-namespace"].(string)
	name, _ := d.Headers["x-agent"].(string)
	event, _ := d.Headers["x-event"].(string)
	hookID, _ := d.Headers["x-hook-id"].(string)
	signature, _ := d.Headers["x-source-signature"].(string)
	provider, _ := d.Headers["x-source"].(string)
	if namespace == "" || name == "" || hookID == "" || event == "" || signature == "" || provider != "github" || provider != ev.source || c.APIReader == nil {
		return nil, fmt.Errorf("forge event has no verified Agent envelope")
	}
	var agent agentsv1alpha1.Agent
	if err := c.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("Agent no longer exists")
		}
		return nil, fmt.Errorf("get webhook Agent: %w", err)
	}
	host := ev.serverURL
	if ev.source == "github" && host == "" {
		host = "github.com"
	}
	configured := false
	for _, git := range agent.Spec.GitProviders {
		if git.Type == ev.source && serverHost(git.ServerURL) == host {
			configured = true
			break
		}
	}
	if !configured {
		return nil, fmt.Errorf("forge event does not match Agent Git provider")
	}
	got, err := hex.DecodeString(signature)
	if err != nil {
		return nil, fmt.Errorf("malformed webhook signature")
	}
	for _, subscription := range agent.Spec.GitHubWebhooks {
		if subscription.HookID != hookID || subscription.SecretRef == "" || serverHost(subscription.ServerURL) != host {
			continue
		}
		var secret corev1.Secret
		if err := c.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: subscription.SecretRef}, &secret); err != nil {
			return nil, fmt.Errorf("%w: %v", errSourceCredentialUnavailable, err)
		}
		key := secret.Data["token"]
		if len(key) == 0 {
			return nil, fmt.Errorf("webhook credential is empty")
		}
		mac := hmac.New(sha256.New, key)
		for _, field := range []string{namespace, name, hookID, provider, event, d.RoutingKey, d.MessageId} {
			mac.Write([]byte(field))
			mac.Write([]byte{0})
		}
		mac.Write(d.Body)
		if hmac.Equal(got, mac.Sum(nil)) {
			return &agent, nil
		}
	}
	return nil, fmt.Errorf("webhook signature mismatch or Agent is not subscribed")
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

	// sourcePart is "github", "github.ghe.example.com", or "gitea.host".
	if strings.HasPrefix(sourcePart, "github.") {
		ev.source = "github"
		ev.serverURL = strings.TrimPrefix(sourcePart, "github.")
	} else if strings.HasPrefix(sourcePart, "gitea.") {
		ev.source = "gitea"
		ev.serverURL = strings.TrimPrefix(sourcePart, "gitea.")
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
	var agents agentsv1alpha1.AgentList
	if ev.source == "github" || ev.source == "gitea" {
		agent, err := c.verifyAgentEvent(ctx, d, ev)
		if err != nil {
			if errors.Is(err, errSourceCredentialUnavailable) {
				return err
			}
			logger.Error(err, "ignoring unverified forge event", "messageId", d.MessageId)
			return nil
		}
		agents.Items = append(agents.Items, *agent)
	} else {
		agentName, _ := d.Headers["x-agent"].(string)
		namespace, _ := d.Headers["x-namespace"].(string)
		if agentName == "" || namespace == "" {
			return nil
		}
		var agent agentsv1alpha1.Agent
		if err := c.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: agentName}, &agent); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("get event Agent: %w", err)
		}
		agents.Items = append(agents.Items, agent)
	}
	for i := range agents.Items {
		agent := &agents.Items[i]
		var taskList agentsv1alpha1.TaskList
		if err := c.Client.List(ctx, &taskList, client.InNamespace(agent.Namespace),
			client.MatchingLabels{"codemowers.cloud/agent": agent.Name}); err != nil {
			return fmt.Errorf("list Agent Tasks: %w", err)
		}
		matched := 0
		for j := range taskList.Items {
			task := &taskList.Items[j]
			if !ownedByAgent(task, agent.Name, agent.UID) || !refMatches(task, ev) {
				continue
			}
			matched++
			if taskHasDelivery(task, d.MessageId) {
				continue
			}
			if err := c.wakeTask(ctx, task, d.RoutingKey, d.MessageId); err != nil {
				return fmt.Errorf("wake Task %s/%s: %w", task.Namespace, task.Name, err)
			}
		}
		if matched == 0 && (ev.source == "github" || ev.source == "gitea") {
			if _, err := c.createTaskForAssignedIssue(ctx, agent.Namespace, agent.Name, ev, d.MessageId); err != nil {
				return err
			}
		}
	}
	return nil
}

func ownedByAgent(task *agentsv1alpha1.Task, name string, uid types.UID) bool {
	for _, owner := range task.OwnerReferences {
		if owner.APIVersion == agentsv1alpha1.GroupVersion.String() && owner.Kind == "Agent" && owner.Name == name && owner.UID == uid {
			return true
		}
	}
	return false
}

func serverHost(raw string) string {
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		return strings.ToLower(raw)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

// refMatches returns true if any of the Task's externalReferences match the event.
func refMatches(task *agentsv1alpha1.Task, ev parsedEvent) bool {
	for _, ref := range task.Spec.ExternalReferences {
		if ref.Source != ev.source {
			continue
		}
		refHost := serverHost(ref.ServerURL)
		eventHost := strings.ToLower(ev.serverURL)
		if ev.source == "github" && eventHost == "" {
			eventHost = "github.com"
		}
		if refHost != eventHost && !(ev.source == "github" && refHost == "" && eventHost == "github.com") {
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

// wakeTask annotates the Task (triggering TaskReconciler to start/resume a worker)
// and publishes a follow-up prompt to the per-task queue so the worker receives it
// via AMQP push without polling.
func (c *EventConsumer) wakeTask(ctx context.Context, task *agentsv1alpha1.Task, routingKey, messageID string) error {
	now := time.Now().UnixMilli()
	if c.AMQP != nil {
		queue := taskQueue(task.Namespace, task.Name)
		msg := rabbitmq.PromptMessage{
			Type:       "event",
			Body:       fmt.Sprintf("New event on %s — read the latest updates and decide what to do next.", routingKey),
			RoutingKey: routingKey,
			MessageID:  messageID,
			ReceivedAt: now,
		}
		if err := c.AMQP.Publish(ctx, queue, msg); err != nil {
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
	// Signal the reconciler to spawn a new worker if the previous one has exited.
	task.Annotations["codemowers.cloud/wake-pending"] = "true"
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
