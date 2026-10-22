package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	"github.com/codemowers/agent/operator/internal/rabbitmq"
	amqp "github.com/rabbitmq/amqp091-go"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type coalesceBroker struct {
	messages []rabbitmq.PromptMessage
	queues   []string
	deleted  []string
}

func (*coalesceBroker) DeclareQueue(string) error      { return nil }
func (*coalesceBroker) QueueDepth(string) (int, error) { return 0, nil }
func (b *coalesceBroker) DeleteQueue(queue string) error {
	b.deleted = append(b.deleted, queue)
	return nil
}
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
	agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "ops", Namespace: "team"}, Spec: agentsv1alpha1.AgentSpec{CoalesceSeconds: int32Ptr(120)}}
	task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "alerts", Namespace: "team", UID: "task-uid"}, Spec: agentsv1alpha1.TaskSpec{AgentRef: agent.Name}}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agentsv1alpha1.Task{}).WithObjects(agent, task).Build()
	broker := &coalesceBroker{}
	consumer := &EventConsumer{Client: k8s, AMQP: broker}
	ctx := context.Background()
	if err := consumer.recordArrival(ctx, task, agent, taskArrival{Key: "alerts", Receipt: "first", Source: "alertmanager", Actor: "Alertmanager", Text: "CPU high", URL: "https://alerts.example.test/1", Wake: true}); err != nil {
		t.Fatal(err)
	}
	if len(broker.messages) != 1 || broker.messages[0].Body != "CPU high" || broker.queues[0] != "system:serviceaccount:team:agent-worker-ops:alerts" {
		t.Fatalf("first event did not wake immediately: %+v", broker.messages)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(task), task); err != nil {
		t.Fatal(err)
	}
	if err := consumer.recordArrival(ctx, task, agent, taskArrival{Key: "alerts", Receipt: "second", Source: "alertmanager", Actor: "Alertmanager", Text: "Memory high", URL: "https://alerts.example.test/2", Wake: true}); err != nil {
		t.Fatal(err)
	}
	if len(broker.messages) != 1 {
		t.Fatalf("second event woke inside window: %+v", broker.messages)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(task), task); err != nil {
		t.Fatal(err)
	}
	if !task.Status.Events.Windows["alerts"].Trailing || len(task.Status.Conversation) != 2 || task.Status.Conversation[1].URL != "https://alerts.example.test/2" {
		t.Fatalf("trailing event was not retained: %+v", task)
	}
	patch := client.MergeFrom(task.DeepCopy())
	window := task.Status.Events.Windows["alerts"]
	window.Until = metav1.NewTime(time.Unix(1, 0))
	task.Status.Events.Windows["alerts"] = window
	if err := k8s.Status().Patch(ctx, task, patch); err != nil {
		t.Fatal(err)
	}
	reconciler := &TaskReconciler{Client: k8s, AMQP: broker}
	if delay, err := reconciler.flushCoalescedWake(ctx, task); err != nil || delay <= time.Minute {
		t.Fatalf("trailing wake did not open the next window: delay=%s err=%v", delay, err)
	}
	if len(broker.messages) != 2 || broker.messages[1].Type != "event" {
		t.Fatalf("missing trailing wake: %+v", broker.messages)
	}
	// A busy key wakes at most once per window.
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(task), task); err != nil {
		t.Fatal(err)
	}
	if err := consumer.recordArrival(ctx, task, agent, taskArrival{Key: "alerts", Receipt: "third", Source: "alertmanager", Actor: "Alertmanager", Text: "Disk full", Wake: true}); err != nil {
		t.Fatal(err)
	}
	if len(broker.messages) != 2 {
		t.Fatalf("event after a trailing wake woke inside the next window: %+v", broker.messages)
	}
	// A quiet window closes, so the next event wakes at once.
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(task), task); err != nil {
		t.Fatal(err)
	}
	patch = client.MergeFrom(task.DeepCopy())
	task.Status.Events.Windows["alerts"] = agentsv1alpha1.EventWindow{Until: metav1.NewTime(time.Unix(1, 0))}
	if err := k8s.Status().Patch(ctx, task, patch); err != nil {
		t.Fatal(err)
	}
	if delay, err := reconciler.flushCoalescedWake(ctx, task); err != nil || delay != 0 || len(task.Status.Events.Windows) != 0 {
		t.Fatalf("quiet window stayed open: delay=%s err=%v windows=%+v", delay, err, task.Status.Events.Windows)
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
		objects = append(objects, &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "issue-8", Namespace: item.namespace, Labels: map[string]string{"codemowers.cloud/agent": item.agent}, OwnerReferences: []metav1.OwnerReference{{APIVersion: agentsv1alpha1.GroupVersion.String(), Kind: "Agent", Name: item.agent, UID: uid}}}, Spec: agentsv1alpha1.TaskSpec{RoutingKeys: []string{"https://github.com/codemowers/agent/issues/8"}}})
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agentsv1alpha1.Task{}).WithObjects(objects...).Build()
	consumer := &EventConsumer{Client: k8s, APIReader: k8s, WebhookPublisher: testWebhookPublisher}
	event := normalizedEvent{Version: 2, Namespace: "team-a", Agent: "developer", Binding: "github-public", BindingUID: "binding-uid", Protocol: "github", DeliveryID: "delivery-1", Type: "issue_comment.created", Actor: "lauri", Text: "New comment", URL: "https://github.com/codemowers/agent/issues/8#issuecomment-1", Key: "https://github.com/codemowers/agent/issues/8"}
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
	if len(untouched.Status.Conversation) != 0 || untouched.Status.Events.WakePending {
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
	if untouched.Status.Events.WakePending {
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
		if task.Status.Events.WakePending != item.want {
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
	if other.Status.Events.WakePending {
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
	route := "webhook." + event.Namespace + "." + event.Binding
	return amqp.Delivery{RoutingKey: route, MessageId: normalizedMessageID(event), UserId: testWebhookPublisher, Body: body,
		Headers: amqp.Table{"x-namespace": event.Namespace, "x-agent": event.Agent, "x-binding-uid": event.BindingUID, "x-source": event.Protocol, "x-event": event.Type, "x-key": event.Key}}
}

func TestSelfEventRequiresMatchingCurrentForgeIdentity(t *testing.T) {
	agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Generation: 3},
		Status: agentsv1alpha1.AgentStatus{ForgesObservedGeneration: 3, Forges: []agentsv1alpha1.ForgeCapability{{
			Type: "gitea", ServerURL: "https://git.example.test", Status: "available", Login: "bot",
		}}}}
	event := intakeEvent{Source: "gitea", Actor: "Bot", Key: "https://git.example.test/team/repo/issues/1"}
	if !selfEvent(agent, event) {
		t.Fatal("current Gitea bot event not suppressed")
	}
	for _, changed := range []intakeEvent{
		{Source: "gitea", Actor: "human", Key: event.Key},
		{Source: "gitea", Actor: "bot", Key: "https://other.example.test/team/repo/issues/1"},
		{Source: "slack-events", Actor: "bot", Key: "https://slack.com/archives/C1/p1"},
		{Source: "gitea", Actor: "bot"},
	} {
		if selfEvent(agent, changed) {
			t.Fatalf("unrelated event suppressed: %+v", changed)
		}
	}
	agent.Status.ForgesObservedGeneration = 2
	if selfEvent(agent, event) {
		t.Fatal("stale forge identity suppressed event")
	}
}

func TestEnvelopeKeyHeaderMustMatchBody(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	consumer := &EventConsumer{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), WebhookPublisher: testWebhookPublisher}
	event := normalizedEvent{Version: 2, Namespace: "team", Agent: "developer", Binding: "b", BindingUID: "uid", Protocol: "github", DeliveryID: "d", Type: "issues.opened", Key: "https://github.com/team/repo/issues/1"}
	delivery := webhookDelivery(t, event)
	delivery.Headers["x-key"] = "https://github.com/team/repo/issues/2"
	// A nil APIReader would panic if the delivery passed envelope checks.
	if err := consumer.handle(context.Background(), delivery); err != nil {
		t.Fatal(err)
	}
	event.Key = "HTTPS://GitHub.com/team/repo/issues/1"
	if err := consumer.handle(context.Background(), webhookDelivery(t, event)); err != nil {
		t.Fatal(err)
	}
}

