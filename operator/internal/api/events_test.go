package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type flushingRecorder struct {
	*httptest.ResponseRecorder
	flushed chan struct{}
}

func (w *flushingRecorder) Flush() {
	w.ResponseRecorder.Flush()
	w.flushed <- struct{}{}
}

func TestEventTopicsOnlyWakeSubscribedAgent(t *testing.T) {
	events := NewEvents(nil)
	updates, unsubscribe := events.subscribe(map[string]bool{"team/developer": true})
	defer unsubscribe()
	events.publish("tasks", "team/support")
	select {
	case event := <-updates:
		t.Fatalf("unsubscribed Agent delivered %q", event)
	default:
	}
	events.publish("tasks", "team/developer")
	if event := <-updates; event != "tasks" {
		t.Fatalf("expected Task invalidation, got %q", event)
	}
	events.publish("agents", "")
	if event := <-updates; event != "agents" {
		t.Fatalf("expected Agent invalidation, got %q", event)
	}
}

func TestEventStreamChecksAgentAccessAndForwardsUpdates(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	developer := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "developer", Namespace: "team"}}
	support := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "support", Namespace: "team"}}
	events := NewEvents(nil)
	h := &Handler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(developer, support).Build(),
		Verifier: fakeVerifier{}, Reviewer: fakeReviewer{}, Events: events}
	request := func(body string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/events", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer valid")
		return r
	}
	denied := httptest.NewRecorder()
	h.streamEvents(denied, request(`{"agents":[{"namespace":"team","name":"support"}]}`))
	if denied.Code != http.StatusForbidden {
		t.Fatalf("unauthorized topic: %d", denied.Code)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &flushingRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{}, 2)}
	done := make(chan struct{})
	go func() {
		h.streamEvents(stream, request(`{"agents":[{"namespace":"team","name":"developer"}]}`).WithContext(ctx))
		close(done)
	}()
	select {
	case <-stream.flushed:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not connect")
	}
	events.publish("tasks", "team/developer")
	select {
	case <-stream.flushed:
	case <-time.After(2 * time.Second):
		t.Fatal("Task event was not delivered")
	}
	cancel()
	<-done
	if stream.Code != http.StatusOK || !strings.Contains(stream.Body.String(), "event: tasks\ndata: {}\n\n") {
		t.Fatalf("unexpected stream response: %d %q", stream.Code, stream.Body.String())
	}
}
