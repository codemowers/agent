package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TaskPolicy authorizes creating a Task for an authenticated event whose
// canonical key no Task claims. It applies to webhook and mail events alike.
// +kubebuilder:object:root=true
type TaskPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              TaskPolicySpec `json:"spec"`
}

// +kubebuilder:object:root=true
type TaskPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TaskPolicy `json:"items"`
}
type TaskPolicySpec struct {
	AgentRef string `json:"agentRef"`
	// KeyPrefix matches canonical keys at a path boundary. The longest
	// matching prefix wins; equally long matches are ambiguous and create nothing.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	KeyPrefix string `json:"keyPrefix"`
	// EventTypes limits creation to these event types; empty allows all.
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=128
	// +listType=set
	EventTypes []string `json:"eventTypes,omitempty"`
	// Verify is "forgeAssignment" to require an open forge issue or pull
	// request assigned to the Agent's account, or "none".
	// +kubebuilder:validation:Enum=forgeAssignment;none
	Verify string `json:"verify"`
	// CoalesceSeconds is copied into the Tasks this policy creates.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=86400
	CoalesceSeconds *int32 `json:"coalesceSeconds,omitempty"`
}
