package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	amqp "github.com/rabbitmq/amqp091-go"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type normalizedEvent struct {
	Version      int    `json:"version"`
	Namespace    string `json:"namespace"`
	Agent        string `json:"agent"`
	Binding      string `json:"binding"`
	BindingUID   string `json:"bindingUID"`
	Protocol     string `json:"protocol"`
	DeliveryID   string `json:"deliveryID"`
	Type         string `json:"type"`
	Actor        string `json:"actor"`
	Text         string `json:"text"`
	URL          string `json:"url,omitempty"`
	Key          string `json:"key,omitempty"`
	FallbackTask string `json:"fallbackTask"`
	CoalesceFor  string `json:"coalesceFor,omitempty"`
}

// intakeEvent is an authenticated arrival from any transport.
type intakeEvent struct {
	Namespace    string
	Source       string // webhook protocol name, or imap
	Type         string
	Actor        string
	Text         string
	URL          string
	Key          string // canonical resource URI; empty routes to the fallback
	FallbackTask string
	Receipt      string
	Coalesce     time.Duration
}

func (c *EventConsumer) handleNormalized(ctx context.Context, d amqp.Delivery) error {
	// RabbitMQ rejects a user_id that differs from the publisher's login, so
	// only the webhook receiver's ServiceAccount can publish these events.
	if c.WebhookPublisher == "" || d.UserId != c.WebhookPublisher {
		return nil
	}
	namespace, _ := d.Headers["x-namespace"].(string)
	agentName, _ := d.Headers["x-agent"].(string)
	bindingUID, _ := d.Headers["x-binding-uid"].(string)
	source, _ := d.Headers["x-source"].(string)
	eventType, _ := d.Headers["x-event"].(string)
	key, _ := d.Headers["x-key"].(string)
	var event normalizedEvent
	if json.Unmarshal(d.Body, &event) != nil || event.Version != 2 || event.Namespace != namespace || event.Agent != agentName || event.BindingUID != bindingUID || event.Protocol != source || event.Type != eventType || event.Key != key || d.MessageId != normalizedMessageID(event) || d.RoutingKey != "webhook."+namespace+"."+event.Binding {
		return nil
	}
	if len(event.Text) > 4096 || len(event.URL) > 2048 || event.DeliveryID == "" {
		return nil
	}
	if event.Key != "" {
		if canonical, err := agentsv1alpha1.CanonicalKey(event.Key); err != nil || canonical != event.Key {
			return nil
		}
	}
	var coalesce time.Duration
	if event.CoalesceFor != "" {
		var err error
		coalesce, err = time.ParseDuration(event.CoalesceFor)
		if err != nil || coalesce < time.Second || coalesce > time.Hour {
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
	_, err := c.routeEvent(ctx, &agent, intakeEvent{Namespace: namespace, Source: event.Protocol, Type: event.Type,
		Actor: event.Actor, Text: event.Text, URL: event.URL, Key: event.Key, FallbackTask: event.FallbackTask,
		Receipt: d.MessageId, Coalesce: coalesce})
	return err
}

// routeEvent wakes the Task that claims the event key, creates one through a
// TaskPolicy, or wakes the fallback Task. It returns the Task woken by an
// exact key match so transports can record their source reference.
func (c *EventConsumer) routeEvent(ctx context.Context, agent *agentsv1alpha1.Agent, ev intakeEvent) (*agentsv1alpha1.Task, error) {
	if selfEvent(agent, ev) {
		return nil, nil
	}
	var tasks agentsv1alpha1.TaskList
	if err := c.APIReader.List(ctx, &tasks, client.InNamespace(ev.Namespace), client.MatchingLabels{"codemowers.cloud/agent": agent.Name}); err != nil {
		return nil, err
	}
	var matched *agentsv1alpha1.Task
	ambiguous := false
	for i := range tasks.Items {
		task := &tasks.Items[i]
		if ev.Key != "" && ownedByAgent(task, agent.Name, agent.UID) && slices.Contains(task.Spec.RoutingKeys, ev.Key) {
			if matched != nil {
				ambiguous = true
			}
			matched = task
		}
	}
	if ambiguous {
		log.FromContext(ctx).Info("multiple Tasks claim event key, using fallback", "namespace", ev.Namespace, "agent", agent.Name)
		matched = nil
	}
	byKey := matched != nil
	if matched == nil && ev.Key != "" && !ambiguous {
		created, err := c.createTaskByPolicy(ctx, agent, ev)
		if err != nil || created {
			return nil, err
		}
	}
	if matched == nil && ev.FallbackTask != "" {
		for i := range tasks.Items {
			task := &tasks.Items[i]
			if task.Name == ev.FallbackTask && ownedByAgent(task, agent.Name, agent.UID) {
				matched = task
				break
			}
		}
	}
	if matched == nil || taskHasDelivery(matched, ev.Receipt) {
		return nil, nil
	}
	message := ev.Text
	if message == "" {
		message = "New " + ev.Source + " event: " + ev.Type
	}
	label := ev.Key
	if label == "" {
		label = ev.Source
	}
	if err := c.wakeTaskWithContent(ctx, matched, label, ev.Receipt, ev.Source, ev.Actor, message, ev.URL, ev.Coalesce); err != nil {
		return nil, err
	}
	if !byKey {
		return nil, nil
	}
	return matched, nil
}

// selfEvent reports an event authored by the Agent's own probed forge account
// on that forge's host, so a worker's comments do not wake its Task.
func selfEvent(agent *agentsv1alpha1.Agent, ev intakeEvent) bool {
	if ev.Key == "" || ev.Actor == "" || agent.Status.ForgesObservedGeneration != agent.Generation {
		return false
	}
	key, err := url.Parse(ev.Key)
	if err != nil || key.Host == "" {
		return false
	}
	for _, forge := range agent.Status.Forges {
		server, err := url.Parse(forge.ServerURL)
		if err == nil && forge.Status == "available" && forge.Login != "" &&
			strings.EqualFold(server.Host, key.Host) && strings.EqualFold(forge.Login, ev.Actor) {
			return true
		}
	}
	return false
}

func normalizedMessageID(event normalizedEvent) string {
	key, _ := json.Marshal([]any{event.BindingUID, event.DeliveryID, event.Type, event.Key})
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])
}
