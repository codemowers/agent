package controller

import (
	"strings"
	"testing"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
)

func TestForgeCredentialEnvironment(t *testing.T) {
	providers := []agentsv1alpha1.GitProvider{
		{Type: "github", ServerURL: "https://github.com", CredentialKeyRef: "github-main"},
		{Type: "gitea", ServerURL: "https://gitea.com", CredentialKeyRef: "gitea-main"},
	}
	env := forgeCredentialEnv(providers)
	if len(env) != 2 || env[0].Name != "GH_TOKEN" || env[1].Name != "GITEA_TOKEN" {
		t.Fatalf("single provider of each type should get default token names: %+v", env)
	}
	for i, key := range []string{"github-main", "gitea-main"} {
		if env[i].ValueFrom.SecretKeyRef.Name != agentsv1alpha1.ProviderTokensSecretName || env[i].ValueFrom.SecretKeyRef.Key != key {
			t.Fatalf("wrong Secret key selected for %s: %+v", env[i].Name, env[i].ValueFrom)
		}
	}
	providers = append(providers,
		agentsv1alpha1.GitProvider{Type: "github", ServerURL: "https://ghe.example.test", CredentialKeyRef: "github-enterprise"},
		agentsv1alpha1.GitProvider{Type: "gitea", ServerURL: "https://git.example.test:3000/one", CredentialKeyRef: "gitea-one"},
		agentsv1alpha1.GitProvider{Type: "gitea", ServerURL: "https://git.example.test:3000/two", CredentialKeyRef: "gitea-two"},
	)
	env = forgeCredentialEnv(providers)
	if len(env) != 5 {
		t.Fatalf("missing provider credentials: %+v", env)
	}
	want := map[string]string{
		"GH_TOKEN_GITHUB_COM":       "github-main",
		"GH_TOKEN_GHE_EXAMPLE_TEST": "github-enterprise",
		"GITEA_TOKEN_GITEA_COM":     "gitea-main",
	}
	seen := make(map[string]bool)
	for _, variable := range env {
		if variable.Name == "GH_TOKEN" || variable.Name == "GITEA_TOKEN" || seen[variable.Name] {
			t.Fatalf("ambiguous or duplicate token name: %s", variable.Name)
		}
		seen[variable.Name] = true
		if key, ok := want[variable.Name]; ok {
			if variable.ValueFrom.SecretKeyRef.Key != key {
				t.Fatalf("wrong key for %s", variable.Name)
			}
		} else if !strings.HasPrefix(variable.Name, "GITEA_TOKEN_GIT_EXAMPLE_TEST_3000_") {
			t.Fatalf("unexpected token name: %s", variable.Name)
		}
	}
}
