package v1alpha1

import (
	"time"

	corev1 "k8s.io/api/core/v1"
)

const (
	DefaultIdleSeconds     int32 = 3600
	DefaultCoalesceSeconds int32 = 180
)

// AgentIdleSeconds resolves the Agent's default for newly created Tasks.
func AgentIdleSeconds(agent *Agent) int32 {
	if agent.Spec.IdleSeconds != nil {
		return *agent.Spec.IdleSeconds
	}
	return DefaultIdleSeconds
}

// CoalesceWindow resolves a Task's event coalescing window: its own
// override, then its Agent's default.
func CoalesceWindow(task *Task, agent *Agent) time.Duration {
	seconds := DefaultCoalesceSeconds
	if task.Spec.CoalesceSeconds != nil {
		seconds = *task.Spec.CoalesceSeconds
	} else if agent.Spec.CoalesceSeconds != nil {
		seconds = *agent.Spec.CoalesceSeconds
	}
	return time.Duration(seconds) * time.Second
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
