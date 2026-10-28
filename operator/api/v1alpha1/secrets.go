package v1alpha1

const ProviderTokensSecretName = "agent-operator-provider-tokens"

// ProviderTokensSecret names the Secret in the Agent's namespace whose keys
// its model, Git provider and MCP server credentialKeyRefs select.
func ProviderTokensSecret(agent *Agent) string {
	if agent.Spec.CredentialSecretName != "" {
		return agent.Spec.CredentialSecretName
	}
	return ProviderTokensSecretName
}
