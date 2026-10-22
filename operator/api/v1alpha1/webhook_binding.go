package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// WebhookBinding subscribes an Agent in its namespace to a receiver's events.
type WebhookBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WebhookBindingSpec `json:"spec"`
}
type WebhookBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WebhookBinding `json:"items"`
}
type WebhookBindingSpec struct {
	ReceiverRef WebhookReceiverRef `json:"receiverRef"`
	AgentRef    string             `json:"agentRef"`
	// KeyPrefixes limits the binding to events whose key starts with one of
	// them at a path boundary; required for a ClusterWebhookReceiver.
	KeyPrefixes []string       `json:"keyPrefixes,omitempty"`
	Routing     WebhookRouting `json:"routing,omitempty"`
}
type WebhookReceiverRef struct {
	// Kind is WebhookReceiver (same namespace) or ClusterWebhookReceiver.
	Kind string `json:"kind"`
	Name string `json:"name"`
}
type WebhookRouting struct {
	KeyCEL    string          `json:"keyCEL,omitempty"`
	Otherwise WebhookFallback `json:"otherwise,omitempty"`
}
type WebhookFallback struct {
	TaskRef string `json:"taskRef,omitempty"`
}

func (in *WebhookBinding) DeepCopyObject() runtime.Object {
	out := *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec.KeyPrefixes = append([]string(nil), in.Spec.KeyPrefixes...)
	return &out
}
func (in *WebhookBindingList) DeepCopyObject() runtime.Object {
	out := *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	out.Items = make([]WebhookBinding, len(in.Items))
	for i := range in.Items {
		out.Items[i] = *(in.Items[i].DeepCopyObject().(*WebhookBinding))
	}
	return &out
}
