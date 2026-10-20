package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	"github.com/codemowers/agent/operator/internal/rabbitmq"
	amqp "github.com/rabbitmq/amqp091-go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type coalesceBroker struct {
	messages []rabbitmq.PromptMessage
	queues   []string
}

func (*coalesceBroker) DeclareQueue(string) error      { return nil }
func (*coalesceBroker) QueueDepth(string) (int, error) { return 0, nil }
func (*coalesceBroker) DeleteQueue(string) error       { return nil }
func (b *coalesceBroker) Publish(_ context.Context, queue string, msg rabbitmq.PromptMessage) error {
	b.messages = append(b.messages, msg)
	b.queues = append(b.queues, queue)
	return nil
}

func TestCoalesceFirstWakeAndTrailingWake(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "ops", Namespace: "team"}}
	task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "alerts", Namespace: "team", UID: "task-uid"}, Spec: agentsv1alpha1.TaskSpec{AgentRef: agent.Name}}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agentsv1alpha1.Task{}).WithObjects(agent, task).Build()
	broker := &coalesceBroker{}
	consumer := &EventConsumer{Client: k8s, AMQP: broker}
	ctx := context.Background()
	if err := consumer.wakeTaskWithContent(ctx, task, "alerts", "first", "alertmanager", "Alertmanager", "CPU high", "https://alerts.example.test/1", 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	if len(broker.messages) != 1 || broker.messages[0].Body != "CPU high" || broker.queues[0] != "system:serviceaccount:team:agent-worker-ops:alerts" {
		t.Fatalf("first event did not wake immediately: %+v", broker.messages)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(task), task); err != nil {
		t.Fatal(err)
	}
	if err := consumer.wakeTaskWithContent(ctx, task, "alerts", "second", "alertmanager", "Alertmanager", "Memory high", "https://alerts.example.test/2", 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	if len(broker.messages) != 1 {
		t.Fatalf("second event woke inside window: %+v", broker.messages)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(task), task); err != nil {
		t.Fatal(err)
	}
	if task.Annotations["codemowers.cloud/coalesce-trailing"] != "true" || len(task.Status.Conversation) != 2 || task.Status.Conversation[1].URL != "https://alerts.example.test/2" {
		t.Fatalf("trailing event was not retained: %+v", task)
	}
	patch := client.MergeFrom(task.DeepCopy())
	task.Annotations["codemowers.cloud/coalesce-until"] = "1"
	if err := k8s.Patch(ctx, task, patch); err != nil {
		t.Fatal(err)
	}
	reconciler := &TaskReconciler{Client: k8s, AMQP: broker}
	if delay, err := reconciler.flushCoalescedWake(ctx, task); err != nil || delay != 0 {
		t.Fatalf("flush delay=%s err=%v", delay, err)
	}
	if len(broker.messages) != 2 || broker.messages[1].Type != "event" {
		t.Fatalf("missing trailing wake: %+v", broker.messages)
	}
}

func TestNormalizedEventRoutesOnlyOwnedTask(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objects := []client.Object{}
	for _, item := range []struct{ namespace, agent string }{{"team-a", "developer"}, {"team-b", "support"}} {
		uid := types.UID(item.agent + "-uid")
		objects = append(objects, &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: item.agent, Namespace: item.namespace, UID: uid, Generation: 1},
			Status: agentsv1alpha1.AgentStatus{ForgesObservedGeneration: 1, Forges: []agentsv1alpha1.ForgeCapability{{Type: "github", ServerURL: "https://github.com", Status: "available", Login: item.agent}}}})
		objects = append(objects, &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "issue-8", Namespace: item.namespace, Labels: map[string]string{"codemowers.cloud/agent": item.agent}, OwnerReferences: []metav1.OwnerReference{{APIVersion: agentsv1alpha1.GroupVersion.String(), Kind: "Agent", Name: item.agent, UID: uid}}}, Spec: agentsv1alpha1.TaskSpec{RoutingKeys: [][]string{{"external", "forge", "github", "https://github.com", "codemowers", "agent", "issue", "8"}}}})
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agentsv1alpha1.Task{}).WithObjects(objects...).Build()
	consumer := &EventConsumer{Client: k8s, APIReader: k8s, WebhookPublisher: testWebhookPublisher}
	event := normalizedEvent{Version: 1, Namespace: "team-a", Agent: "developer", Receiver: "github-public", ReceiverUID: "receiver-uid", Protocol: "github", Source: "github", DeliveryID: "delivery-1", Type: "issues", Actor: "lauri", Text: "New comment", URL: "https://github.com/codemowers/agent/issues/8#issuecomment-1", Key: []string{"external", "forge", "github", "https://github.com", "codemowers", "agent", "issue", "8"}}
	self := event
	self.DeliveryID = "delivery-self"
	self.Actor = "developer"
	if err := consumer.handle(context.Background(), webhookDelivery(t, self)); err != nil {
		t.Fatal(err)
	}
	var untouched agentsv1alpha1.Task
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "issue-8"}, &untouched); err != nil {
		t.Fatal(err)
	}
	if len(untouched.Status.Conversation) != 0 || untouched.Annotations["codemowers.cloud/wake-pending"] == "true" {
		t.Fatal("self-authored forge event reached its Task")
	}
	forged := webhookDelivery(t, event)
	forged.UserId = "system:serviceaccount:team-a:agent-mailbox"
	if err := consumer.handle(context.Background(), forged); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "issue-8"}, &untouched); err != nil {
		t.Fatal(err)
	}
	if untouched.Annotations["codemowers.cloud/wake-pending"] == "true" {
		t.Fatal("event from another broker user reached its Task")
	}
	delivery := webhookDelivery(t, event)
	if err := consumer.handle(context.Background(), delivery); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		namespace string
		want      bool
	}{{"team-a", true}, {"team-b", false}} {
		var task agentsv1alpha1.Task
		if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: item.namespace, Name: "issue-8"}, &task); err != nil {
			t.Fatal(err)
		}
		if (task.Annotations["codemowers.cloud/wake-pending"] == "true") != item.want {
			t.Fatalf("%s wake mismatch", item.namespace)
		}
		if item.want && (len(task.Status.Conversation) != 1 || task.Status.Conversation[0].URL != event.URL) {
			t.Fatalf("%s source URL missing from conversation: %+v", item.namespace, task.Status.Conversation)
		}
	}
	// Changing an envelope header cannot redirect the delivery.
	delivery.Headers["x-namespace"] = "team-b"
	if err := consumer.handle(context.Background(), delivery); err != nil {
		t.Fatal(err)
	}
	var other agentsv1alpha1.Task
	_ = k8s.Get(context.Background(), client.ObjectKey{Namespace: "team-b", Name: "issue-8"}, &other)
	if other.Annotations["codemowers.cloud/wake-pending"] == "true" {
		t.Fatal("tampered envelope crossed namespace")
	}
}

