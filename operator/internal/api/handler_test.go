package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fakeVerifier struct{}

func (fakeVerifier) Verify(_ context.Context, token string) (caller, error) {
	if token == "valid" {
		return caller{user: "https://issuer/#alice", subject: "alice", groups: []string{"system:authenticated"}}, nil
	}
	return caller{}, fmt.Errorf("invalid token")
}

type fakeReviewer struct{}

func (fakeReviewer) Allowed(_ context.Context, _ caller, _, name string) (bool, error) {
	return name == "developer", nil
}

func TestTaskAPIRequiresIdentityAndAgentAccess(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	checked := metav1.Now()
	developer := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "developer", Namespace: "team", UID: types.UID("developer-uid"), Generation: 1},
		Spec: agentsv1alpha1.AgentSpec{Suspended: true, IdleSeconds: int32Ptr(90), Model: agentsv1alpha1.ModelSpec{Backend: "claude", Name: "claude-default"}, Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}}},
		Status: agentsv1alpha1.AgentStatus{ForgesCheckedAt: &checked, ForgesObservedGeneration: 1, Forges: []agentsv1alpha1.ForgeCapability{{Type: "gitea", ServerURL: "https://git.example.test", Status: "available"}}}}
	support := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "support", Namespace: "team", UID: types.UID("support-uid")}}
	developerTask := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "dev-task", Namespace: "team", OwnerReferences: []metav1.OwnerReference{{APIVersion: agentsv1alpha1.GroupVersion.String(), Kind: "Agent", Name: "developer", UID: developer.UID}}},
		Spec: agentsv1alpha1.TaskSpec{Title: "Review the issue", IdleSeconds: int32Ptr(90), ExternalReferences: []agentsv1alpha1.ExternalReference{{Source: "gitea", Kind: "issue", ServerURL: "https://git.example.test", RepositoryOwner: "team", RepositoryName: "repo", ID: "1"}}},
		Status: agentsv1alpha1.TaskStatus{Phase: agentsv1alpha1.TaskPhaseRunning, Activity: "idle", WorkerPodName: "worker", LastResultAt: &checked, Progress: &agentsv1alpha1.TaskProgress{Summary: "Worker reply"},
			ExternalReferences: []agentsv1alpha1.ExternalReference{{Source: "imap", Kind: "thread", ID: "<root@example.test>", MailboxRef: "mail", MailboxUID: "mail-uid"}},
			Conversation:       []agentsv1alpha1.TaskMessage{{ID: "reply-1", Role: "agent", Content: "Worker reply", CreatedAt: metav1.Now()}}}}
	supportTask := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "support-task", Namespace: "team", OwnerReferences: []metav1.OwnerReference{{APIVersion: agentsv1alpha1.GroupVersion.String(), Kind: "Agent", Name: "support", UID: support.UID}}}}
	h := &Handler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(developer, support, developerTask, supportTask).Build(),
		Verifier: fakeVerifier{}, Reviewer: fakeReviewer{}}

	missing := httptest.NewRecorder()
	h.listTasks(missing, httptest.NewRequest(http.MethodGet, "/api/tasks", nil))
	if missing.Code != http.StatusUnauthorized {
		t.Fatalf("missing identity: got %d", missing.Code)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	request.Header.Set("Authorization", "Bearer valid")
	agentResponse := httptest.NewRecorder()
	h.routes().ServeHTTP(agentResponse, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	if agentResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated Agent catalog: %d", agentResponse.Code)
	}
	agentRequest := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	agentRequest.Header.Set("Authorization", "Bearer valid")
	agentResponse = httptest.NewRecorder()
	h.routes().ServeHTTP(agentResponse, agentRequest)
	var agentCatalog []agentItem
	if agentResponse.Code != http.StatusOK || json.Unmarshal(agentResponse.Body.Bytes(), &agentCatalog) != nil || len(agentCatalog) != 1 || agentCatalog[0].Name != "developer" || agentCatalog[0].IdleSeconds != 90 || len(agentCatalog[0].Forges) != 1 {
		t.Fatalf("Agent catalog leaked or omitted settings: %d %s", agentResponse.Code, agentResponse.Body.String())
	}
	response := httptest.NewRecorder()
	h.listTasks(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("list tasks: got %d: %s", response.Code, response.Body.String())
	}
	var tasks []taskItem
	if err := json.Unmarshal(response.Body.Bytes(), &tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Name != "dev-task" {
		t.Fatalf("Agent access leaked another Task: %+v", tasks)
	}
	if tasks[0].Summary != "Worker reply" {
		t.Fatalf("worker reply missing from API: %+v", tasks[0])
	}
	if tasks[0].Title != "Review the issue" {
		t.Fatalf("Task title missing from list: %+v", tasks[0])
	}
	if tasks[0].IdleSeconds != 90 || tasks[0].Activity != "idle" || tasks[0].LastResultAt == "" {
		t.Fatalf("idle timing missing from list: %+v", tasks[0])
	}
	if tasks[0].LastMessageID != "reply-1" {
		t.Fatalf("last message absent from list: %+v", tasks[0])
	}
	for _, tc := range []struct {
		name string
		want int
	}{{"dev-task", http.StatusOK}, {"support-task", http.StatusForbidden}} {
		get := httptest.NewRequest(http.MethodGet, "/api/tasks/team/"+tc.name, nil)
		get.Header.Set("Authorization", "Bearer valid")
		response := httptest.NewRecorder()
		h.routes().ServeHTTP(response, get)
		if response.Code != tc.want {
			t.Fatalf("GET %s: %d %s", tc.name, response.Code, response.Body.String())
		}
		if tc.want == http.StatusOK {
			var detail taskDetail
			if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil {
				t.Fatal(err)
			}
			if len(detail.Conversation) != 1 || detail.Conversation[0].Content != "Worker reply" {
				t.Fatalf("missing authorized conversation: %+v", detail)
			}
			if detail.Title != "Review the issue" {
				t.Fatalf("Task title missing from detail: %+v", detail)
			}
			if detail.IdleSeconds != 90 || detail.Activity != "idle" || detail.LastResultAt == "" {
				t.Fatalf("idle timing missing from detail: %+v", detail)
			}
			if len(detail.ExternalReferences) != 2 || detail.ExternalReferences[0].RepositoryName != "repo" || detail.ExternalReferences[1].Source != "imap" {
				t.Fatalf("missing external reference: %+v", detail)
			}
		}
	}
	create := httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(`{"agentName":"developer","agentNamespace":"team","objective":"Review the issue","modelName":"claude-selected"}`))
	create.Header.Set("Authorization", "Bearer valid")
	created := httptest.NewRecorder()
	h.createTask(created, create)
	if created.Code != http.StatusCreated {
		t.Fatalf("create Task: %d %s", created.Code, created.Body.String())
	}
	var name struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &name); err != nil {
		t.Fatal(err)
	}
	var task agentsv1alpha1.Task
	if err := h.Client.Get(context.Background(), client.ObjectKey{Namespace: "team", Name: name.Name}, &task); err != nil {
		t.Fatal(err)
	}
	if task.Spec.AgentRef != "developer" {
		t.Fatalf("Task did not reference selected Agent: %q", task.Spec.AgentRef)
	}
	if !task.Spec.Suspend {
		t.Fatal("Task did not inherit Agent suspension")
	}
	if task.Spec.IdleSeconds == nil || *task.Spec.IdleSeconds != 90 {
		t.Fatalf("Task did not copy Agent idle timeout: %+v", task.Spec.IdleSeconds)
	}
	if task.Spec.Resources == nil || task.Spec.Resources.Requests.Memory().String() != "256Mi" {
		t.Fatalf("Task did not copy Agent resources: %+v", task.Spec.Resources)
	}
	if task.Spec.Model == nil || task.Spec.Model.Name != "claude-selected" || task.Spec.Model.Backend != "claude" {
		t.Fatalf("Task did not copy selected model with Agent backend: %+v", task.Spec.Model)
	}
}

