package controller

import (
	"encoding/json"
	"strings"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// gitProviderEnv describes one Git provider to the worker, which writes the
// gh, tea and Git credential configuration from it.
type gitProviderEnv struct {
	Type      string `json:"type"`
	ServerURL string `json:"serverURL"`
	TokenEnv  string `json:"tokenEnv"`
}

// forgeCredentialEnv requires each Git provider's key of the Agent's
// credential Secret under its own name, so a missing key keeps the worker
// Pod from starting instead of leaving tools to fall back to another token,
// and lists the providers in AGENT_GIT_PROVIDERS. GH_HOST and
// GITEA_INSTANCE_URL, which select gh's and tea's environment login, are set
// only when they are unambiguous: tea sends GH_TOKEN to GITEA_INSTANCE_URL
// when GITEA_TOKEN is empty.
func forgeCredentialEnv(agent *agentsv1alpha1.Agent) []corev1.EnvVar {
	secret := agentsv1alpha1.CredentialSecret(agent)
	env := []corev1.EnvVar{}
	providers := []gitProviderEnv{}
	var enterprise, gitea []agentsv1alpha1.GitProvider
	for _, provider := range agent.Spec.GitProviders {
		key := agentsv1alpha1.GitProviderTokenKey(provider)
		env = append(env, corev1.EnvVar{Name: key, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key}}})
		providers = append(providers, gitProviderEnv{provider.Type, strings.TrimRight(provider.ServerURL, "/"), key})
		switch {
		case provider.Type == "gitea":
			gitea = append(gitea, provider)
		case !agentsv1alpha1.GitHubDotCom(provider.ServerURL):
			enterprise = append(enterprise, provider)
		}
	}
	if len(providers) == 0 {
		return nil
	}
	data, _ := json.Marshal(providers)
	env = append(env, corev1.EnvVar{Name: "AGENT_GIT_PROVIDERS", Value: string(data)})
	if len(enterprise) == 1 && agentsv1alpha1.GitProviderTokenKey(enterprise[0]) == "GH_ENTERPRISE_TOKEN" {
		env = append(env, corev1.EnvVar{Name: "GH_HOST", Value: hostOf(enterprise[0].ServerURL)})
	}
	if len(gitea) == 1 && agentsv1alpha1.GitProviderTokenKey(gitea[0]) == "GITEA_TOKEN" {
		env = append(env, corev1.EnvVar{Name: "GITEA_INSTANCE_URL", Value: strings.TrimRight(gitea[0].ServerURL, "/")})
	}
	return env
}

func hostOf(serverURL string) string {
	host := strings.TrimPrefix(strings.TrimPrefix(serverURL, "https://"), "http://")
	return strings.TrimRight(host, "/")
}
