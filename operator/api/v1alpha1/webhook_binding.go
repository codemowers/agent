package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// WebhookBinding binds one protocol to an Agent in its own namespace.
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
	ProtocolRef        string               `json:"protocolRef"`
	AgentRef           string               `json:"agentRef"`
	Match              map[string]string    `json:"match,omitempty"`
	VerificationKeyRef *WebhookSecretKeyRef `json:"verificationKeyRef,omitempty"`
	// ServiceAccounts lists the <namespace>/<name> senders accepted by a
	// serviceAccountToken protocol.
	ServiceAccounts []string       `json:"serviceAccounts,omitempty"`
	Routing         WebhookRouting `json:"routing"`
}
type WebhookSecretKeyRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}
type WebhookRouting struct {
	KeyCEL      string          `json:"keyCEL,omitempty"`
	Otherwise   WebhookFallback `json:"otherwise,omitempty"`
	CoalesceFor string          `json:"coalesceFor,omitempty"`
}
type WebhookFallback struct {
	TaskRef string `json:"taskRef,omitempty"`
}

func (in *WebhookBinding) DeepCopyObject() runtime.Object {
	out := *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec.Match = copyStringMap(in.Spec.Match)
	out.Spec.ServiceAccounts = append([]string(nil), in.Spec.ServiceAccounts...)
	if in.Spec.VerificationKeyRef != nil {
		key := *in.Spec.VerificationKeyRef
		out.Spec.VerificationKeyRef = &key
	}
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
