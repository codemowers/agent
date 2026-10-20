package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	amqp "github.com/rabbitmq/amqp091-go"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type normalizedEvent struct {
	Version      int      `json:"version"`
	Namespace    string   `json:"namespace"`
	Agent        string   `json:"agent"`
	Receiver     string   `json:"receiver"`
	ReceiverUID  string   `json:"receiverUID"`
	Protocol     string   `json:"protocol"`
	Source       string   `json:"source"`
	DeliveryID   string   `json:"deliveryID"`
	Type         string   `json:"type"`
	Action       string   `json:"action"`
	Actor        string   `json:"actor"`
	Text         string   `json:"text"`
	URL          string   `json:"url,omitempty"`
	Key          []string `json:"key"`
	FallbackTask string   `json:"fallbackTask"`
	CoalesceFor  string   `json:"coalesceFor,omitempty"`
}

func (c *EventConsumer) handleNormalized(ctx context.Context, d amqp.Delivery) error {
	// RabbitMQ rejects a user_id that differs from the publisher's login, so
	// only the webhook receiver's ServiceAccount can publish these events.
	if c.WebhookPublisher == "" || d.UserId != c.WebhookPublisher {
		return nil
	}
	namespace, _ := d.Headers["x-namespace"].(string)
	agentName, _ := d.Headers["x-agent"].(string)
	receiverUID, _ := d.Headers["x-hook-id"].(string)
	source, _ := d.Headers["x-source"].(string)
	eventType, _ := d.Headers["x-event"].(string)
	var event normalizedEvent
	if json.Unmarshal(d.Body, &event) != nil || event.Version != 1 || event.Namespace != namespace || event.Agent != agentName || event.ReceiverUID != receiverUID || event.Source != source || event.Type != eventType || d.MessageId != normalizedMessageID(event) || d.RoutingKey != "receiver."+namespace+"."+event.Receiver {
		return nil
	}
	if len(event.Key) > 16 || len(event.Text) > 4096 || len(event.URL) > 2048 || event.DeliveryID == "" ||
		(len(event.Key) > 0 && event.Key[0] != "internal" && event.Key[0] != "external") {
		return nil
	}
	for _, part := range event.Key {
		if part == "" || len(part) > 256 {
			return nil
		}
	}
	var agent agentsv1alpha1.Agent
	if err := c.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: agentName}, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get event Agent: %w", err)
	}
	if selfForgeEvent(&agent, event) {
		return nil
	}
	var tasks agentsv1alpha1.TaskList
	if err := c.APIReader.List(ctx, &tasks, client.InNamespace(namespace), client.MatchingLabels{"codemowers.cloud/agent": agentName}); err != nil {
		return err
	}
	var matched *agentsv1alpha1.Task
	for i := range tasks.Items {
		task := &tasks.Items[i]
		if ownedByAgent(task, agentName, agent.UID) && routingKeyMatches(task, event.Key) {
			if matched != nil {
				return fmt.Errorf("multiple Tasks claim event key")
			}
			matched = task
		}
	}
	if matched == nil {
		created, err := c.createTaskByPolicy(ctx, event, d.MessageId)
		if err != nil {
			return err
		}
		if created {
			return nil
		}
	}
	if matched == nil && event.FallbackTask != "" {
		for i := range tasks.Items {
			task := &tasks.Items[i]
			if task.Name == event.FallbackTask && ownedByAgent(task, agentName, agent.UID) {
				matched = task
				break
			}
		}
	}
	if matched == nil || taskHasDelivery(matched, d.MessageId) {
		return nil
	}
	message := event.Text
	if message == "" {
		message = "New " + event.Source + " event: " + event.Type
	}
	var coalesceFor time.Duration
	if event.CoalesceFor != "" {
		var err error
		coalesceFor, err = time.ParseDuration(event.CoalesceFor)
		if err != nil || coalesceFor < time.Second || coalesceFor > time.Hour {
			return nil
		}
	}
	return c.wakeTaskWithContent(ctx, matched, strings.Join(event.Key, " / "), d.MessageId, event.Source, event.Actor, message, event.URL, coalesceFor)
}

// The receiver extracts the authenticated sender; the operator compares that
// account with the Agent's probed forge identity before routing an event.
func selfForgeEvent(agent *agentsv1alpha1.Agent, event normalizedEvent) bool {
	if len(event.Key) < 4 || event.Key[0] != "external" || event.Key[1] != "forge" ||
		(event.Source != "github" && event.Source != "gitea") || event.Key[2] != event.Source ||
		event.Actor == "" || agent.Status.ForgesObservedGeneration != agent.Generation {
		return false
	}
	for _, forge := range agent.Status.Forges {
		if forge.Status == "available" && forge.Type == event.Source && forge.ServerURL == event.Key[3] &&
			forge.Login != "" && strings.EqualFold(forge.Login, event.Actor) {
			return true
		}
	}
	return false
}
func normalizedMessageID(event normalizedEvent) string {
	key, _ := json.Marshal([]any{event.ReceiverUID, event.DeliveryID, event.Source, event.Type, event.Key})
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])
}

func (c *EventConsumer) createTaskByPolicy(ctx context.Context, event normalizedEvent, messageID string) (bool, error) {
	var policies agentsv1alpha1.TaskCreationPolicyList
	if err := c.APIReader.List(ctx, &policies, client.InNamespace(event.Namespace)); err != nil {
		return false, err
	}
	var policy *agentsv1alpha1.TaskCreationPolicy
	for i := range policies.Items {
		candidate := &policies.Items[i]
		if candidate.Spec.AgentRef != event.Agent || candidate.Spec.ReceiverRef != event.Receiver || !hasKeyPrefix(event.Key, candidate.Spec.KeyPrefix) {
			continue
		}
		if policy != nil {
			return false, fmt.Errorf("ambiguous TaskCreationPolicy for receiver %s/%s", event.Namespace, event.Receiver)
		}
		policy = candidate
	}
	if policy == nil {
		return false, nil
	}
	switch policy.Spec.Check {
	case "currentForgeAssignment":
		ev, ok := forgeEventFromKey(event.Key)
		if !ok {
			return false, nil
		}
		return c.createTaskForAssignedIssue(ctx, event.Namespace, event.Agent, ev, messageID, event.CoalesceFor)
	default:
		return false, fmt.Errorf("unsupported TaskCreationPolicy check %q", policy.Spec.Check)
	}
}
func hasKeyPrefix(key, prefix []string) bool {
	if len(prefix) == 0 || len(key) < len(prefix) {
		return false
	}
	for i := range prefix {
		if key[i] != prefix[i] {
			return false
		}
	}
	return true
}

func forgeEventFromKey(key []string) (parsedEvent, bool) {
	if len(key) != 8 || (key[0] != "external" && key[0] != "internal") || key[1] != "forge" || (key[2] != "github" && key[2] != "gitea") || (key[6] != "issue" && key[6] != "pull_request") {
		return parsedEvent{}, false
	}
	server, err := url.Parse(key[3])
	if err != nil || server.Scheme != "https" || server.Hostname() == "" || server.User != nil {
		return parsedEvent{}, false
	}
	if key[4] == "" || key[5] == "" || key[7] == "" {
		return parsedEvent{}, false
	}
	return parsedEvent{source: key[2], serverURL: strings.ToLower(server.Hostname()), owner: key[4], repo: key[5], kind: key[6], id: key[7], routeKey: append([]string(nil), key...)}, true
}
