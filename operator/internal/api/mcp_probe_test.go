package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	"github.com/codemowers/agent/operator/internal/rabbitmq"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type probePublisher struct {
	queue string
	msg   rabbitmq.PromptMessage
	count int
}

func (p *probePublisher) DeclareQueue(string) error      { return nil }
func (p *probePublisher) DeclareProbeQueue(string) error { return nil }
func (p *probePublisher) Publish(_ context.Context, queue string, msg rabbitmq.PromptMessage) error {
	p.queue, p.msg = queue, msg
	p.count++
	return nil
}

func TestMCPProbeRequestRequiresTaskAccessAndTargetsCurrentPod(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "developer", Namespace: "team", UID: types.UID("agent-uid")}}
	task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "team", UID: types.UID("task-uid"),
		OwnerReferences: []metav1.OwnerReference{{APIVersion: agentsv1alpha1.GroupVersion.String(), Kind: "Agent", Name: agent.Name, UID: agent.UID}}},
		Spec: agentsv1alpha1.TaskSpec{AgentRef: agent.Name}, Status: agentsv1alpha1.TaskStatus{
			Phase: agentsv1alpha1.TaskPhaseRunning, WorkerPodName: "worker", WorkerPodUID: "pod-uid"}}
	store := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(task).WithObjects(agent, task).Build()
	broker := &probePublisher{}
	h := &Handler{Client: store, Verifier: fakeVerifier{}, Reviewer: fakeReviewer{}, AMQP: broker}
	request := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/tasks/team/task/mcp-probe", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		h.routes().ServeHTTP(response, r)
		return response
	}
	if response := request(""); response.Code != http.StatusUnauthorized || broker.count != 0 {
		t.Fatalf("unauthorized probe accepted: %d", response.Code)
	}
	if response := request("valid"); response.Code != http.StatusOK || broker.count != 1 {
		t.Fatalf("probe not published: %d %s", response.Code, response.Body.String())
	}
	if broker.queue != rabbitmq.ProbeQueue("system:serviceaccount:team:agent-worker-developer:task", "pod-uid") || broker.msg.Type != "mcp_probe" || broker.msg.PodUID != "pod-uid" || len(broker.msg.MessageID) != 32 {
		t.Fatalf("wrong worker target: %+v", broker.msg)
	}
	if response := request("valid"); response.Code != http.StatusOK || broker.count != 1 {
		t.Fatalf("repeat probe was not coalesced: %d %s", response.Code, response.Body.String())
	}
	var current agentsv1alpha1.Task
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(task), &current); err != nil || current.Status.MCPProbe == nil || current.Status.MCPProbe.RequestID != broker.msg.MessageID {
		t.Fatalf("pending probe absent: %v %+v", err, current.Status.MCPProbe)
	}
}
