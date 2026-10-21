// +groupName=codemowers.cloud
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
	SchemeBuilder.Register(&WebhookBinding{}, &WebhookBindingList{})
	SchemeBuilder.Register(&TaskPolicy{}, &TaskPolicyList{})
	SchemeBuilder.Register(&Mailbox{}, &MailboxList{})
	SchemeBuilder.Register(&MailBinding{}, &MailBindingList{})
	SchemeBuilder.Register(&Device{}, &DeviceList{})
}

// --- Agent ---

func (in *Agent) DeepCopy() *Agent {
	if in == nil {
		return nil
	}
	out := new(Agent)
	in.DeepCopyInto(out)
	return out
}

func (in *Agent) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *Agent) DeepCopyInto(out *Agent) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *AgentSpec) DeepCopyInto(out *AgentSpec) {
	*out = *in
	if in.IdleSeconds != nil {
		value := *in.IdleSeconds
		out.IdleSeconds = &value
	}
	if in.Resources != nil {
		out.Resources = in.Resources.DeepCopy()
	}
	in.Template.DeepCopyInto(&out.Template)
	if in.VolumeClaimTemplates != nil {
		out.VolumeClaimTemplates = make([]VolumeClaimTemplate, len(in.VolumeClaimTemplates))
		for i := range in.VolumeClaimTemplates {
			out.VolumeClaimTemplates[i].Metadata = in.VolumeClaimTemplates[i].Metadata
			out.VolumeClaimTemplates[i].Metadata.Labels = copyStringMap(in.VolumeClaimTemplates[i].Metadata.Labels)
			out.VolumeClaimTemplates[i].Metadata.Annotations = copyStringMap(in.VolumeClaimTemplates[i].Metadata.Annotations)
			in.VolumeClaimTemplates[i].Spec.DeepCopyInto(&out.VolumeClaimTemplates[i].Spec)
		}
	}
	if in.GitProviders != nil {
		out.GitProviders = make([]GitProvider, len(in.GitProviders))
		copy(out.GitProviders, in.GitProviders)
	}
	if in.MCPEndpoints != nil {
		out.MCPEndpoints = make([]MCPEndpoint, len(in.MCPEndpoints))
		copy(out.MCPEndpoints, in.MCPEndpoints)
	}
}

func copyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func (in *AgentStatus) DeepCopyInto(out *AgentStatus) {
	*out = *in
	if in.ForgesCheckedAt != nil {
		out.ForgesCheckedAt = in.ForgesCheckedAt.DeepCopy()
	}
	if in.Forges != nil {
		out.Forges = make([]ForgeCapability, len(in.Forges))
		for i := range in.Forges {
			out.Forges[i] = in.Forges[i]
			out.Forges[i].Repositories = append([]ForgeRepository(nil), in.Forges[i].Repositories...)
			out.Forges[i].Destinations = append([]ForgeDestination(nil), in.Forges[i].Destinations...)
		}
	}
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		copy(out.Conditions, in.Conditions)
	}
}

// --- AgentList ---

func (in *AgentList) DeepCopy() *AgentList {
	if in == nil {
		return nil
	}
	out := new(AgentList)
	in.DeepCopyInto(out)
	return out
}

func (in *AgentList) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *AgentList) DeepCopyInto(out *AgentList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Agent, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// --- Task ---

func (in *Task) DeepCopy() *Task {
	if in == nil {
		return nil
	}
	out := new(Task)
	in.DeepCopyInto(out)
	return out
}

func (in *Task) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *Task) DeepCopyInto(out *Task) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *TaskSpec) DeepCopyInto(out *TaskSpec) {
	*out = *in
	if in.IdleSeconds != nil {
		value := *in.IdleSeconds
		out.IdleSeconds = &value
	}
	if in.Resources != nil {
		out.Resources = in.Resources.DeepCopy()
	}
	if in.DisabledMCPs != nil {
		out.DisabledMCPs = append([]string(nil), in.DisabledMCPs...)
	}
	if in.RoutingKeys != nil {
		out.RoutingKeys = append([]string(nil), in.RoutingKeys...)
	}
	if in.ExternalReferences != nil {
		out.ExternalReferences = append([]ExternalReference(nil), in.ExternalReferences...)
	}
	if in.CompletionPolicy != nil {
		cp := *in.CompletionPolicy
		if in.CompletionPolicy.RequiredChecks != nil {
			cp.RequiredChecks = make([]string, len(in.CompletionPolicy.RequiredChecks))
			copy(cp.RequiredChecks, in.CompletionPolicy.RequiredChecks)
		}
		out.CompletionPolicy = &cp
	}
	if in.Model != nil {
		m := *in.Model
		out.Model = &m
	}
}

func (in *TaskStatus) DeepCopyInto(out *TaskStatus) {
	*out = *in
	if in.MCPProbe != nil {
		probe := *in.MCPProbe
		probe.Endpoints = append([]MCPEndpointStatus(nil), in.MCPProbe.Endpoints...)
		for i := range probe.Endpoints {
			probe.Endpoints[i].Tools = append([]string(nil), probe.Endpoints[i].Tools...)
		}
		if in.MCPProbe.CheckedAt != nil {
			checked := *in.MCPProbe.CheckedAt
			probe.CheckedAt = &checked
		}
		out.MCPProbe = &probe
	}
	if in.ExternalReferences != nil {
		out.ExternalReferences = append([]ExternalReference(nil), in.ExternalReferences...)
	}
	if in.Conversation != nil {
		out.Conversation = make([]TaskMessage, len(in.Conversation))
		copy(out.Conversation, in.Conversation)
		for i := range in.Conversation {
			if metrics := in.Conversation[i].Metrics; metrics != nil {
				copied := *metrics
				copyCount := func(value *int64) *int64 {
					if value == nil {
						return nil
					}
					copy := *value
					return &copy
				}
				copied.DurationMs = copyCount(metrics.DurationMs)
				copied.APIDurationMs = copyCount(metrics.APIDurationMs)
				copied.InputTokens = copyCount(metrics.InputTokens)
				copied.OutputTokens = copyCount(metrics.OutputTokens)
				copied.CacheReadTokens = copyCount(metrics.CacheReadTokens)
				copied.CacheWriteTokens = copyCount(metrics.CacheWriteTokens)
				out.Conversation[i].Metrics = &copied
			}
		}
	}
	if in.LastResultAt != nil {
		value := *in.LastResultAt
		out.LastResultAt = &value
	}
	if in.LastMessageAt != nil {
		value := *in.LastMessageAt
		out.LastMessageAt = &value
	}
	if in.WorkspacePVCs != nil {
		out.WorkspacePVCs = make(map[string]string, len(in.WorkspacePVCs))
		for k, v := range in.WorkspacePVCs {
			out.WorkspacePVCs[k] = v
		}
	}
	if in.Progress != nil {
		p := *in.Progress
		out.Progress = &p
	}
	if in.Outputs != nil {
		out.Outputs = make(map[string]string, len(in.Outputs))
		for k, v := range in.Outputs {
			out.Outputs[k] = v
		}
	}
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		copy(out.Conditions, in.Conditions)
	}
}

// --- TaskList ---

func (in *TaskList) DeepCopy() *TaskList {
	if in == nil {
		return nil
	}
	out := new(TaskList)
	in.DeepCopyInto(out)
	return out
}

func (in *TaskList) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *TaskList) DeepCopyInto(out *TaskList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Task, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}