func TestTaskDeliveryReceiptSurvivesInterleavedEvents(t *testing.T) {
	task := &agentsv1alpha1.Task{Status: agentsv1alpha1.TaskStatus{Events: agentsv1alpha1.TaskEvents{InitialDeliveryID: "delivery-0", Receipts: []string{"delivery-1", "delivery-2", "delivery-3"}}}}
	for _, id := range []string{"delivery-0", "delivery-1", "delivery-2", "delivery-3"} {
		if !taskHasDelivery(task, id) {
			t.Fatalf("redelivery %s was not recognized", id)
		}
	}
	if taskHasDelivery(task, "delivery-4") {
		t.Fatal("new delivery mistaken for duplicate")
	}
}

func TestRetryEventBoundsFailuresButWaitsOutOutages(t *testing.T) {
	noSleep := func(context.Context, time.Duration) bool { return true }
	calls := 0
	handled, err := retryEvent(context.Background(), func(context.Context) error {
		calls++
		return errors.New("invalid binding")
	}, noSleep)
	if handled || err == nil || calls != maxEventFailures {
		t.Fatalf("permanent failure: handled=%v err=%v calls=%d", handled, err, calls)
	}
	calls = 0
	unavailable := apierrors.NewServiceUnavailable("down")
	handled, err = retryEvent(context.Background(), func(context.Context) error {
		if calls++; calls < maxEventFailures*3 {
			return fmt.Errorf("get Task: %w", unavailable)
		}
		return nil
	}, noSleep)
	if !handled || err != nil {
		t.Fatalf("outage dropped the event: handled=%v err=%v calls=%d", handled, err, calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if handled, _ := retryEvent(ctx, func(context.Context) error { return unavailable },
		func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil }); handled {
		t.Fatal("cancelled retry reported success")
	}
}

func TestTaskQueueFollowsTaskDeletion(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "team"}}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agentsv1alpha1.Task{}).WithObjects(task).Build()
	broker := &coalesceBroker{}
	r := &TaskReconciler{Client: k8s, AMQP: broker}
	ctx := context.Background()
	if err := r.trackTaskQueue(ctx, task, "old"); err != nil {
		t.Fatal(err)
	}
	if err := r.trackTaskQueue(ctx, task, "new"); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Delete(ctx, task); err != nil {
		t.Fatal(err)
	}
	var deleting agentsv1alpha1.Task
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(task), &deleting); err != nil {
		t.Fatalf("finalizer did not hold the Task: %v", err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}); err != nil {
		t.Fatal(err)
	}
	if len(broker.deleted) != 2 || broker.deleted[0] != "old" || broker.deleted[1] != "new" {
		t.Fatalf("queues not deleted: %v", broker.deleted)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(task), &deleting); !apierrors.IsNotFound(err) {
		t.Fatalf("Task kept after its queue was deleted: %v", err)
	}
}