func TestAgentAccessOverridesRBAC(t *testing.T) {
	h := &Handler{Reviewer: fakeReviewer{}}
	alice := caller{user: "https://issuer/#alice", subject: "alice", groups: []string{"system:authenticated", "admins"}}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, tc := range []struct {
		name   string
		agent  string
		access *agentsv1alpha1.AgentAccess
		want   bool
	}{
		{"RBAC without ACL", "developer", nil, true},
		{"no RBAC without ACL", "support", nil, false},
		{"ACL user without RBAC", "support", &agentsv1alpha1.AgentAccess{Users: []string{"alice"}}, true},
		{"ACL group without RBAC", "support", &agentsv1alpha1.AgentAccess{Groups: []string{"admins"}}, true},
		{"ACL excludes despite RBAC", "developer", &agentsv1alpha1.AgentAccess{Users: []string{"bob"}}, false},
		{"empty ACL admits nobody", "developer", &agentsv1alpha1.AgentAccess{}, false},
		{"issuer identity is not a subject", "support", &agentsv1alpha1.AgentAccess{Users: []string{"https://issuer/#alice"}}, false},
	} {
		agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: tc.agent, Namespace: "team"}, Spec: agentsv1alpha1.AgentSpec{Access: tc.access}}
		if got, err := h.canAccessAgent(r, alice, agent); err != nil || got != tc.want {
			t.Errorf("%s: got %v, %v", tc.name, got, err)
		}
	}
}

