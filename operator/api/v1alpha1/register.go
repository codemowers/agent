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
	in.Worker.DeepCopyInto(&out.Worker)
	in.Access.DeepCopyInto(&out.Access)
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
	if in.ToolImages != nil {
		out.ToolImages = make([]ToolImage, len(in.ToolImages))
		copy(out.ToolImages, in.ToolImages)
	}
}

func (in *WorkerSpec) DeepCopyInto(out *WorkerSpec) {
	*out = *in
	if in.SecurityContext != nil {
		sc := new(corev1.SecurityContext)
		in.SecurityContext.DeepCopyInto(sc)
		out.SecurityContext = sc
	}
	if in.Env != nil {
		out.Env = make([]corev1.EnvVar, len(in.Env))
		for i := range in.Env {
			in.Env[i].DeepCopyInto(&out.Env[i])
		}
	}
	if in.EnvFrom != nil {
		out.EnvFrom = make([]corev1.EnvFromSource, len(in.EnvFrom))
		for i := range in.EnvFrom {
			in.EnvFrom[i].DeepCopyInto(&out.EnvFrom[i])
		}
	}
	if in.WorkspaceClaimTemplates != nil {
		out.WorkspaceClaimTemplates = make([]WorkspaceClaimTemplate, len(in.WorkspaceClaimTemplates))
		for i := range in.WorkspaceClaimTemplates {
			in.WorkspaceClaimTemplates[i].DeepCopyInto(&out.WorkspaceClaimTemplates[i])
		}
	}
	if in.Volumes != nil {
		out.Volumes = make([]AgentVolume, len(in.Volumes))
		for i := range in.Volumes {
			in.Volumes[i].DeepCopyInto(&out.Volumes[i])
		}
	}
}

func (in *WorkspaceClaimTemplate) DeepCopyInto(out *WorkspaceClaimTemplate) {
	*out = *in
	in.Spec.DeepCopyInto(&out.Spec)
}

func (in *AgentVolume) DeepCopyInto(out *AgentVolume) {
	*out = *in
	if in.ConfigMap != nil {
		cm := new(corev1.ConfigMapVolumeSource)
		in.ConfigMap.DeepCopyInto(cm)
		out.ConfigMap = cm
	}
	if in.Secret != nil {
		s := new(corev1.SecretVolumeSource)
		in.Secret.DeepCopyInto(s)
		out.Secret = s
	}
	if in.EmptyDir != nil {
		ed := in.EmptyDir.DeepCopy()
		out.EmptyDir = ed
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
