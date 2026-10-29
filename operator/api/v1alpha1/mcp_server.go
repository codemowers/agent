package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterMCPServer advertises an MCP server to the workers of every
// namespace that hold the custom verb "use" on it. Its name is the endpoint
// name workers see.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=".spec.url"
// +kubebuilder:printcolumn:name="Audience",type=string,JSONPath=".spec.audience"
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([a-z0-9-]{0,57}[a-z0-9])?$') && !self.metadata.name.startsWith('mailbox-')",message="name must be a DNS label of at most 59 characters not starting with mailbox-"
type ClusterMCPServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MCPServerSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type ClusterMCPServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterMCPServer `json:"items"`
}

// MCPServer advertises an MCP server to the workers of its namespace that
// hold "use" on it; it overrides a ClusterMCPServer of the same name.
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=".spec.url"
// +kubebuilder:printcolumn:name="Audience",type=string,JSONPath=".spec.audience"
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([a-z0-9-]{0,57}[a-z0-9])?$') && !self.metadata.name.startsWith('mailbox-')",message="name must be a DNS label of at most 59 characters not starting with mailbox-"
type MCPServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MCPServerSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type MCPServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MCPServer `json:"items"`
}

// +kubebuilder:validation:XValidation:rule="!(has(self.audience) && has(self.credentialKeyRef))",message="audience and credentialKeyRef are mutually exclusive"
type MCPServerSpec struct {
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https?://`
	URL string `json:"url"`
	// Audience is the audience of the projected ServiceAccount token workers
	// present, for servers with an audience contract of their own.
	// +kubebuilder:validation:MaxLength=2048
	Audience string `json:"audience,omitempty"`
	// CredentialKeyRef names a key of the worker namespace's provider tokens
	// Secret that workers present as a bearer token instead of a
	// ServiceAccount token, for servers such as Linear that authorize each
	// call with their own credentials.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	CredentialKeyRef string `json:"credentialKeyRef,omitempty"`
}

// TokenAudience is the explicit audience, else the <service>.<namespace> of
// an in-cluster URL; a server outside the cluster without an audience gets
// no token, and a server with a credential gets no ServiceAccount token.
func (s MCPServerSpec) TokenAudience() string {
	if s.CredentialKeyRef != "" {
		return ""
	}
	if s.Audience != "" {
		return s.Audience
	}
	return ServiceAudience(s.URL)
}
