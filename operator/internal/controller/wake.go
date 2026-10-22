package controller

import (
	"context"
	"fmt"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WakeTask records an authorized wake request on a Task and wakes it like any
// other arrival. Requests of one caller share a coalescing window; a repeated
// idempotency key of the same caller is recorded once. A completed Task
// records the request without waking. It returns the Kubernetes error for a
// missing Task.
func (c *EventConsumer) WakeTask(ctx context.Context, namespace, name, actor, key, text, sourceURL string) error {
	var task agentsv1alpha1.Task
	if err := c.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &task); err != nil {
		return err
	}
	var agent agentsv1alpha1.Agent
	if err := c.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: task.Spec.AgentRef}, &agent); err != nil {
		// A missing Agent is not a missing Task.
		return fmt.Errorf("get Task Agent: %v", err)
	}
	receipt := "wake/" + actor + "/" + key
	if key == "" {
		receipt = "wake/" + actor + "/" + randSuffix()
	}
	if taskHasDelivery(&task, receipt) {
		return nil
	}
	return c.recordArrival(ctx, &task, &agent, taskArrival{Key: "wake/" + actor, Receipt: receipt, Source: "wake",
		Actor: actor, Text: text, URL: sourceURL, Binding: actor, Wake: !task.Status.Completed()})
}
