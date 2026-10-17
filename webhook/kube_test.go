package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHookIDResolvesSecretsInSubscriberNamespaces(t *testing.T) {
	seen := map[string]bool{}
	listPages := 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer service-account-token" {
			t.Errorf("Kubernetes request omitted service account token")
		}
		seen[r.URL.Path] = true
		body := ""
		status := http.StatusOK
		switch r.URL.Path {
		case "/apis/codemowers.cloud/v1alpha1/agents":
			listPages++
			if r.URL.Query().Get("continue") == "" {
				body = `{"metadata":{"continue":"next"},"items":[{"metadata":{"namespace":"team-a","name":"developer"},"spec":{"githubWebhooks":[{"serverURL":"https://github.com","hookID":"42","secretRef":"team-hook"}],"gitProviders":[{"type":"github","serverURL":"https://github.com"}]}},{"metadata":{"namespace":"team-c","name":"broken"},"spec":{"githubWebhooks":[{"serverURL":"https://ghe.example.test","hookID":"42","secretRef":"missing-hook"}],"gitProviders":[{"type":"github","serverURL":"https://ghe.example.test"}]}}]}`
			} else {
				body = `{"items":[{"metadata":{"namespace":"team-b","name":"support"},"spec":{"githubWebhooks":[{"serverURL":"https://github.com","hookID":"42","secretRef":"other-hook"}],"gitProviders":[{"type":"github","serverURL":"https://github.com"}]}}]}`
			}
		case "/api/v1/namespaces/team-a/secrets/team-hook":
			body = `{"data":{"token":"c2VjcmV0"}}`
		case "/api/v1/namespaces/team-b/secrets/other-hook":
			body = `{"data":{"token":"b3RoZXI="}}`
		default:
			status = http.StatusNotFound
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("service-account-token"), 0600); err != nil {
		t.Fatal(err)
	}
	hooks, err := (&kubeHookResolver{client: &http.Client{Transport: transport}, base: "https://kube.example", tokenFile: tokenFile}).Resolve(context.Background(), "42")
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 2 || string(hooks[0].Secret) != "secret" || string(hooks[1].Secret) != "other" || len(seen) != 4 || listPages != 2 {
		t.Fatalf("wrong webhook resolution: hooks %v, requests %v", hooks, seen)
	}
}
