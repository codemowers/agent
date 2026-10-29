package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Mailbox defines independent inbound and outbound connections. Credentials
// are referenced by key and must never be copied into Agent or Task resources.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=".spec.address"
// +kubebuilder:printcolumn:name="IMAP",type=string,JSONPath=".status.imap.state"
// +kubebuilder:printcolumn:name="SMTP",type=string,JSONPath=".status.smtp.state"
type Mailbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MailboxSpec   `json:"spec"`
	Status            MailboxStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type MailboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Mailbox `json:"items"`
}

// +kubebuilder:validation:XValidation:rule="has(self.imap) || has(self.smtp)",message="At least one of imap or smtp is required"
// +kubebuilder:validation:XValidation:rule="!has(self.smtp) || !has(self.smtp.sentCopy) || !self.smtp.sentCopy.enabled || has(self.imap)",message="SMTP Sent copies require IMAP on the same Mailbox"
type MailboxSpec struct {
	// AgentRef pins the Mailbox to one same-namespace Agent: only its workers
	// get the Mailbox MCP, and only while RBAC also grants them send or read.
	// Without it, RBAC alone decides, so several Agents can share a Mailbox.
	// +kubebuilder:validation:MaxLength=253
	AgentRef string `json:"agentRef,omitempty"`
	// +kubebuilder:validation:MaxLength=254
	Address string `json:"address"`
	// +kubebuilder:validation:MaxLength=120
	DisplayName string    `json:"displayName,omitempty"`
	IMAP        *MailIMAP `json:"imap,omitempty"`
	SMTP        *MailSMTP `json:"smtp,omitempty"`
}
type MailSecretRef struct {
	Name        string `json:"name"`
	UsernameKey string `json:"usernameKey"`
	PasswordKey string `json:"passwordKey"`
}
type MailIMAP struct {
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`
	// +kubebuilder:default=993
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`
	// +kubebuilder:default=auto
	// +kubebuilder:validation:Enum=auto;implicit;starttls;disabled
	TLS string `json:"tls,omitempty"`
	// +kubebuilder:default=INBOX
	Folder               string        `json:"folder,omitempty"`
	SentFolder           string        `json:"sentFolder,omitempty"`
	CredentialsSecretRef MailSecretRef `json:"credentialsSecretRef"`
}
type MailSMTP struct {
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`
	// +kubebuilder:default=465
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`
	// +kubebuilder:default=auto
	// +kubebuilder:validation:Enum=auto;implicit;starttls;disabled
	TLS string `json:"tls,omitempty"`
	// +kubebuilder:validation:MaxLength=254
	FromAddress string `json:"fromAddress,omitempty"`
	// +kubebuilder:validation:MaxLength=254
	ReplyTo              string        `json:"replyTo,omitempty"`
	SentCopy             *MailSentCopy `json:"sentCopy,omitempty"`
	CredentialsSecretRef MailSecretRef `json:"credentialsSecretRef"`
}
type MailSentCopy struct {
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`
}
type MailboxStatus struct {
	IMAP *MailIMAPStatus `json:"imap,omitempty"`
	SMTP *MailSMTPStatus `json:"smtp,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
type MailIMAPStatus struct {
	// +kubebuilder:validation:Enum=pending;connected;disconnected;error
	State          string       `json:"state,omitempty"`
	Cursor         *MailCursor  `json:"cursor,omitempty"`
	LastCheckedAt  *metav1.Time `json:"lastCheckedAt,omitempty"`
	LastReceivedAt *metav1.Time `json:"lastReceivedAt,omitempty"`
}
type MailCursor struct {
	Folder string `json:"folder"`
	// +kubebuilder:validation:Minimum=0
	UIDValidity int64 `json:"uidValidity"`
	// +kubebuilder:validation:Minimum=0
	LastUID int64 `json:"lastUID"`
}
type MailSMTPStatus struct {
	// +kubebuilder:validation:Enum=pending;ready;disconnected;error
	State      string       `json:"state,omitempty"`
	LastSentAt *metav1.Time `json:"lastSentAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type MailBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MailBindingSpec   `json:"spec"`
	Status            MailBindingStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type MailBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MailBinding `json:"items"`
}
type MailBindingSpec struct {
	MailboxRef string    `json:"mailboxRef"`
	AgentRef   string    `json:"agentRef"`
	Match      MailMatch `json:"match"`
	Event      MailEvent `json:"event"`
	// +optional
	Routing MailRouting `json:"routing"`
}

// +kubebuilder:validation:XValidation:rule="has(self.fromDomain) || (has(self.headers) && size(self.headers) > 0)",message="A From domain or at least one header rule is required"
type MailMatch struct {
	// +kubebuilder:validation:MaxLength=253
	FromDomain string `json:"fromDomain,omitempty"`
	// +kubebuilder:validation:items:MaxLength=254
	// +listType=set
	ExceptFromAddresses []string `json:"exceptFromAddresses,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	Headers []MailHeaderRule `json:"headers,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="has(self.pattern) || has(self.startsWith) || has(self.notEquals)",message="Each header rule needs pattern, startsWith or notEquals"
type MailHeaderRule struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=80
	Name string `json:"name"`
	// +kubebuilder:validation:MaxLength=512
	StartsWith string `json:"startsWith,omitempty"`
	// +kubebuilder:validation:MaxLength=2048
	Pattern string `json:"pattern,omitempty"`
	// +kubebuilder:validation:MaxLength=512
	NotEquals string `json:"notEquals,omitempty"`
}
type MailEvent struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Type string `json:"type"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Actor string `json:"actor"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	Text string `json:"text"`
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Key string `json:"key"`
}
type MailRouting struct {
	Otherwise WebhookFallback `json:"otherwise,omitempty"`
}
type MailBindingStatus struct {
	ObservedGeneration int64        `json:"observedGeneration,omitempty"`
	LastReceivedAt     *metav1.Time `json:"lastReceivedAt,omitempty"`
	LastRoutedAt       *metav1.Time `json:"lastRoutedAt,omitempty"`
	// +kubebuilder:validation:MaxLength=512
	LastError string `json:"lastError,omitempty"`
}
