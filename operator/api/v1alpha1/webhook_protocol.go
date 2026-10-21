package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// WebhookProtocol defines a reusable, cluster-owned HTTP protocol.
type WebhookProtocol struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WebhookProtocolSpec `json:"spec"`
}
type WebhookProtocolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WebhookProtocol `json:"items"`
}
type WebhookProtocolSpec struct {
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
	// Audience is the token audience for algorithm serviceAccountToken.
	Audience string `json:"audience,omitempty"`
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
	// AllowedKeyPrefixes bounds the canonical keys that this protocol's
	// events, including binding keyCEL results, may carry.
	AllowedKeyPrefixes []string `json:"allowedKeyPrefixes"`
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

func (in *WebhookProtocol) DeepCopyObject() runtime.Object {
	out := *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec.Event.RequiredHeaders = append([]string(nil), in.Spec.Event.RequiredHeaders...)
	out.Spec.Event.AllowedKeyPrefixes = append([]string(nil), in.Spec.Event.AllowedKeyPrefixes...)
	out.Spec.Response.Rules = append([]WebhookResponseRule(nil), in.Spec.Response.Rules...)
	if in.Spec.Verification.Freshness != nil {
		v := *in.Spec.Verification.Freshness
		out.Spec.Verification.Freshness = &v
	}
	return &out
}
func (in *WebhookProtocolList) DeepCopyObject() runtime.Object {
	out := *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	out.Items = make([]WebhookProtocol, len(in.Items))
	for i := range in.Items {
		out.Items[i] = *(in.Items[i].DeepCopyObject().(*WebhookProtocol))
	}
	return &out
}
