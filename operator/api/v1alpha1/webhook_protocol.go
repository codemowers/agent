package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WebhookProtocol defines a reusable, cluster-owned HTTP protocol.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
type WebhookProtocol struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WebhookProtocolSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type WebhookProtocolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WebhookProtocol `json:"items"`
}
type WebhookProtocolSpec struct {
	// Signature describes how the sender signs requests; the key comes from
	// the receiver.
	Signature *WebhookSignature `json:"signature,omitempty"`
	Event     WebhookEventSpec  `json:"event"`
	Response  WebhookResponse   `json:"response,omitempty"`
}
type WebhookSignature struct {
	// Algorithm is hmacSha256 with the receiver's secret, or ed25519 with its
	// public key.
	// +kubebuilder:validation:Enum=hmacSha256;ed25519
	Algorithm string `json:"algorithm"`
	// +kubebuilder:validation:Enum=utf8;hex
	KeyEncoding string `json:"keyEncoding,omitempty"`
	Header      string `json:"header"`
	Prefix      string `json:"prefix,omitempty"`
	// +kubebuilder:validation:Enum=hex;base64
	Encoding string `json:"encoding"`
	// +kubebuilder:validation:MaxLength=4096
	SignedBytesCEL string            `json:"signedBytesCEL,omitempty"`
	Freshness      *WebhookFreshness `json:"freshness,omitempty"`
}
type WebhookFreshness struct {
	TimestampHeader string `json:"timestampHeader"`
	MaxSkew         string `json:"maxSkew"`
}
type WebhookEventSpec struct {
	// +kubebuilder:validation:MaxItems=32
	RequiredHeaders []string `json:"requiredHeaders,omitempty"`
	// +kubebuilder:validation:MaxLength=8192
	AcceptCEL string `json:"acceptCEL,omitempty"`
	// +kubebuilder:validation:MaxLength=16384
	EventsCEL string `json:"eventsCEL"`
	// AllowedKeyPrefixes bounds the canonical keys that this protocol's
	// events, including binding keyCEL results, may carry. Protocols whose
	// events carry no key leave it empty.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=1024
	// +listType=set
	AllowedKeyPrefixes []string `json:"allowedKeyPrefixes"`
}
type WebhookResponse struct {
	// +kubebuilder:validation:Minimum=200
	// +kubebuilder:validation:Maximum=299
	Status int `json:"status,omitempty"`
	// +kubebuilder:validation:Minimum=200
	// +kubebuilder:validation:Maximum=299
	DefaultStatus int `json:"defaultStatus,omitempty"`
	// +kubebuilder:validation:MaxItems=8
	Rules []WebhookResponseRule `json:"rules,omitempty"`
}
type WebhookResponseRule struct {
	// +kubebuilder:validation:MaxLength=4096
	WhenCEL string `json:"whenCEL"`
	// +kubebuilder:validation:Minimum=200
	// +kubebuilder:validation:Maximum=299
	Status      int    `json:"status"`
	ContentType string `json:"contentType,omitempty"`
	// +kubebuilder:validation:MaxLength=4096
	BodyCEL string `json:"bodyCEL,omitempty"`
}
