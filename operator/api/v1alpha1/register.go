// +groupName=agents.codemowers.cloud
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
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

func (in *AgentAccess) DeepCopyInto(out *AgentAccess) {
	*out = *in
	if in.Groups != nil {
		out.Groups = make([]string, len(in.Groups))
		copy(out.Groups, in.Groups)
	}
	if in.Subjects != nil {
		out.Subjects = make([]string, len(in.Subjects))
		copy(out.Subjects, in.Subjects)
	}
}

func (in *AgentSpec) DeepCopyInto(out *AgentSpec) {
	*out = *in
	if in.Containers != nil {
		out.Containers = make([]corev1.Container, len(in.Containers))
		for i := range in.Containers {
			in.Containers[i].DeepCopyInto(&out.Containers[i])
		}
	}
	if in.InitContainers != nil {
		out.InitContainers = make([]corev1.Container, len(in.InitContainers))
		for i := range in.InitContainers {
			in.InitContainers[i].DeepCopyInto(&out.InitContainers[i])
		}
	}
	in.Access.DeepCopyInto(&out.Access)
	if in.VolumeClaimTemplates != nil {
		out.VolumeClaimTemplates = make([]corev1.PersistentVolumeClaim, len(in.VolumeClaimTemplates))
		for i := range in.VolumeClaimTemplates {
			in.VolumeClaimTemplates[i].DeepCopyInto(&out.VolumeClaimTemplates[i])
		}
	}
	if in.Volumes != nil {
		out.Volumes = make([]corev1.Volume, len(in.Volumes))
		for i := range in.Volumes {
			in.Volumes[i].DeepCopyInto(&out.Volumes[i])
		}
	}
	if in.GitProviders != nil {
		out.GitProviders = make([]GitProvider, len(in.GitProviders))
		copy(out.GitProviders, in.GitProviders)
	}
	if in.SlackWorkspaces != nil {
		out.SlackWorkspaces = make([]SlackWorkspace, len(in.SlackWorkspaces))
		copy(out.SlackWorkspaces, in.SlackWorkspaces)
	}
	if in.CredentialRefs != nil {
		out.CredentialRefs = make([]string, len(in.CredentialRefs))
		copy(out.CredentialRefs, in.CredentialRefs)
	}
	if in.MCPEndpoints != nil {
		out.MCPEndpoints = make([]MCPEndpoint, len(in.MCPEndpoints))
		copy(out.MCPEndpoints, in.MCPEndpoints)
	}
}

func (in *AgentStatus) DeepCopyInto(out *AgentStatus) {
	*out = *in
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
	if in.ExternalReferences != nil {
		out.ExternalReferences = make([]ExternalReference, len(in.ExternalReferences))
		copy(out.ExternalReferences, in.ExternalReferences)
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
