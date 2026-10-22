package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TaskPolicy authorizes creating a Task for an authenticated event whose
// canonical key no Task claims. It applies to webhook and mail events alike.
type TaskPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              TaskPolicySpec `json:"spec"`
}
type TaskPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TaskPolicy `json:"items"`
}
type TaskPolicySpec struct {
	AgentRef string `json:"agentRef"`
	// KeyPrefix matches canonical keys at a path boundary. The longest
	// matching prefix wins; equally long matches are ambiguous and create nothing.
	KeyPrefix string `json:"keyPrefix"`
	// EventTypes limits creation to these event types; empty allows all.
	EventTypes []string `json:"eventTypes,omitempty"`
	// Verify is "forgeAssignment" to require an open forge issue or pull
	// request assigned to the Agent's account, or "none".
	Verify string `json:"verify"`
	// CoalesceSeconds is copied into the Tasks this policy creates.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=86400
	CoalesceSeconds *int32 `json:"coalesceSeconds,omitempty"`
}

func (in *TaskPolicy) DeepCopyObject() runtime.Object {
	out := *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec.EventTypes = append([]string(nil), in.Spec.EventTypes...)
	if in.Spec.CoalesceSeconds != nil {
		value := *in.Spec.CoalesceSeconds
		out.Spec.CoalesceSeconds = &value
	}
	return &out
}
func (in *TaskPolicyList) DeepCopyObject() runtime.Object {
	out := *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	out.Items = make([]TaskPolicy, len(in.Items))
	for i := range in.Items {
		out.Items[i] = *(in.Items[i].DeepCopyObject().(*TaskPolicy))
	}
	return &out
}
