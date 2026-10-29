package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WebhookBinding subscribes an Agent in its namespace to a receiver's events.
// +kubebuilder:object:root=true
type WebhookBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WebhookBindingSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type WebhookBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WebhookBinding `json:"items"`
}

// +kubebuilder:validation:XValidation:rule="self.receiverRef.kind != 'ClusterWebhookReceiver' || has(self.keyPrefixes)",message="bindings to a ClusterWebhookReceiver need keyPrefixes"
type WebhookBindingSpec struct {
	ReceiverRef WebhookReceiverRef `json:"receiverRef"`
	AgentRef    string             `json:"agentRef"`
	// KeyPrefixes limits the binding to events whose key starts with one of
	// them at a path boundary; required for a ClusterWebhookReceiver.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=1024
	// +listType=set
	KeyPrefixes []string       `json:"keyPrefixes,omitempty"`
	Routing     WebhookRouting `json:"routing,omitempty"`
}
type WebhookReceiverRef struct {
	// Kind is WebhookReceiver (same namespace) or ClusterWebhookReceiver.
	// +kubebuilder:validation:Enum=WebhookReceiver;ClusterWebhookReceiver
	Kind string `json:"kind"`
	Name string `json:"name"`
}
type WebhookRouting struct {
	// +kubebuilder:validation:MaxLength=8192
	KeyCEL    string          `json:"keyCEL,omitempty"`
	Otherwise WebhookFallback `json:"otherwise,omitempty"`
}
type WebhookFallback struct {
	TaskRef string `json:"taskRef,omitempty"`
}
