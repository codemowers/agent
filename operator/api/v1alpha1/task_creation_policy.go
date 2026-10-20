package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TaskCreationPolicy authorizes creation from an unmatched, authenticated key.
// It is namespace-scoped and bound to exactly one Agent and receiver.
type TaskCreationPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              TaskCreationPolicySpec `json:"spec"`
}
type TaskCreationPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TaskCreationPolicy `json:"items"`
}
type TaskCreationPolicySpec struct {
	AgentRef    string   `json:"agentRef"`
	ReceiverRef string   `json:"receiverRef"`
	KeyPrefix   []string `json:"keyPrefix"`
	Check       string   `json:"check"`
}

func (in *TaskCreationPolicy) DeepCopyObject() runtime.Object {
	out := *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec.KeyPrefix = append([]string(nil), in.Spec.KeyPrefix...)
	return &out
}
func (in *TaskCreationPolicyList) DeepCopyObject() runtime.Object {
	out := *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	out.Items = make([]TaskCreationPolicy, len(in.Items))
	for i := range in.Items {
		out.Items[i] = *(in.Items[i].DeepCopyObject().(*TaskCreationPolicy))
	}
	return &out
}
