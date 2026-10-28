package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestAgentMCPBindsWorkerToOwnTaskAndAgent(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	controller := true
	agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "developer", Namespace: "team", UID: types.UID("agent-uid")}}
	other := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "team", UID: types.UID("other-uid")}}
	owner := func(a *agentsv1alpha1.Agent) []metav1.OwnerReference {
		return []metav1.OwnerReference{{APIVersion: agentsv1alpha1.GroupVersion.String(), Kind: "Agent", Name: a.Name, UID: a.UID, Controller: &controller}}
	}
	task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "own", Namespace: "team", UID: types.UID("task-uid"), Labels: map[string]string{"codemowers.ai/agent": agent.Name}, OwnerReferences: owner(agent)},
		Spec: agentsv1alpha1.TaskSpec{AgentRef: agent.Name, Objective: "Work"}, Status: agentsv1alpha1.TaskStatus{WorkerPodName: "worker", WorkerPodUID: "pod-uid"}}
	peer := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "peer", Namespace: "team", UID: types.UID("peer-uid"), Labels: map[string]string{"codemowers.ai/agent": agent.Name}, OwnerReferences: owner(agent)}, Spec: agentsv1alpha1.TaskSpec{AgentRef: agent.Name, Objective: "Peer"}}
	foreign := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: "team", UID: types.UID("foreign-uid"), Labels: map[string]string{"codemowers.ai/agent": other.Name}, OwnerReferences: owner(other)}, Spec: agentsv1alpha1.TaskSpec{AgentRef: other.Name}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "team", UID: types.UID("pod-uid"), CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)), OwnerReferences: []metav1.OwnerReference{{APIVersion: agentsv1alpha1.GroupVersion.String(), Kind: "Task", Name: task.Name, UID: task.UID, Controller: &controller}}},
		Spec: corev1.PodSpec{ServiceAccountName: "worker-sa", Containers: []corev1.Container{{Name: "worker", Env: []corev1.EnvVar{
			{Name: "AGENT_MCP_ENDPOINTS", Value: `[{"name":"agent_operator"},{"name":"mailbox-shared"}]`}}}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	store := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(task, peer, foreign, pod).WithObjects(agent, other, task, peer, foreign, pod).Build()
	kube := kubefake.NewSimpleClientset()
	kube.PrependReactor("create", "tokenreviews", func(action ktesting.Action) (bool, runtime.Object, error) {
		request := action.(ktesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		if len(request.Spec.Audiences) != 1 || request.Spec.Audiences[0] != "agent-operator.harness" {
			t.Fatal("wrong audience")
		}
		return true, &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{Authenticated: request.Spec.Token == "valid",
			User: authenticationv1.UserInfo{Username: "system:serviceaccount:team:worker-sa", Extra: map[string]authenticationv1.ExtraValue{
				"authentication.kubernetes.io/pod-name": {"worker"}, "authentication.kubernetes.io/pod-uid": {"pod-uid"}}}}}, nil
	})
	h := &Handler{Client: store, APIReader: store, TokenReviews: kube.AuthenticationV1(), HarnessNamespace: "harness"}
	call := func(token, name, args string) *httptest.ResponseRecorder {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		h.agentMCP(response, req)
		return response
	}
	if response := call("invalid", "list_tasks", `{}`); response.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token accepted: %d", response.Code)
	}
	if response := call("valid", "get_task", `{"name":"foreign"}`); !strings.Contains(response.Body.String(), "outside Agent scope") {
		t.Fatalf("cross-Agent read allowed: %s", response.Body.String())
	}
	if response := call("valid", "list_tasks", `{}`); response.Code != 200 || strings.Contains(response.Body.String(), "foreign") || !strings.Contains(response.Body.String(), "peer") {
		t.Fatalf("Task list scope wrong: %s", response.Body.String())
	}
	if response := call("valid", "report_turn_start", `{}`); response.Code != 200 || strings.Contains(response.Body.String(), `"error"`) {
		t.Fatalf("start rejected: %s", response.Body.String())
	}
	if response := call("valid", "set_task_title", `{"title":"  Investigate webhook delivery  ","name":"foreign"}`); !strings.Contains(response.Body.String(), `"error"`) {
		t.Fatalf("title accepted an explicit foreign Task: %s", response.Body.String())
	}
	if response := call("valid", "set_task_title", `{"title":"First line\nSecond line"}`); !strings.Contains(response.Body.String(), `"error"`) {
		t.Fatalf("multiline title accepted: %s", response.Body.String())
	}
	if response := call("valid", "set_task_title", `{"title":"  Investigate webhook delivery  "}`); response.Code != 200 || strings.Contains(response.Body.String(), `"error"`) {
		t.Fatalf("own title rejected: %s", response.Body.String())
	}
	var titled agentsv1alpha1.Task
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(task), &titled); err != nil {
		t.Fatal(err)
	}
	if titled.Spec.Title != "Investigate webhook delivery" {
		t.Fatalf("own title not stored: %q", titled.Spec.Title)
	}
	if response := call("valid", "get_task", `{"name":"own"}`); !strings.Contains(response.Body.String(), "Investigate webhook delivery") {
		t.Fatalf("title missing from Task read: %s", response.Body.String())
	}
	if response := call("valid", "report_progress", `{"id":"progress-1","content":"Reading files","task":"foreign"}`); !strings.Contains(response.Body.String(), `"error"`) {
		t.Fatalf("progress accepted an explicit foreign Task: %s", response.Body.String())
	}
	if response := call("valid", "report_progress", `{"id":"progress-1","content":"Reading files"}`); response.Code != 200 || strings.Contains(response.Body.String(), `"error"`) {
		t.Fatalf("own progress rejected: %s", response.Body.String())
	}
	if response := call("valid", "report_turn_progress", `{"id":"turn-1","content":"","backend":"claude","model":"claude-opus-test"}`); response.Code != 200 || strings.Contains(response.Body.String(), `"error"`) {
		t.Fatalf("model-only turn progress rejected: %s", response.Body.String())
	}
	if response := call("valid", "report_turn_progress", `{"id":"turn-1","content":"x","backend":"gemini"}`); !strings.Contains(response.Body.String(), `"error"`) {
		t.Fatalf("unknown backend accepted: %s", response.Body.String())
	}
	if response := call("valid", "report_turn_progress", `{"id":"turn-1","content":"Reading the operator"}`); response.Code != 200 || strings.Contains(response.Body.String(), `"error"`) {
		t.Fatalf("turn progress rejected: %s", response.Body.String())
	}
	if response := call("valid", "report_turn_progress", `{"id":"turn-1","content":"","thinking":"Comparing approaches"}`); response.Code != 200 || strings.Contains(response.Body.String(), `"error"`) {
		t.Fatalf("thinking-only progress rejected: %s", response.Body.String())
	}
	var streamed agentsv1alpha1.Task
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(task), &streamed); err != nil {
		t.Fatal(err)
	}
	if (streamed.Status.Progress.Summary == "Reading the operator" || streamed.Status.Progress.Summary == "Comparing approaches") || len(streamed.Status.Conversation) != 1 {
		t.Fatalf("turn progress written to Task status: %+v", streamed.Status)
	}
	var foreignAfterProgress agentsv1alpha1.Task
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(foreign), &foreignAfterProgress); err != nil {
		t.Fatal(err)
	}
	if len(foreignAfterProgress.Status.Conversation) != 0 {
		t.Fatal("worker wrote progress to another Agent's Task")
	}
	if foreignAfterProgress.Spec.Title != "" {
		t.Fatal("worker titled another Agent's Task")
	}
	at := time.Now().UTC().Format(time.RFC3339Nano)
	arg, _ := json.Marshal(map[string]any{"id": "turn-1", "summary": "Done", "completedAt": at,
		"backend": "claude", "model": "claude-sonnet-test", "metrics": map[string]int64{
			"durationMs": 12000, "inputTokens": 42, "outputTokens": 0, "cacheReadTokens": 7}})
	if response := call("valid", "report_turn_result", string(arg)); response.Code != 200 || strings.Contains(response.Body.String(), `"error"`) {
		t.Fatalf("result rejected: %s", response.Body.String())
	}
	var current agentsv1alpha1.Task
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(task), &current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Activity != "idle" || len(current.Status.Conversation) != 2 || current.Status.Conversation[1].Content != "Done" {
		t.Fatalf("own result absent: %+v", current.Status)
	}
	turn := current.Status.Conversation[1]
	if turn.Backend != "claude" || turn.Model != "claude-sonnet-test" || turn.Metrics == nil ||
		turn.Metrics.DurationMs == nil || *turn.Metrics.DurationMs != 12000 ||
		turn.Metrics.InputTokens == nil || *turn.Metrics.InputTokens != 42 ||
		turn.Metrics.OutputTokens == nil || *turn.Metrics.OutputTokens != 0 ||
		turn.Metrics.CacheReadTokens == nil || *turn.Metrics.CacheReadTokens != 7 {
		t.Fatalf("turn metadata absent: %+v", turn)
	}
	if response := call("valid", "complete_task", `{"name":"foreign","reason":"Done"}`); !strings.Contains(response.Body.String(), "outside Agent scope") {
		t.Fatalf("cross-Agent completion allowed: %s", response.Body.String())
	}
	if response := call("valid", "complete_task", `{"name":"peer","reason":"Verified"}`); response.Code != 200 || strings.Contains(response.Body.String(), `"error"`) {
		t.Fatalf("same-Agent completion rejected: %s", response.Body.String())
	}
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(peer), &current); err != nil {
		t.Fatal(err)
	}
	if !current.Status.Completed() {
		t.Fatalf("peer was not completed: %+v", current)
	}
	if response := call("valid", "create_task", `{"objective":"New work","routingKey":"https://example.test/one"}`); response.Code != 200 || strings.Contains(response.Body.String(), `"error"`) {
		t.Fatalf("create rejected: %s", response.Body.String())
	}
	if response := call("valid", "create_task", `{"objective":"New work","routingKey":"https://example.test/one"}`); !strings.Contains(response.Body.String(), `\"existing\":true`) {
		t.Fatalf("duplicate key created: %s", response.Body.String())
	}
	if response := call("valid", "create_task", `{"objective":"Held work","routingKey":"https://example.test/held","suspend":true}`); response.Code != 200 || strings.Contains(response.Body.String(), `"error"`) {
		t.Fatalf("suspended create rejected: %s", response.Body.String())
	}
	var held agentsv1alpha1.Task
	if err := store.Get(context.Background(), client.ObjectKey{Namespace: task.Namespace, Name: agentsv1alpha1.TaskNameForKey(agent.UID, "https://example.test/held")}, &held); err != nil || !held.Spec.Suspend {
		t.Fatalf("created Task not suspended: %v %+v", err, held.Spec)
	}
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(task), &current); err != nil {
		t.Fatal(err)
	}
	current.Status.MCPProbe = &agentsv1alpha1.MCPProbeStatus{RequestID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PodUID: "pod-uid", State: "pending", UpdatedAt: metav1.Now()}
	if err := store.Status().Update(context.Background(), &current); err != nil {
		t.Fatal(err)
	}
	if response := call("valid", "report_mcp_probe", `{"requestId":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","endpoints":[{"name":"agent_operator","state":"available"}]}`); !strings.Contains(response.Body.String(), `"error"`) {
		t.Fatalf("stale probe was accepted: %s", response.Body.String())
	}
	if response := call("valid", "report_mcp_probe", `{"requestId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","endpoints":[{"name":"bad name","state":"available"}]}`); !strings.Contains(response.Body.String(), `"error"`) {
		t.Fatalf("unknown endpoint was accepted: %s", response.Body.String())
	}
	roundtrips := histogramCount(t, mcpProbeRoundtrip)
	if response := call("valid", "report_mcp_probe", `{"requestId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","endpoints":[{"name":"agent_operator","state":"available"},{"name":"mailbox-shared","state":"available"}]}`); response.Code != 200 || strings.Contains(response.Body.String(), `"error"`) {
		t.Fatalf("current probe was rejected: %s", response.Body.String())
	}
	if histogramCount(t, mcpProbeRoundtrip) != roundtrips+1 {
		t.Fatal("probe round trip was not observed")
	}
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(task), &current); err != nil || current.Status.MCPProbe == nil || current.Status.MCPProbe.State != "complete" {
		t.Fatalf("probe status was not saved: %v %+v", err, current.Status.MCPProbe)
	}
	current.Status.Conversation = append(current.Status.Conversation,
		agentsv1alpha1.TaskMessage{ID: "queued-one", Role: "user", Content: "Steer", DeliveryState: "queued", CreatedAt: metav1.Now()},
		agentsv1alpha1.TaskMessage{ID: "queued-two", Role: "user", Content: "Later", DeliveryState: "queued", CreatedAt: metav1.Now()})
	if err := store.Status().Update(context.Background(), &current); err != nil {
		t.Fatal(err)
	}
	if response := call("valid", "report_turn_start", `{"id":"turn-queue","messageIds":["queued-one"]}`); strings.Contains(response.Body.String(), `"error"`) {
		t.Fatal(response.Body.String())
	}
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(task), &current); err != nil {
		t.Fatal(err)
	}
	if current.Status.TurnStartedAt == nil || current.Status.TurnID != "turn-queue" {
		t.Fatal("missing current turn start")
	}
	for _, message := range current.Status.Conversation {
		if message.ID == "queued-one" && message.DeliveryState != "delivered" {
			t.Fatal("consumed prompt still queued")
		}
		if message.ID == "queued-two" && message.DeliveryState != "queued" {
			t.Fatal("unconsumed prompt acknowledged")
		}
	}
	started := current.Status.TurnStartedAt.DeepCopy()
	if response := call("valid", "report_turn_start", `{"id":"turn-queue","messageIds":["queued-one"]}`); strings.Contains(response.Body.String(), `"error"`) {
		t.Fatal(response.Body.String())
	}
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(task), &current); err != nil {
		t.Fatal(err)
	}
	if !current.Status.TurnStartedAt.Equal(started) {
		t.Fatal("retry reset elapsed turn duration")
	}
	var suspended agentsv1alpha1.Agent
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(agent), &suspended); err != nil {
		t.Fatal(err)
	}
	suspended.Spec.Suspend = true
	if err := store.Update(context.Background(), &suspended); err != nil {
		t.Fatal(err)
	}
	if response := call("valid", "create_task", `{"objective":"Late work"}`); !strings.Contains(response.Body.String(), "Agent is suspended") {
		t.Fatalf("worker of suspended Agent created a Task: %s", response.Body.String())
	}
	suspended.Spec.Suspend = false
	if err := store.Update(context.Background(), &suspended); err != nil {
		t.Fatal(err)
	}
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(task), &current); err != nil {
		t.Fatal(err)
	}
	current.Status.WorkerPodUID = "replacement-pod-uid"
	if err := store.Status().Update(context.Background(), &current); err != nil {
		t.Fatal(err)
	}
	if response := call("valid", "report_progress", `{"id":"late","content":"stale worker"}`); response.Code != http.StatusUnauthorized {
		t.Fatalf("stale worker still authorized: %d %s", response.Code, response.Body.String())
	}
	if response := call("valid", "set_task_title", `{"title":"Stale edit"}`); response.Code != http.StatusUnauthorized {
		t.Fatalf("stale worker could edit title: %d %s", response.Code, response.Body.String())
	}
}

