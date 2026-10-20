package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ClusterWebhookReceiver defines a reusable, cluster-owned HTTP protocol.
type ClusterWebhookReceiver struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ClusterWebhookReceiverSpec `json:"spec"`
}
type ClusterWebhookReceiverList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterWebhookReceiver `json:"items"`
}
type ClusterWebhookReceiverSpec struct {
	Verification WebhookVerification `json:"verification"`
	Event        WebhookEventSpec    `json:"event"`
	Response     WebhookResponse     `json:"response,omitempty"`
}
type WebhookVerification struct {
	Algorithm      string            `json:"algorithm"`
	KeyEncoding    string            `json:"keyEncoding,omitempty"`
	Signature      WebhookSignature  `json:"signature,omitempty"`
	SignedBytesCEL string            `json:"signedBytesCEL,omitempty"`
	Freshness      *WebhookFreshness `json:"freshness,omitempty"`
}
type WebhookSignature struct {
	Header   string `json:"header,omitempty"`
	Prefix   string `json:"prefix,omitempty"`
	Encoding string `json:"encoding,omitempty"`
}
type WebhookFreshness struct {
	TimestampHeader string `json:"timestampHeader"`
	MaxSkew         string `json:"maxSkew"`
}
type WebhookEventSpec struct {
	RequiredHeaders []string `json:"requiredHeaders,omitempty"`
	AcceptCEL       string   `json:"acceptCEL,omitempty"`
	EventsCEL       string   `json:"eventsCEL"`
}
type WebhookResponse struct {
	Status        int                   `json:"status,omitempty"`
	DefaultStatus int                   `json:"defaultStatus,omitempty"`
	Rules         []WebhookResponseRule `json:"rules,omitempty"`
}
type WebhookResponseRule struct {
	WhenCEL     string `json:"whenCEL"`
	Status      int    `json:"status"`
	ContentType string `json:"contentType,omitempty"`
	BodyCEL     string `json:"bodyCEL,omitempty"`
}

func (in *ClusterWebhookReceiver) DeepCopyObject() runtime.Object {
	out := *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec.Event.RequiredHeaders = append([]string(nil), in.Spec.Event.RequiredHeaders...)
	out.Spec.Response.Rules = append([]WebhookResponseRule(nil), in.Spec.Response.Rules...)
	if in.Spec.Verification.Freshness != nil {
		v := *in.Spec.Verification.Freshness
		out.Spec.Verification.Freshness = &v
	}
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
