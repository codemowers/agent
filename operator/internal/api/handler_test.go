package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fakeVerifier struct{}

func (fakeVerifier) Verify(_ context.Context, token string) (caller, error) {
	if token == "valid" {
		return caller{user: "https://issuer/#alice", groups: []string{"system:authenticated"}}, nil
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
	developer := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "developer", Namespace: "team", UID: types.UID("developer-uid")},
		Spec: agentsv1alpha1.AgentSpec{Suspended: true, Access: agentsv1alpha1.AgentAccess{Subjects: []string{"someone-else"}}}}
	support := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "support", Namespace: "team", UID: types.UID("support-uid")},
		Spec: agentsv1alpha1.AgentSpec{Access: agentsv1alpha1.AgentAccess{Subjects: []string{"alice"}}}}
	developerTask := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "dev-task", Namespace: "team", OwnerReferences: []metav1.OwnerReference{{APIVersion: agentsv1alpha1.GroupVersion.String(), Kind: "Agent", Name: "developer", UID: developer.UID}}},
		Status: agentsv1alpha1.TaskStatus{Progress: &agentsv1alpha1.TaskProgress{Summary: "Worker reply"}}}
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
	create := httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(`{"agentName":"developer","agentNamespace":"team","objective":"Review the issue"}`))
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
	if !task.Spec.Suspend {
		t.Fatal("Task did not inherit Agent suspension")
	}
}
