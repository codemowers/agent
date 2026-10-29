package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Device is one outbound device-agent connection. The bridge reads its
// referenced Secret only when RBAC grants access to that name.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Connected",type=boolean,JSONPath=".status.connected"
// +kubebuilder:printcolumn:name="Hostname",type=string,JSONPath=".status.hostname"
type Device struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DeviceSpec   `json:"spec"`
	Status            DeviceStatus `json:"status,omitempty"`
}

type DeviceSpec struct {
	// +kubebuilder:validation:MaxLength=100
	DisplayName         string             `json:"displayName,omitempty"`
	ConnectionSecretRef DeviceSecretKeyRef `json:"connectionSecretRef"`
}

type DeviceSecretKeyRef struct {
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]+$`
	Key string `json:"key"`
}

type DeviceStatus struct {
	Connected bool `json:"connected,omitempty"`
	// +kubebuilder:validation:MaxLength=100
	Hostname string `json:"hostname,omitempty"`
	// +kubebuilder:validation:MaxLength=40
	Version     string       `json:"version,omitempty"`
	ConnectedAt *metav1.Time `json:"connectedAt,omitempty"`
	LastSeenAt  *metav1.Time `json:"lastSeenAt,omitempty"`
}

// +kubebuilder:object:root=true
type DeviceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Device `json:"items"`
}
