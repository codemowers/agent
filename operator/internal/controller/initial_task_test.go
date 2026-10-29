package controller

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestTaskPolicyCreatesTaskOnlyForCurrentAssignee(t *testing.T) {
	assigned := true
	userCalls := 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status := http.StatusOK
		body := ""
		if r.Header.Get("Authorization") != "Bearer configured-token" {
			t.Error("forge credential missing")
			status = http.StatusUnauthorized
		}
		switch r.URL.Path {
		case "/api/v3/user":
			userCalls++
			body = `{"login":"agent-bot"}`
		case "/api/v3/repos/team/project/issues/8", "/api/v3/repos/team/project/issues/9":
			assignees := `[]`
			if assigned {
				assignees = `[{"login":"agent-bot"}]`
			}
			body = fmt.Sprintf(`{"state":"open","title":"Fix build","html_url":"https://forge.example.test/team/project/issues/8","assignees":%s}`, assignees)
		default:
			status = http.StatusNotFound
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "developer", Namespace: "team", UID: types.UID("agent-uid")},
		Spec: agentsv1alpha1.AgentSpec{CredentialSecretRef: agentsv1alpha1.CredentialSecretRef{Name: "developer-credentials"}, IdleSeconds: int32Ptr(120), CoalesceSeconds: int32Ptr(120), Resources: &corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}}, GitProviders: []agentsv1alpha1.GitProvider{{Type: "github", ServerURL: "https://forge.example.test"}}}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "developer-credentials", Namespace: "team"},
		Data: map[string][]byte{"GH_ENTERPRISE_TOKEN": []byte("configured-token")}}
	policies := []client.Object{
		&agentsv1alpha1.TaskPolicy{ObjectMeta: metav1.ObjectMeta{Name: "assigned", Namespace: "team"}, Spec: agentsv1alpha1.TaskPolicySpec{
			AgentRef: "developer", KeyPrefix: "https://forge.example.test/team", EventTypes: []string{"issues.assigned"}, Verify: "forgeAssignment"}},
		// A prefix boundary keeps this policy away from team/project.
		&agentsv1alpha1.TaskPolicy{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "team"}, Spec: agentsv1alpha1.TaskPolicySpec{
			AgentRef: "developer", KeyPrefix: "https://forge.example.test/team/proj", Verify: "none"}},
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agentsv1alpha1.Task{}).WithObjects(append(policies, agent, secret)...).Build()
	// The cached client has not seen the new Task yet; the status write must
	// still record the creating delivery and window.
	cached := interceptor.NewClient(k8s, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*agentsv1alpha1.Task); ok {
			return apierrors.NewNotFound(schema.GroupResource{Group: "codemowers.ai", Resource: "tasks"}, key.Name)
		}
		return c.Get(ctx, key, obj, opts...)
	}})
	consumer := &EventConsumer{Client: cached, APIReader: k8s, HTTPClient: &http.Client{Transport: transport}}
	event := intakeEvent{Namespace: "team", Source: "github", Type: "issues.assigned", Key: "https://forge.example.test/team/project/issues/8", Receipt: "delivery-1"}
	if created, err := consumer.createTaskByPolicy(context.Background(), agent, intakeEvent{Namespace: "team", Type: "issue_comment.created", Key: event.Key}); err != nil || created {
		t.Fatalf("event type outside the policy created a Task: %v %v", created, err)
	}
	created, err := consumer.createTaskByPolicy(context.Background(), agent, event)
	if err != nil || !created {
		t.Fatalf("assigned issue was not created: created=%v err=%v", created, err)
	}
	var tasks agentsv1alpha1.TaskList
	if err := k8s.List(context.Background(), &tasks, client.InNamespace("team")); err != nil {
		t.Fatal(err)
	}
	if len(tasks.Items) != 1 || tasks.Items[0].Spec.ExternalReferences[0].ID != "8" {
		t.Fatalf("wrong Task created: %+v", tasks.Items)
	}
	if _, ok := tasks.Items[0].Status.Events.Windows[event.Key]; !ok || tasks.Items[0].Status.Events.InitialDeliveryID != "delivery-1" {
		t.Fatal("created Task did not start its coalesce window")
	}
	if !slices.Equal(tasks.Items[0].Spec.RoutingKeys, []string{event.Key}) || tasks.Items[0].Name != agentsv1alpha1.TaskNameForKey(agent.UID, event.Key) {
		t.Fatalf("created Task lacks its forge routing key: %+v", tasks.Items[0].Spec.RoutingKeys)
	}
	if tasks.Items[0].Spec.AgentRef != agent.Name {
		t.Fatalf("webhook Task did not reference Agent: %q", tasks.Items[0].Spec.AgentRef)
	}
	if tasks.Items[0].Spec.IdleSeconds == nil || *tasks.Items[0].Spec.IdleSeconds != 120 {
		t.Fatalf("webhook Task did not copy Agent idle timeout: %+v", tasks.Items[0].Spec.IdleSeconds)
	}
	if tasks.Items[0].Spec.Resources == nil || tasks.Items[0].Spec.Resources.Limits.Memory().String() != "2Gi" {
		t.Fatalf("webhook Task did not copy Agent resources: %+v", tasks.Items[0].Spec.Resources)
	}
	assigned = false
	event.Key = "https://forge.example.test/team/project/issues/9"
	event.Receipt = "delivery-2"
	created, err = consumer.createTaskByPolicy(context.Background(), agent, event)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("unverified issue created a Task")
	}
	if err := k8s.List(context.Background(), &tasks, client.InNamespace("team")); err != nil {
		t.Fatal(err)
	}
	if len(tasks.Items) != 1 {
		t.Fatalf("unassigned issue changed Task count to %d", len(tasks.Items))
	}
	// A current forge snapshot replaces the per-event account lookup.
	agent.Generation = 1
	agent.Status = agentsv1alpha1.AgentStatus{Forges: []agentsv1alpha1.ForgeCapability{{
		Type: "github", ServerURL: "https://forge.example.test", Status: "available", Login: "agent-bot"}}}
	currentForges(agent)
	calls := userCalls
	if _, err := consumer.createTaskByPolicy(context.Background(), agent, event); err != nil || userCalls != calls {
		t.Fatalf("probed login not reused: err=%v calls=%d", err, userCalls-calls)
	}
}