func TestTaskLastMessageIgnoresSystemStatus(t *testing.T) {
	created := metav1.NewTime(time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC))
	mail := metav1.NewTime(created.Add(2 * time.Minute))
	webhook := metav1.NewTime(created.Add(3 * time.Minute))
	agent := metav1.NewTime(created.Add(4 * time.Minute))
	system := metav1.NewTime(created.Add(5 * time.Minute))
	task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: created},
		Status: agentsv1alpha1.TaskStatus{Conversation: []agentsv1alpha1.TaskMessage{
			{ID: "mail", Role: "external", Source: "mail", CreatedAt: mail},
			{ID: "hook", Role: "external", Source: "webhook", CreatedAt: webhook},
			{ID: "agent", Role: "agent", CreatedAt: agent},
			{ID: "pod", Role: "system", CreatedAt: system},
		}}}
	if got := taskDisplay(task).LastMessageAt; got != agent.Format(time.RFC3339Nano) {
		t.Fatalf("system status moved Task: got %q", got)
	}
	task.Status.Conversation = nil
	if got := taskDisplay(task).LastMessageAt; got != created.Format(time.RFC3339Nano) {
		t.Fatalf("objective fallback missing: got %q", got)
	}
}

func TestTaskSuspendAndStopControls(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	owner := func(name string, uid types.UID) []metav1.OwnerReference {
		return []metav1.OwnerReference{{APIVersion: agentsv1alpha1.GroupVersion.String(), Kind: "Agent", Name: name, UID: uid}}
	}
	developer := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "developer", Namespace: "team", UID: "developer-uid"}}
	support := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "support", Namespace: "team", UID: "support-uid"}}
	task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "dev-task", Namespace: "team", OwnerReferences: owner("developer", developer.UID)},
		Status: agentsv1alpha1.TaskStatus{Phase: agentsv1alpha1.TaskPhaseRunning, Activity: "idle", WorkerPodUID: "pod-uid"}}
	completed := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "done-task", Namespace: "team", OwnerReferences: owner("developer", developer.UID)}, Spec: agentsv1alpha1.TaskSpec{Suspend: true},
		Status: agentsv1alpha1.TaskStatus{Completion: &agentsv1alpha1.TaskCompletion{CompletedAt: &metav1.Time{Time: time.Now()}}}}
	foreign := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "support-task", Namespace: "team", OwnerReferences: owner("support", support.UID)}}
	h := &Handler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(developer, support, task, completed, foreign).
		WithStatusSubresource(&agentsv1alpha1.Task{}).Build(), Verifier: fakeVerifier{}, Reviewer: fakeReviewer{}}
	post := func(path, body string) int {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer valid")
		response := httptest.NewRecorder()
		h.routes().ServeHTTP(response, request)
		return response.Code
	}
	suspended := func(name string) bool {
		var current agentsv1alpha1.Task
		if err := h.Client.Get(context.Background(), client.ObjectKey{Namespace: "team", Name: name}, &current); err != nil {
			t.Fatal(err)
		}
		return current.Spec.Suspend
	}
	if code := post("/api/tasks/team/dev-task/suspend", `{"suspend":true}`); code != http.StatusOK || !suspended("dev-task") {
		t.Fatalf("pause: %d", code)
	}
	if code := post("/api/tasks/team/dev-task/suspend", `{"suspend":false}`); code != http.StatusOK || suspended("dev-task") {
		t.Fatalf("resume: %d", code)
	}
	if code := post("/api/tasks/team/done-task/suspend", `{"suspend":false}`); code != http.StatusConflict || !suspended("done-task") {
		t.Fatalf("completed Task resumed: %d", code)
	}
	if code := post("/api/tasks/team/support-task/suspend", `{"suspend":true}`); code != http.StatusForbidden || suspended("support-task") {
		t.Fatalf("foreign Task paused: %d", code)
	}
	if code := post("/api/tasks/team/dev-task/stop", ``); code != http.StatusServiceUnavailable && code != http.StatusConflict {
		t.Fatalf("stop without RabbitMQ or running turn: %d", code)
	}
}
