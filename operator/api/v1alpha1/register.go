// +groupName=codemowers.ai
// +kubebuilder:object:generate=true
package v1alpha1

// CRDs and DeepCopy methods are generated from these types and their markers;
// edit the types, never chart/crds/ or zz_generated.deepcopy.go.
//go:generate go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.23.0 object crd:generateEmbeddedObjectMeta=true paths=. output:crd:dir=../../../chart/crds

import (
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(&Agent{}, &AgentList{})
	SchemeBuilder.Register(&Task{}, &TaskList{})
	SchemeBuilder.Register(&WebhookProtocol{}, &WebhookProtocolList{})
	SchemeBuilder.Register(&WebhookReceiver{}, &WebhookReceiverList{})
	SchemeBuilder.Register(&ClusterWebhookReceiver{}, &ClusterWebhookReceiverList{})
	SchemeBuilder.Register(&WebhookBinding{}, &WebhookBindingList{})
	SchemeBuilder.Register(&TaskPolicy{}, &TaskPolicyList{})
	SchemeBuilder.Register(&Mailbox{}, &MailboxList{})
	SchemeBuilder.Register(&MailBinding{}, &MailBindingList{})
	SchemeBuilder.Register(&Device{}, &DeviceList{})
	SchemeBuilder.Register(&Feed{}, &FeedList{})
	SchemeBuilder.Register(&FeedBinding{}, &FeedBindingList{})
	SchemeBuilder.Register(&ClusterMCPServer{}, &ClusterMCPServerList{})
	SchemeBuilder.Register(&MCPServer{}, &MCPServerList{})
}