func TestEquallySpecificTaskPoliciesCreateNothing(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "developer", Namespace: "team", UID: "agent-uid"}}
	var objects []client.Object
	for _, name := range []string{"a", "b"} {
		objects = append(objects, &agentsv1alpha1.TaskPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team"},
			Spec: agentsv1alpha1.TaskPolicySpec{AgentRef: "developer", KeyPrefix: "mid:", Verify: "none"}})
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agentsv1alpha1.Task{}).WithObjects(objects...).Build()
	consumer := &EventConsumer{Client: k8s, APIReader: k8s}
	if created, err := consumer.createTaskByPolicy(context.Background(), agent, intakeEvent{Namespace: "team", Key: "mid:root@example.test"}); err != nil || created {
		t.Fatalf("ambiguous policies created=%v err=%v", created, err)
	}
	if err := k8s.Delete(context.Background(), objects[1]); err != nil {
		t.Fatal(err)
	}
	if created, err := consumer.createTaskByPolicy(context.Background(), agent, intakeEvent{Namespace: "team", Source: "imap", Key: "mid:root@example.test"}); err != nil || created {
		t.Fatalf("unverified mail created=%v err=%v", created, err)
	}
	if created, err := consumer.createTaskByPolicy(context.Background(), agent, intakeEvent{Namespace: "team", Source: "webhook", Key: "mid:root@example.test", Text: "hello"}); err != nil || !created {
		t.Fatalf("verify none webhook created=%v err=%v", created, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
