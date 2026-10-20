package v1alpha1

import corev1 "k8s.io/api/core/v1"

const DefaultIdleSeconds int32 = 3600

// AgentIdleSeconds resolves the Agent's default for newly created Tasks.
func AgentIdleSeconds(agent *Agent) int32 {
	if agent.Spec.IdleSeconds != nil {
		return *agent.Spec.IdleSeconds
	}
	return DefaultIdleSeconds
}

// AgentWorkerResources captures the worker container's resource budget for a
// new Task. The dedicated Agent field takes precedence over the Pod template.
func AgentWorkerResources(agent *Agent) *corev1.ResourceRequirements {
	if agent.Spec.Resources != nil {
		return agent.Spec.Resources.DeepCopy()
	}
	for _, container := range agent.Spec.Template.Spec.Containers {
		if container.Name == "worker" {
			return container.Resources.DeepCopy()
		}
	}
	return &corev1.ResourceRequirements{}
}
