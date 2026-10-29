package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WebhookReceiver is a namespaced endpoint at /hooks/<namespace>/<name> for
// WebhookBindings in its namespace.
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Protocol",type=string,JSONPath=".spec.protocolRef"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.senders.secretKeyRef) || !has(self.spec.senders.secretKeyRef.__namespace__)",message="secretKeyRef.namespace is only allowed on a ClusterWebhookReceiver"
type WebhookReceiver struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WebhookReceiverSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type WebhookReceiverList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WebhookReceiver `json:"items"`
}

// ClusterWebhookReceiver is an endpoint at /hooks/<name> that fans each
// event out to the WebhookBindings of any namespace referencing it.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Protocol",type=string,JSONPath=".spec.protocolRef"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.senders.secretKeyRef) || has(self.spec.senders.secretKeyRef.__namespace__)",message="secretKeyRef.namespace is required on a ClusterWebhookReceiver"
type ClusterWebhookReceiver struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WebhookReceiverSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type ClusterWebhookReceiverList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterWebhookReceiver `json:"items"`
}

type WebhookReceiverSpec struct {
	ProtocolRef string `json:"protocolRef"`
	// Match holds settings the protocol's CEL reads as receiver.match, such
	// as serverURL.
	// +kubebuilder:validation:MaxProperties=16
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(self[k]) <= 1024)",message="match values are at most 1024 characters"
	Match   map[string]string `json:"match,omitempty"`
	Senders WebhookSenders    `json:"senders"`
}

// WebhookSenders lists how a receiver authenticates deliveries: a request
// must pass the secret signature, a listed ServiceAccount's token or the
// Ed25519 signature, and also come from an allowed address when GitHubMeta or
// CIDRs restrict it.
// +kubebuilder:validation:XValidation:rule="has(self.secretKeyRef) || has(self.serviceAccounts) || has(self.ed25519PublicKey)",message="secretKeyRef, serviceAccounts or ed25519PublicKey is required"
// +kubebuilder:validation:XValidation:rule="!has(self.serviceAccounts) || has(self.audience)",message="serviceAccounts require an audience"
type WebhookSenders struct {
	// SecretKeyRef selects the signing key: a Secret in the receiver's
	// namespace, or in the named namespace for a ClusterWebhookReceiver.
	SecretKeyRef *WebhookSecretKeyRef `json:"secretKeyRef,omitempty"`
	// GitHubMeta is a GitHub or GHES meta API URL; deliveries must come from
	// its hooks address ranges.
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^https://`
	GitHubMeta string `json:"githubMeta,omitempty"`
	// CIDRs restrict deliveries to these client address ranges.
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=64
	CIDRs []string `json:"cidrs,omitempty"`
	// ServiceAccounts are <namespace>/<name> senders presenting a projected
	// token for Audience.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?/[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	ServiceAccounts []string `json:"serviceAccounts,omitempty"`
	// +kubebuilder:validation:MaxLength=253
	Audience string `json:"audience,omitempty"`
	// Ed25519PublicKey is the hex public key for a protocol signature.
	// +kubebuilder:validation:Pattern=`^[0-9a-fA-F]{64}$`
	Ed25519PublicKey string `json:"ed25519PublicKey,omitempty"`
}

type WebhookSecretKeyRef struct {
	// Namespace is required for, and only allowed on, a ClusterWebhookReceiver.
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace,omitempty"`
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	Key string `json:"key"`
}
