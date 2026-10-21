package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// WebhookReceiver is a namespaced endpoint at /hooks/<namespace>/<name> for
// WebhookBindings in its namespace.
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

// ClusterWebhookReceiver is an endpoint at /hooks/<name> that fans each
// event out to the WebhookBindings of any namespace referencing it.
type ClusterWebhookReceiver struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WebhookReceiverSpec `json:"spec"`
}
type ClusterWebhookReceiverList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterWebhookReceiver `json:"items"`
}

type WebhookReceiverSpec struct {
	ProtocolRef string `json:"protocolRef"`
	// Match holds settings the protocol's CEL reads as receiver.match, such
	// as serverURL.
	Match   map[string]string `json:"match,omitempty"`
	Senders WebhookSenders    `json:"senders"`
}

// WebhookSenders lists how a receiver authenticates deliveries: a request
// must pass the secret signature, a listed ServiceAccount's token or the
// Ed25519 signature, and also come from an allowed address when GitHubMeta or
// CIDRs restrict it.
type WebhookSenders struct {
	// SecretKeyRef selects the signing key: a Secret in the receiver's
	// namespace, or in the named namespace for a ClusterWebhookReceiver.
	SecretKeyRef *WebhookSecretKeyRef `json:"secretKeyRef,omitempty"`
	// GitHubMeta is a GitHub or GHES meta API URL; deliveries must come from
	// its hooks address ranges.
	GitHubMeta string `json:"githubMeta,omitempty"`
	// CIDRs restrict deliveries to these client address ranges.
	CIDRs []string `json:"cidrs,omitempty"`
	// ServiceAccounts are <namespace>/<name> senders presenting a projected
	// token for Audience.
	ServiceAccounts []string `json:"serviceAccounts,omitempty"`
	Audience        string   `json:"audience,omitempty"`
	// Ed25519PublicKey is the hex public key for a protocol signature.
	Ed25519PublicKey string `json:"ed25519PublicKey,omitempty"`
}

type WebhookSecretKeyRef struct {
	// Namespace is required for, and only allowed on, a ClusterWebhookReceiver.
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Key       string `json:"key"`
}

func (in WebhookReceiverSpec) deepCopy() WebhookReceiverSpec {
	out := in
	if in.Senders.SecretKeyRef != nil {
		ref := *in.Senders.SecretKeyRef
		out.Senders.SecretKeyRef = &ref
	}
	out.Match = copyStringMap(in.Match)
	out.Senders.CIDRs = append([]string(nil), in.Senders.CIDRs...)
	out.Senders.ServiceAccounts = append([]string(nil), in.Senders.ServiceAccounts...)
	return out
}

func (in *WebhookReceiver) DeepCopyObject() runtime.Object {
	out := *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec.deepCopy()
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
func (in *ClusterWebhookReceiver) DeepCopyObject() runtime.Object {
	out := *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec.deepCopy()
	return &out
}
func (in *ClusterWebhookReceiverList) DeepCopyObject() runtime.Object {
	out := *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	out.Items = make([]ClusterWebhookReceiver, len(in.Items))
	for i := range in.Items {
		out.Items[i] = *(in.Items[i].DeepCopyObject().(*ClusterWebhookReceiver))
	}
	return &out
}
