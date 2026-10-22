package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	authv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

type recordingWaker struct{ calls []string }

func (w *recordingWaker) WakeTask(_ context.Context, namespace, name, actor, key, text, sourceURL string) error {
	if name == "missing" {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "tasks"}, name)
	}
	w.calls = append(w.calls, strings.Join([]string{namespace, name, actor, key, text, sourceURL}, "|"))
	return nil
}

func TestWakeRequiresAudienceTokenAndWakeVerb(t *testing.T) {
	kube := kubefake.NewSimpleClientset()
	kube.PrependReactor("create", "tokenreviews", func(action ktesting.Action) (bool, runtime.Object, error) {
		review := action.(ktesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		review.Status.Authenticated = review.Spec.Token == "valid" && review.Spec.Audiences[0] == "agent-operator-wake.harness"
		review.Status.User = authenticationv1.UserInfo{Username: "system:serviceaccount:agents:schedule"}
		return true, review, nil
	})
	kube.PrependReactor("create", "subjectaccessreviews", func(action ktesting.Action) (bool, runtime.Object, error) {
		review := action.(ktesting.CreateAction).GetObject().(*authv1.SubjectAccessReview)
		attrs := review.Spec.ResourceAttributes
		review.Status.Allowed = attrs.Verb == "wake" && attrs.Resource == "tasks" && attrs.Name != "forbidden"
		return true, review, nil
	})
	waker := &recordingWaker{}
	h := &Handler{TokenReviews: kube.AuthenticationV1(), AccessReviews: kube.AuthorizationV1(), Waker: waker, HarnessNamespace: "harness"}
	wake := func(token, task, key, body string) int {
		request := httptest.NewRequest(http.MethodPost, "/internal/v1/namespaces/agents/tasks/"+task+"/wake", strings.NewReader(body))
		request.SetPathValue("namespace", "agents")
		request.SetPathValue("name", task)
		request.Header.Set("Authorization", "Bearer "+token)
		if key != "" {
			request.Header.Set("Idempotency-Key", key)
		}
		response := httptest.NewRecorder()
		h.wakeTask(response, request)
		return response.Code
	}
	for _, c := range []struct {
		token, task, key, body string
		want                   int
	}{
		{"invalid", "watch", "", `{}`, http.StatusUnauthorized},
		{"valid", "forbidden", "", `{}`, http.StatusForbidden},
		{"valid", "missing", "", `{}`, http.StatusNotFound},
		{"valid", "watch", "", `{"url":"file:///etc/passwd"}`, http.StatusBadRequest},
		{"valid", "watch", "line\nbreak", `{}`, http.StatusBadRequest},
		{"valid", "watch", "2026-W43", `{"text":"Weekly watch","url":"https://lwkd.info/2026/20261015"}`, http.StatusAccepted},
	} {
		if got := wake(c.token, c.task, c.key, c.body); got != c.want {
			t.Fatalf("%s %s: got %d, want %d", c.token, c.task, got, c.want)
		}
	}
	if len(waker.calls) != 1 || waker.calls[0] != "agents|watch|system:serviceaccount:agents:schedule|2026-W43|Weekly watch|https://lwkd.info/2026/20261015" {
		t.Fatalf("unexpected wakes: %v", waker.calls)
	}
}
