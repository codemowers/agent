package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ClusterMCPServer advertises an MCP server to the workers of every
// namespace that hold the custom verb "use" on it. Its name is the endpoint
// name workers see.
type ClusterMCPServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MCPServerSpec `json:"spec"`
}
type ClusterMCPServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterMCPServer `json:"items"`
}

// MCPServer advertises an MCP server to the workers of its namespace that
// hold "use" on it; it overrides a ClusterMCPServer of the same name.
type MCPServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MCPServerSpec `json:"spec"`
}
type MCPServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MCPServer `json:"items"`
}
type MCPServerSpec struct {
	URL string `json:"url"`
	// Audience is the audience of the projected ServiceAccount token workers
	// present, for servers with an audience contract of their own.
	Audience string `json:"audience,omitempty"`
	// CredentialKeyRef names a key of the worker namespace's provider tokens
	// Secret that workers present as a bearer token instead of a
	// ServiceAccount token, for servers such as Linear that authorize each
	// call with their own credentials.
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

func (in *ClusterMCPServer) DeepCopyObject() runtime.Object {
	out := *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	return &out
}
func (in *ClusterMCPServerList) DeepCopyObject() runtime.Object {
	out := *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	out.Items = make([]ClusterMCPServer, len(in.Items))
	for i := range in.Items {
		out.Items[i] = *(in.Items[i].DeepCopyObject().(*ClusterMCPServer))
	}
	return &out
}
func (in *MCPServer) DeepCopyObject() runtime.Object {
	out := *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	return &out
}
func (in *MCPServerList) DeepCopyObject() runtime.Object {
	out := *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	out.Items = make([]MCPServer, len(in.Items))
	for i := range in.Items {
		out.Items[i] = *(in.Items[i].DeepCopyObject().(*MCPServer))
	}
	return &out
}
