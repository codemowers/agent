package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Mailbox defines independent inbound and outbound connections. Credentials
// are referenced by key and must never be copied into Agent or Task resources.
type Mailbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MailboxSpec   `json:"spec"`
	Status            MailboxStatus `json:"status,omitempty"`
}
type MailboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Mailbox `json:"items"`
}
type MailboxSpec struct {
	Address     string    `json:"address"`
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
	Host                 string        `json:"host"`
	Port                 int32         `json:"port,omitempty"`
	TLS                  string        `json:"tls,omitempty"`
	Folder               string        `json:"folder,omitempty"`
	SentFolder           string        `json:"sentFolder,omitempty"`
	CredentialsSecretRef MailSecretRef `json:"credentialsSecretRef"`
}
type MailSMTP struct {
	Host                 string        `json:"host"`
	Port                 int32         `json:"port,omitempty"`
	TLS                  string        `json:"tls,omitempty"`
	FromAddress          string        `json:"fromAddress,omitempty"`
	ReplyTo              string        `json:"replyTo,omitempty"`
	SentCopy             *MailSentCopy `json:"sentCopy,omitempty"`
	CredentialsSecretRef MailSecretRef `json:"credentialsSecretRef"`
}
type MailSentCopy struct {
	Enabled bool `json:"enabled,omitempty"`
}
type MailboxStatus struct {
	IMAP       *MailIMAPStatus    `json:"imap,omitempty"`
	SMTP       *MailSMTPStatus    `json:"smtp,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
type MailIMAPStatus struct {
	State          string       `json:"state,omitempty"`
	Cursor         *MailCursor  `json:"cursor,omitempty"`
	LastCheckedAt  *metav1.Time `json:"lastCheckedAt,omitempty"`
	LastReceivedAt *metav1.Time `json:"lastReceivedAt,omitempty"`
}
type MailCursor struct {
	Folder      string `json:"folder"`
	UIDValidity int64  `json:"uidValidity"`
	LastUID     int64  `json:"lastUID"`
}
type MailSMTPStatus struct {
	State      string       `json:"state,omitempty"`
	LastSentAt *metav1.Time `json:"lastSentAt,omitempty"`
}

type MailSubscription struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MailSubscriptionSpec   `json:"spec"`
	Status            MailSubscriptionStatus `json:"status,omitempty"`
}
type MailSubscriptionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MailSubscription `json:"items"`
}
type MailSubscriptionSpec struct {
	MailboxRef string      `json:"mailboxRef"`
	AgentRef   string      `json:"agentRef"`
	Match      MailMatch   `json:"match"`
	Event      MailEvent   `json:"event"`
	Routing    MailRouting `json:"routing"`
}
type MailMatch struct {
	FromDomain          string           `json:"fromDomain,omitempty"`
	ExceptFromAddresses []string         `json:"exceptFromAddresses,omitempty"`
	Headers             []MailHeaderRule `json:"headers,omitempty"`
}
type MailHeaderRule struct {
	Name       string `json:"name"`
	StartsWith string `json:"startsWith,omitempty"`
	Pattern    string `json:"pattern,omitempty"`
	NotEquals  string `json:"notEquals,omitempty"`
}
type MailEvent struct {
	Type  string   `json:"type"`
	Actor string   `json:"actor"`
	Text  string   `json:"text"`
	URL   string   `json:"url,omitempty"`
	Key   []string `json:"key"`
}
type MailRouting struct {
	ExistingTask string          `json:"existingTask"`
	Otherwise    WebhookFallback `json:"otherwise,omitempty"`
	CoalesceFor  string          `json:"coalesceFor,omitempty"`
}
type MailSubscriptionStatus struct {
	ObservedGeneration int64        `json:"observedGeneration,omitempty"`
	LastReceivedAt     *metav1.Time `json:"lastReceivedAt,omitempty"`
	LastRoutedAt       *metav1.Time `json:"lastRoutedAt,omitempty"`
	LastError          string       `json:"lastError,omitempty"`
}

func (in *Mailbox) DeepCopyObject() runtime.Object {
	out := *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	if in.Spec.IMAP != nil {
		v := *in.Spec.IMAP
		out.Spec.IMAP = &v
	}
	if in.Spec.SMTP != nil {
		v := *in.Spec.SMTP
		if v.SentCopy != nil {
			c := *v.SentCopy
			v.SentCopy = &c
		}
		out.Spec.SMTP = &v
	}
	if in.Status.IMAP != nil {
		v := *in.Status.IMAP
		if v.Cursor != nil {
			c := *v.Cursor
			v.Cursor = &c
		}
		if v.LastCheckedAt != nil {
			v.LastCheckedAt = v.LastCheckedAt.DeepCopy()
		}
		if v.LastReceivedAt != nil {
			v.LastReceivedAt = v.LastReceivedAt.DeepCopy()
		}
		out.Status.IMAP = &v
	}
	if in.Status.SMTP != nil {
		v := *in.Status.SMTP
		if v.LastSentAt != nil {
			v.LastSentAt = v.LastSentAt.DeepCopy()
		}
		out.Status.SMTP = &v
	}
	out.Status.Conditions = append([]metav1.Condition(nil), in.Status.Conditions...)
	return &out
}
func (in *MailboxList) DeepCopyObject() runtime.Object {
	out := *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	out.Items = make([]Mailbox, len(in.Items))
	for i := range in.Items {
		out.Items[i] = *(in.Items[i].DeepCopyObject().(*Mailbox))
	}
	return &out
}
func (in *MailSubscription) DeepCopyObject() runtime.Object {
	out := *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec.Match.ExceptFromAddresses = append([]string(nil), in.Spec.Match.ExceptFromAddresses...)
	out.Spec.Match.Headers = append([]MailHeaderRule(nil), in.Spec.Match.Headers...)
	out.Spec.Event.Key = append([]string(nil), in.Spec.Event.Key...)
	if in.Status.LastReceivedAt != nil {
		out.Status.LastReceivedAt = in.Status.LastReceivedAt.DeepCopy()
	}
	if in.Status.LastRoutedAt != nil {
		out.Status.LastRoutedAt = in.Status.LastRoutedAt.DeepCopy()
	}
	return &out
}
func (in *MailSubscriptionList) DeepCopyObject() runtime.Object {
	out := *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	out.Items = make([]MailSubscription, len(in.Items))
	for i := range in.Items {
		out.Items[i] = *(in.Items[i].DeepCopyObject().(*MailSubscription))
	}
	return &out
}
