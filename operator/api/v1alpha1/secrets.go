package v1alpha1

import (
	"net/url"
	"strings"
)

// CredentialSecret names the Secret whose keys the Agent's worker container
// receives as environment variables.
func CredentialSecret(agent *Agent) string {
	return agent.Spec.CredentialSecretRef.Name
}

// ModelCredentialEnv is the environment variable the backend's CLI reads its
// API key from.
func ModelCredentialEnv(backend string) string {
	return map[string]string{"claude": "ANTHROPIC_API_KEY", "codex": "OPENAI_API_KEY", "opencode": "AGENT_MODEL_API_KEY"}[backend]
}

// ModelCredentialKey is the credential Secret key holding the model's API key.
func ModelCredentialKey(model ModelSpec) string {
	if model.CredentialKeyRef != "" {
		return model.CredentialKeyRef
	}
	return ModelCredentialEnv(model.Backend)
}

// GitHubDotCom reports whether a GitHub provider is github.com or a GHE.com
// subdomain, which gh authenticates with GH_TOKEN rather than
// GH_ENTERPRISE_TOKEN.
func GitHubDotCom(serverURL string) bool {
	parsed, err := url.Parse(serverURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "github.com" || strings.HasSuffix(host, ".ghe.com")
}

// GitProviderTokenKey is the credential Secret key, and the worker
// environment variable, holding the provider's token.
func GitProviderTokenKey(provider GitProvider) string {
	switch {
	case provider.CredentialKeyRef != "":
		return provider.CredentialKeyRef
	case provider.Type == "gitea":
		return "GITEA_TOKEN"
	case GitHubDotCom(provider.ServerURL):
		return "GH_TOKEN"
	default:
		return "GH_ENTERPRISE_TOKEN"
	}
}

// MCPTokenEnv is the credential Secret key, and the worker environment
// variable, holding the bearer token for a credentialed MCP server. Server
// names are DNS labels, so mapping dashes to underscores cannot collide.
func MCPTokenEnv(server string) string {
	return "MCP_" + strings.ToUpper(strings.ReplaceAll(server, "-", "_")) + "_TOKEN"
}