const testWebhookPublisher = "system:serviceaccount:agent-system:agent-operator-webhook"

func webhookDelivery(t *testing.T, event normalizedEvent) amqp.Delivery {
	t.Helper()
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	route := "receiver." + event.Namespace + "." + event.Receiver
	return amqp.Delivery{RoutingKey: route, MessageId: normalizedMessageID(event), UserId: testWebhookPublisher, Body: body,
		Headers: amqp.Table{"x-namespace": event.Namespace, "x-agent": event.Agent, "x-hook-id": event.ReceiverUID, "x-source": event.Source, "x-event": event.Type}}
}

func TestSelfForgeEventRequiresMatchingCurrentProviderIdentity(t *testing.T) {
	agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Generation: 3},
		Status: agentsv1alpha1.AgentStatus{ForgesObservedGeneration: 3, Forges: []agentsv1alpha1.ForgeCapability{{
			Type: "gitea", ServerURL: "https://git.example.test", Status: "available", Login: "bot",
		}}}}
	event := normalizedEvent{Source: "gitea", Actor: "Bot", Key: []string{
		"external", "forge", "gitea", "https://git.example.test", "team", "repo", "issue", "1",
	}}
	if !selfForgeEvent(agent, event) {
		t.Fatal("current Gitea bot event not suppressed")
	}
	for _, changed := range []normalizedEvent{
		{Source: "gitea", Actor: "human", Key: event.Key},
		{Source: "gitea", Actor: "bot", Key: []string{"external", "forge", "gitea", "https://other.example.test"}},
		{Source: "slack", Actor: "bot", Key: []string{"external", "slack", "workspace", "channel", "thread"}},
	} {
		if selfForgeEvent(agent, changed) {
			t.Fatalf("unrelated event suppressed: %+v", changed)
		}
	}
	agent.Status.ForgesObservedGeneration = 2
	if selfForgeEvent(agent, event) {
		t.Fatal("stale forge identity suppressed event")
	}
}

func TestForgeTaskCreationPolicyKey(t *testing.T) {
	key := []string{"external", "forge", "gitea", "https://git.example.test", "team", "repo", "issue", "42"}
	event, ok := forgeEventFromKey(key)
	if !ok || !routingKeyMatches(&agentsv1alpha1.Task{Spec: agentsv1alpha1.TaskSpec{RoutingKeys: [][]string{key}}}, eventTaskKey(event)) {
		t.Fatal("forge key lost its canonical identity")
	}
	if !hasKeyPrefix(key, []string{"external", "forge", "gitea", "https://git.example.test"}) || hasKeyPrefix(key, []string{"external", "forge", "github"}) {
		t.Fatal("policy key prefix mismatch")
	}
	if _, ok := forgeEventFromKey([]string{"external", "forge", "gitea", "http://evil", "team", "repo", "issue", "42"}); ok {
		t.Fatal("insecure forge URL accepted")
	}
}

func TestTaskDeliveryReceiptSurvivesInterleavedEvents(t *testing.T) {
	task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"codemowers.cloud/initial-delivery-id": "delivery-0", "codemowers.cloud/event-message-id": "delivery-3", "codemowers.cloud/delivery-receipts": `["delivery-1","delivery-2","delivery-3"]`}}}
	for _, id := range []string{"delivery-0", "delivery-1", "delivery-2", "delivery-3"} {
		if !taskHasDelivery(task, id) {
			t.Fatalf("redelivery %s was not recognized", id)
		}
	}
	if taskHasDelivery(task, "delivery-4") {
		t.Fatal("new delivery mistaken for duplicate")
	}
}
