package controller

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestFirstWebhookCreatesTaskOnlyForCurrentAssignee(t *testing.T) {
	assigned := true
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status := http.StatusOK
		body := ""
		if r.Header.Get("Authorization") != "Bearer configured-token" {
			t.Error("forge credential missing")
			status = http.StatusUnauthorized
		}
		switch r.URL.Path {
		case "/api/v3/user":
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
		Spec: agentsv1alpha1.AgentSpec{Suspended: true, GitProviders: []agentsv1alpha1.GitProvider{{Type: "github", ServerURL: "https://forge.example.test",
			CredentialSecretRef: "git-token"}}}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "git-token", Namespace: "team"},
		Data: map[string][]byte{"token": []byte("configured-token")}}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent, secret).Build()
	consumer := &EventConsumer{Client: k8s, APIReader: k8s, HTTPClient: &http.Client{Transport: transport}}
	event := parsedEvent{source: "github", serverURL: "forge.example.test", owner: "team", repo: "project", kind: "issue", id: "8"}
	created, err := consumer.createTaskForAssignedIssue(context.Background(), "team", "developer", event, "delivery-1")
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
	if !tasks.Items[0].Spec.Suspend {
		t.Fatal("webhook Task did not inherit Agent suspension")
	}
	assigned = false
	event.id = "9"
	created, err = consumer.createTaskForAssignedIssue(context.Background(), "team", "developer", event, "delivery-2")
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
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
