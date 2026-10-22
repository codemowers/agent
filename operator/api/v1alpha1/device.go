package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Device is one outbound device-agent connection. The bridge reads its
// referenced Secret only when RBAC grants access to that name.
type Device struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DeviceSpec   `json:"spec"`
	Status            DeviceStatus `json:"status,omitempty"`
}

type DeviceSpec struct {
	DisplayName         string             `json:"displayName,omitempty"`
	ConnectionSecretRef DeviceSecretKeyRef `json:"connectionSecretRef"`
}

type DeviceSecretKeyRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

type DeviceStatus struct {
	Connected   bool         `json:"connected,omitempty"`
	Hostname    string       `json:"hostname,omitempty"`
	Version     string       `json:"version,omitempty"`
	ConnectedAt *metav1.Time `json:"connectedAt,omitempty"`
	LastSeenAt  *metav1.Time `json:"lastSeenAt,omitempty"`
}

type DeviceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Device `json:"items"`
}

func (in *Device) DeepCopyInto(out *Device) {
	*out = *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	if in.Status.ConnectedAt != nil {
		out.Status.ConnectedAt = in.Status.ConnectedAt.DeepCopy()
	}
	if in.Status.LastSeenAt != nil {
		out.Status.LastSeenAt = in.Status.LastSeenAt.DeepCopy()
	}
}

func (in *Device) DeepCopy() *Device {
	if in == nil {
		return nil
	}
	out := new(Device)
	in.DeepCopyInto(out)
	return out
}

func (in *Device) DeepCopyObject() runtime.Object { return in.DeepCopy() }

func (in *DeviceList) DeepCopyInto(out *DeviceList) {
	*out = *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Device, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *DeviceList) DeepCopy() *DeviceList {
	if in == nil {
		return nil
	}
	out := new(DeviceList)
	in.DeepCopyInto(out)
	return out
}

func (in *DeviceList) DeepCopyObject() runtime.Object { return in.DeepCopy() }
