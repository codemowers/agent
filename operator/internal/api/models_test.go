package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type noCachedSecretClient struct{ client.Client }

func (c noCachedSecretClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	if _, ok := object.(*corev1.Secret); ok {
		return fmt.Errorf("Secret read used cached client")
	}
	return c.Client.Get(ctx, key, object, opts...)
}

func TestAgentModelsRequireAccessAndFilterProviderCatalog(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "developer", Namespace: "team"},
		Spec: agentsv1alpha1.AgentSpec{Model: agentsv1alpha1.ModelSpec{Backend: "codex", Name: "gpt-5", CredentialKeyRef: "openai"}}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: agentsv1alpha1.ProviderTokensSecretName, Namespace: "team"}, Data: map[string][]byte{"openai": []byte("private-token")}}
	store := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent, secret).Build()
	h := &Handler{Client: noCachedSecretClient{store}, APIReader: store, Verifier: fakeVerifier{}, Reviewer: fakeReviewer{},
		ModelHTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Host != "api.openai.com" || request.Header.Get("Authorization") != "Bearer private-token" {
				t.Errorf("unexpected provider request: %s", request.URL)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"gpt-5"},{"id":"gpt-image-1"}]}`)), Header: make(http.Header)}, nil
		})}}
	request := httptest.NewRequest(http.MethodGet, "/api/agents/team/developer/models", nil)
	request.Header.Set("Authorization", "Bearer valid")
	response := httptest.NewRecorder()
	h.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"name":"gpt-5"`) || strings.Contains(response.Body.String(), "gpt-image-1") || strings.Contains(response.Body.String(), "private-token") {
		t.Fatalf("unexpected model catalog: %d %s", response.Code, response.Body.String())
	}
	missing := httptest.NewRecorder()
	h.routes().ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/api/agents/team/developer/models", nil))
	if missing.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated catalog: %d", missing.Code)
	}
}
