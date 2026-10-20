package controller

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type forgeRoundTrip func(*http.Request) (*http.Response, error)

func (fn forgeRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func TestForgeURLsKeepGiteaSubpath(t *testing.T) {
	base, api, err := forgeURLs(agentsv1alpha1.GitProvider{Type: "gitea", ServerURL: "https://git.example.test/gitea/"})
	if err != nil || base != "https://git.example.test/gitea" || api != "https://git.example.test/gitea/api/v1" {
		t.Fatalf("unexpected Gitea URLs: %q %q %v", base, api, err)
	}
}

func TestForgeCapabilityProbe(t *testing.T) {
	for _, tc := range []struct {
		name, server, api, orgPath, orgBody string
	}{
		{"github", "https://github.com", "api.github.com", "/user/memberships/orgs/example", `{"role":"admin","state":"active"}`},
		{"gitea", "https://git.example.test", "git.example.test", "/users/agent/orgs/example/permissions", `{"can_create_repository":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "team"}, Spec: agentsv1alpha1.AgentSpec{
				GitProviders: []agentsv1alpha1.GitProvider{{Type: tc.name, ServerURL: tc.server, CredentialKeyRef: "forge-token"}},
			}}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: agentsv1alpha1.ProviderTokensSecretName, Namespace: "team"}, Data: map[string][]byte{"forge-token": []byte("private-token")}}
			store := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
			r := &AgentReconciler{APIReader: store, ForgeHTTPClient: &http.Client{Transport: forgeRoundTrip(func(request *http.Request) (*http.Response, error) {
				if request.URL.Host != tc.api || request.Header.Get("Authorization") != "Bearer private-token" {
					t.Errorf("unexpected forge request: %s", request.URL)
				}
				path := strings.TrimPrefix(request.URL.Path, "/api/v1")
				body := `[]`
				switch {
				case path == "/user":
					body = `{"login":"agent","id":7}`
				case path == "/user/repos":
					body = `[{"full_name":"example/project","owner":{"login":"example","type":"Organization"},"permissions":{"push":true}}]`
				case path == "/repos/search":
					body = `{"ok":true,"data":[{"full_name":"example/project","owner":{"login":"example","type":"Organization"},"permissions":{"push":true}}]}`
				case path == tc.orgPath:
					body = tc.orgBody
				case path == "/orgs/example":
					body = `{"members_can_create_repositories":true}`
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}}
			result := r.probeForges(context.Background(), agent)
			if len(result) != 1 || result[0].Status != "available" || result[0].RepositoryCount != 1 || len(result[0].Destinations) != 2 || result[0].Destinations[1].Policy != "allowed" {
				t.Fatalf("unexpected %s probe: %+v", tc.name, result)
			}
			if strings.Contains(result[0].Detail, "private-token") {
				t.Fatal("credential leaked into status")
			}
		})
	}
}

func TestAgentReconcileCachesForgeProbe(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "team"}}
	store := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(agent).WithObjects(agent).Build()
	r := &AgentReconciler{Client: store, APIReader: store}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(agent)}
	result, err := r.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("unexpected periodic refresh: %s", result.RequeueAfter)
	}
	var updated agentsv1alpha1.Agent
	if err := store.Get(context.Background(), request.NamespacedName, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.ForgesCheckedAt == nil {
		t.Fatal("forge check timestamp not persisted")
	}
	result, err = r.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("unexpected periodic refresh: %s", result.RequeueAfter)
	}
}
