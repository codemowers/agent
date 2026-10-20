package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// WebhookReceiver binds one protocol to an Agent in its own namespace.
type WebhookReceiver struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WebhookReceiverSpec `json:"spec"`
}
type WebhookReceiverList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WebhookReceiver `json:"items"`
}
type WebhookReceiverSpec struct {
	SourceRef          WebhookSourceRef     `json:"sourceRef"`
	AgentRef           string               `json:"agentRef"`
	Match              map[string]string    `json:"match,omitempty"`
	VerificationKeyRef *WebhookSecretKeyRef `json:"verificationKeyRef,omitempty"`
	Routing            WebhookRouting       `json:"routing"`
}
type WebhookSourceRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}
type WebhookSecretKeyRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}
type WebhookRouting struct {
	KeyCEL       string          `json:"keyCEL,omitempty"`
	ExistingTask string          `json:"existingTask,omitempty"`
	Otherwise    WebhookFallback `json:"otherwise,omitempty"`
	CoalesceFor  string          `json:"coalesceFor,omitempty"`
}
type WebhookFallback struct {
	TaskRef string `json:"taskRef,omitempty"`
}

func (in *WebhookReceiver) DeepCopyObject() runtime.Object {
	out := *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec.Match = copyStringMap(in.Spec.Match)
	if in.Spec.VerificationKeyRef != nil {
		key := *in.Spec.VerificationKeyRef
		out.Spec.VerificationKeyRef = &key
	}
	return &out
}
func (in *WebhookReceiverList) DeepCopyObject() runtime.Object {
	out := *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	out.Items = make([]WebhookReceiver, len(in.Items))
	for i := range in.Items {
		out.Items[i] = *(in.Items[i].DeepCopyObject().(*WebhookReceiver))
	}
	return &out
}