func histogramCount(t *testing.T, histogram prometheus.Histogram) uint64 {
	var metric dto.Metric
	if err := histogram.Write(&metric); err != nil {
		t.Fatal(err)
	}
	return metric.GetHistogram().GetSampleCount()
}

func TestCreditExhaustionSuspendsOnlyOwningTask(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "own", Namespace: "team", UID: "task", CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour))}, Status: agentsv1alpha1.TaskStatus{WorkerPodUID: "pod"}}
	peer := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "peer", Namespace: "team"}}
	store := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(task, peer).WithObjects(task, peer).Build()
	h := &Handler{Client: store}
	scope := &workerScope{Task: *task, Pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "pod"}}}
	arg, _ := json.Marshal(map[string]any{"id": "credit-turn", "summary": "Credit balance is too low", "completedAt": time.Now().UTC().Format(time.RFC3339Nano), "backend": "claude", "failureReason": "credits_exhausted"})
	for range 2 {
		if _, err := h.callAgentTool(context.Background(), scope, "report_turn_result", arg); err != nil {
			t.Fatal(err)
		}
	}
	var current agentsv1alpha1.Task
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(task), &current); err != nil {
		t.Fatal(err)
	}
	if !current.Spec.Suspend || current.Status.Completed() || len(current.Status.Conversation) != 2 {
		t.Fatalf("credit suspension lost or duplicated history: %+v", current)
	}
	current = agentsv1alpha1.Task{}
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(peer), &current); err != nil {
		t.Fatal(err)
	}
	if current.Spec.Suspend {
		t.Fatal("peer suspended")
	}
}
