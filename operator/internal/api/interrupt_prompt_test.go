package api

import (
	"context"
	"encoding/json"
	"fmt"
	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	"github.com/codemowers/agent/operator/internal/rabbitmq"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"net/http"
	"net/http/httptest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"strings"
	"testing"
	"time"
)

type interruptPublisher struct {
	probePublisher
	messages []rabbitmq.PromptMessage
	failStop bool
	onPrompt func(string)
}

func (p *interruptPublisher) Publish(_ context.Context, _ string, msg rabbitmq.PromptMessage) error {
	p.messages = append(p.messages, msg)
	if msg.Type == "prompt" && p.onPrompt != nil {
		p.onPrompt(msg.MessageID)
	}
	if msg.Type == "stop" && p.failStop {
		return fmt.Errorf("broker unavailable")
	}
	return nil
}
func TestInterruptQueuesPromptBeforeStoppingAndReportsPartialSuccess(t *testing.T) {
	for _, failStop := range []bool{false, true} {
		t.Run(fmt.Sprint(failStop), func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = agentsv1alpha1.AddToScheme(scheme)
			agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "developer", Namespace: "team", UID: "agent-uid"}}
			task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "team", UID: "task-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: agentsv1alpha1.GroupVersion.String(), Kind: "Agent", Name: agent.Name, UID: agent.UID}}}, Spec: agentsv1alpha1.TaskSpec{AgentRef: agent.Name}, Status: agentsv1alpha1.TaskStatus{Phase: agentsv1alpha1.TaskPhaseRunning, Activity: "working", WorkerPodUID: "pod-uid"}}
			store := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(task).WithObjects(agent, task).Build()
			broker := &interruptPublisher{failStop: failStop}
			broker.onPrompt = func(id string) {
				var current agentsv1alpha1.Task
				if err := store.Get(context.Background(), client.ObjectKeyFromObject(task), &current); err != nil {
					t.Fatal(err)
				}
				current.Status.DeliveredMessageIDs = []string{id}
				if err := store.Status().Update(context.Background(), &current); err != nil {
					t.Fatal(err)
				}
			}
			h := &Handler{Client: store, Verifier: fakeVerifier{}, Reviewer: fakeReviewer{}, AMQP: broker}
			start := time.Now().UnixMilli()
			req := httptest.NewRequest(http.MethodPost, "/api/tasks/team/task/prompts", strings.NewReader(`{"body":"Use SSE instead","interrupt":true}`))
			req.Header.Set("Authorization", "Bearer valid")
			res := httptest.NewRecorder()
			h.routes().ServeHTTP(res, req)
			if res.Code != http.StatusOK {
				t.Fatalf("status %d: %s", res.Code, res.Body.String())
			}
			if len(broker.messages) != 2 || broker.messages[0].Type != "prompt" || broker.messages[1].Type != "stop" || broker.messages[1].PodUID != "pod-uid" || broker.messages[1].ReceivedAt < start {
				t.Fatalf("wrong delivery order/target: %+v", broker.messages)
			}
			var after agentsv1alpha1.Task
			if err := store.Get(context.Background(), client.ObjectKeyFromObject(task), &after); err != nil {
				t.Fatal(err)
			}
			for _, message := range after.Status.Conversation {
				if message.Role == "user" && message.DeliveryState != "delivered" {
					t.Fatal("fast delivery was overwritten as queued")
				}
			}
			var result map[string]string
			_ = json.Unmarshal(res.Body.Bytes(), &result)
			if result["status"] != "published" || (result["interruptError"] != "") != failStop {
				t.Fatalf("incorrect partial success: %+v", result)
			}
		})
	}
}
