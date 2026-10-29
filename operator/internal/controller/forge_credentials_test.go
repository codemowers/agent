package controller

import (
	"encoding/json"
	"testing"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func forgeEnvMap(env []corev1.EnvVar) map[string]corev1.EnvVar {
	byName := map[string]corev1.EnvVar{}
	for _, variable := range env {
		byName[variable.Name] = variable
	}
	return byName
}

func TestForgeCredentialEnvironment(t *testing.T) {
	agent := &agentsv1alpha1.Agent{Spec: agentsv1alpha1.AgentSpec{
		CredentialSecretRef: agentsv1alpha1.CredentialSecretRef{Name: "dev-credentials"},
		GitProviders: []agentsv1alpha1.GitProvider{
			{Type: "github", ServerURL: "https://github.com"},
			{Type: "github", ServerURL: "https://ghe.example.test/"},
			{Type: "gitea", ServerURL: "https://git.example.test:3000/one"},
		},
	}}
	env := forgeEnvMap(forgeCredentialEnv(agent))
	for _, key := range []string{"GH_TOKEN", "GH_ENTERPRISE_TOKEN", "GITEA_TOKEN"} {
		ref := env[key].ValueFrom
		if ref == nil || ref.SecretKeyRef.Name != "dev-credentials" || ref.SecretKeyRef.Key != key ||
			(ref.SecretKeyRef.Optional != nil && *ref.SecretKeyRef.Optional) {
			t.Fatalf("%s must be a required key of the Agent's credential Secret: %+v", key, env[key])
		}
	}
	if env["GH_HOST"].Value != "ghe.example.test" || env["GITEA_INSTANCE_URL"].Value != "https://git.example.test:3000/one" {
		t.Fatalf("single Enterprise Server and Gitea providers select gh and tea environment logins: %+v", env)
	}
	var providers []gitProviderEnv
	if err := json.Unmarshal([]byte(env["AGENT_GIT_PROVIDERS"].Value), &providers); err != nil || len(providers) != 3 ||
		providers[1] != (gitProviderEnv{"github", "https://ghe.example.test", "GH_ENTERPRISE_TOKEN"}) {
		t.Fatalf("worker provider list: %v %+v", err, providers)
	}

	// A second Gitea with its own key leaves tea to the generated config file,
	// and a Gitea provider reading another key never selects tea's
	// environment login, which would fall back to sending GH_TOKEN.
	agent.Spec.GitProviders = append(agent.Spec.GitProviders,
		agentsv1alpha1.GitProvider{Type: "gitea", ServerURL: "https://git.example.test:3000/two", CredentialKeyRef: "GITEA_TOKEN_TWO"})
	env = forgeEnvMap(forgeCredentialEnv(agent))
	if _, ok := env["GITEA_INSTANCE_URL"]; ok {
		t.Fatalf("GITEA_INSTANCE_URL is ambiguous with two Gitea providers: %+v", env)
	}
	if env["GITEA_TOKEN_TWO"].ValueFrom.SecretKeyRef.Key != "GITEA_TOKEN_TWO" {
		t.Fatalf("override key is read under its own name: %+v", env["GITEA_TOKEN_TWO"])
	}
	agent.Spec.GitProviders = []agentsv1alpha1.GitProvider{{Type: "gitea", ServerURL: "https://gitea.com", CredentialKeyRef: "CODEBERG"}}
	if _, ok := forgeEnvMap(forgeCredentialEnv(agent))["GITEA_INSTANCE_URL"]; ok {
		t.Fatal("a Gitea provider without GITEA_TOKEN must not select tea's environment login")
	}
	agent.Spec.GitProviders = nil
	if env := forgeCredentialEnv(agent); env != nil {
		t.Fatalf("no providers, no forge environment: %+v", env)
	}
}
