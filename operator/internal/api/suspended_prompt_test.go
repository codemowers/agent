package api

import (
	"context"
	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"net/http"
	"net/http/httptest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"strings"
	"testing"
)

func TestSuspendedTaskQueuesSteeringWithoutResuming(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(scheme)
	agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "developer", Namespace: "team", UID: types.UID("agent-uid")}}
	task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "team", UID: types.UID("task-uid"), OwnerReferences: []metav1.OwnerReference{{APIVersion: agentsv1alpha1.GroupVersion.String(), Kind: "Agent", Name: agent.Name, UID: agent.UID}}}, Spec: agentsv1alpha1.TaskSpec{AgentRef: agent.Name, Suspend: true}, Status: agentsv1alpha1.TaskStatus{Phase: agentsv1alpha1.TaskPhasePaused}}
	store := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(task).WithObjects(agent, task).Build()
	broker := &probePublisher{}
	h := &Handler{Client: store, Verifier: fakeVerifier{}, Reviewer: fakeReviewer{}, AMQP: broker}
	request := httptest.NewRequest(http.MethodPost, "/api/tasks/team/task/prompts", strings.NewReader(`{"body":"Use SSE instead"}`))
	request.Header.Set("Authorization", "Bearer valid")
	response := httptest.NewRecorder()
	h.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("queue steering: %d %s", response.Code, response.Body.String())
	}
	var actual agentsv1alpha1.Task
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(task), &actual); err != nil {
		t.Fatal(err)
	}
	if !actual.Spec.Suspend || actual.Status.Phase != agentsv1alpha1.TaskPhasePaused || broker.count != 1 || broker.msg.Body != "Use SSE instead" || len(actual.Status.Conversation) != 1 {
		t.Fatalf("suspended steering was not retained without resume: %+v", actual)
	}
}