func TestOtherBindingNotificationAndForgeCloseDoNotPrompt(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	const key = "https://github.com/team/repo/issues/1"
	agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "developer", Namespace: "team", UID: "agent-uid"}}
	task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "issue-1", Namespace: "team", UID: "task-uid",
		Labels:          map[string]string{"codemowers.cloud/agent": agent.Name},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: agentsv1alpha1.GroupVersion.String(), Kind: "Agent", Name: agent.Name, UID: agent.UID}}},
		Spec: agentsv1alpha1.TaskSpec{AgentRef: agent.Name, RoutingKeys: []string{key}}}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agentsv1alpha1.Task{}).WithObjects(agent, task).Build()
	broker := &coalesceBroker{}
	consumer := &EventConsumer{Client: k8s, APIReader: k8s, AMQP: broker}
	ctx := context.Background()
	for i, ev := range []intakeEvent{
		{Source: "github", Binding: "webhook", Type: "issue_comment.created", Actor: "lauri", Text: "Disable it", Receipt: "comment"},
		{Source: "imap", Binding: "mail", Type: "github_notification", Actor: "lauri", Text: "GitHub notification", Receipt: "comment-mail"},
		{Source: "github", Binding: "webhook", Type: "issues.closed", Actor: "lauri", Text: "Issue title", Receipt: "closed"},
		{Source: "github", Binding: "webhook", Type: "issue_comment.created", Actor: "lauri", Text: "Thanks", Receipt: "late"},
	} {
		ev.Namespace, ev.Key = "team", key
		if _, err := consumer.routeEvent(ctx, agent, ev); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
	}
	if len(broker.messages) != 1 || broker.messages[0].Body != "Disable it" {
		t.Fatalf("only the webhook comment should prompt: %+v", broker.messages)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(task), task); err != nil {
		t.Fatal(err)
	}
	if !task.Status.Completed() || task.Status.Completion.Actor != "lauri" || len(task.Status.Conversation) != 5 {
		t.Fatalf("close was not recorded as completion: %+v", task.Status)
	}
}
