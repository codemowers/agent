// Package controller implements the Agent and Task reconcilers.
package controller

import (
	"context"
	"crypto/rand"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"github.com/codemowers/agent/operator/internal/rabbitmq"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
)

// AgentReconciler reconciles Agent CRs: counts owned Tasks and updates status.
type AgentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *AgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var agent agentsv1alpha1.Agent
	if err := r.Get(ctx, req.NamespacedName, &agent); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	var taskList agentsv1alpha1.TaskList
	if err := r.List(ctx, &taskList,
		client.InNamespace(agent.Namespace),
		client.MatchingLabels{"codemowers.cloud/agent": agent.Name},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("list tasks: %w", err)
	}

	if err := r.reconcileAccessBinding(ctx, &agent); err != nil {
		return ctrl.Result{}, err
	}

	patch := client.MergeFrom(agent.DeepCopy())
	agent.Status.TaskCount = len(taskList.Items)
	if err := r.Status().Patch(ctx, &agent, patch); err != nil {
		return ctrl.Result{}, err
	}

	log.FromContext(ctx).Info("reconciled agent", "tasks", agent.Status.TaskCount)
	return ctrl.Result{}, nil
}

// reconcileAccessBinding creates or updates a RoleBinding that grants the principals
// listed in spec.access manage access to this Agent and its Tasks.
func (r *AgentReconciler) reconcileAccessBinding(ctx context.Context, agent *agentsv1alpha1.Agent) error {
	bindingName := "agent-access-" + agent.Name
	subjects := accessSubjects(agent.Spec.Access)

	var existing rbacv1.RoleBinding
	err := r.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: bindingName}, &existing)

	if errors.IsNotFound(err) {
		if len(subjects) == 0 {
			return nil // nothing to create
		}
		rb := r.buildAccessBinding(bindingName, agent, subjects)
		return r.Create(ctx, rb)
	}
	if err != nil {
		return fmt.Errorf("get access binding: %w", err)
	}

	// Update subjects if they changed.
	if len(subjects) == 0 {
		return r.Delete(ctx, &existing)
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Subjects = subjects
	return r.Patch(ctx, &existing, patch)
}

func (r *AgentReconciler) buildAccessBinding(name string, agent *agentsv1alpha1.Agent, subjects []rbacv1.Subject) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: agent.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         agentsv1alpha1.GroupVersion.String(),
				Kind:               "Agent",
				Name:               agent.Name,
				UID:                agent.UID,
				BlockOwnerDeletion: boolPtr(true),
				Controller:         boolPtr(true),
			}},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     "agent-manager",
		},
		Subjects: subjects,
	}
}

// accessSubjects converts AgentAccess into RBAC subject entries.
// Groups use the OIDC group format from Passmower (e.g. "github.com:codemowers:admins").
// Subjects are individual user emails, matched as User kind.
func accessSubjects(access agentsv1alpha1.AgentAccess) []rbacv1.Subject {
	var subjects []rbacv1.Subject
	for _, g := range access.Groups {
		subjects = append(subjects, rbacv1.Subject{
			Kind:     rbacv1.GroupKind,
			APIGroup: rbacv1.GroupName,
			Name:     g,
		})
	}
	for _, s := range access.Subjects {
		subjects = append(subjects, rbacv1.Subject{
			Kind:     rbacv1.UserKind,
			APIGroup: rbacv1.GroupName,
			Name:     s,
		})
	}
	return subjects
}

func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.Agent{}).
		Owns(&agentsv1alpha1.Task{}).
		Owns(&rbacv1.RoleBinding{}).
		Complete(r)
}

// TaskReconciler reconciles Task CRs: manages worker Pod and workspace PVC lifecycle.
type TaskReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	AMQP    AMQPManager // nil when RabbitMQ is not configured
	AMQPURL string      // injected into worker pods as AMQP_URL
}

func (r *TaskReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var task agentsv1alpha1.Task
	if err := r.Get(ctx, req.NamespacedName, &task); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Resolve owning Agent.
	agent, err := r.ownerAgent(ctx, &task)
	if err != nil {
		return ctrl.Result{}, err
	}

	if task.Spec.Suspend {
		return r.reconcileSuspended(ctx, &task)
	}

	// Ensure one PVC per VolumeClaimTemplate entry.
	if len(agent.Spec.VolumeClaimTemplates) > 0 {
		if err := r.ensureWorkspacePVCs(ctx, &task, agent); err != nil {
			return ctrl.Result{}, err
		}
	}

	return r.reconcileWorkerPod(ctx, &task, agent)
}

func (r *TaskReconciler) reconcileSuspended(ctx context.Context, task *agentsv1alpha1.Task) (ctrl.Result, error) {
	// Delete running worker pod if any.
	if task.Status.WorkerPodName != "" {
		var pod corev1.Pod
		err := r.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: task.Status.WorkerPodName}, &pod)
		if err == nil {
			if err := r.Delete(ctx, &pod); err != nil && !errors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
	}
	return r.setPhase(ctx, task, agentsv1alpha1.TaskPhasePaused, "Suspended")
}

func (r *TaskReconciler) ensureWorkspacePVCs(ctx context.Context, task *agentsv1alpha1.Task, agent *agentsv1alpha1.Agent) error {
	if task.Status.WorkspacePVCs == nil {
		task.Status.WorkspacePVCs = make(map[string]string)
	}
	taskUID8 := string(task.UID)[:8]
	patch := client.MergeFrom(task.DeepCopy())
	changed := false
	for _, tmpl := range agent.Spec.VolumeClaimTemplates {
		name := tmpl.Name
		if _, exists := task.Status.WorkspacePVCs[name]; exists {
			continue
		}
		pvcName := "agent-workspace-" + taskUID8 + "-" + name
		spec := tmpl.Spec.DeepCopy()
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pvcName,
				Namespace: task.Namespace,
				Labels: map[string]string{
					"codemowers.cloud/task":     task.Name,
					"codemowers.cloud/agent":    agent.Name,
					"codemowers.cloud/pvc-role": name,
				},
				OwnerReferences: []metav1.OwnerReference{taskOwnerRef(task)},
			},
			Spec: *spec,
		}
		if err := r.Create(ctx, pvc); err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("create workspace PVC %q: %w", name, err)
		}
		task.Status.WorkspacePVCs[name] = pvcName
		changed = true
	}
	if changed {
		return r.Status().Patch(ctx, task, patch)
	}
	return nil
}

func (r *TaskReconciler) reconcileWorkerPod(ctx context.Context, task *agentsv1alpha1.Task, agent *agentsv1alpha1.Agent) (ctrl.Result, error) {
	// Check if a worker pod already exists for this task.
	if task.Status.WorkerPodName != "" {
		var pod corev1.Pod
		err := r.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: task.Status.WorkerPodName}, &pod)
		if err == nil {
			// If the pod completed and a new wake signal arrived (event consumer
			// set codemowers.cloud/wake-pending), clear the pod name so we
			// spawn a fresh worker below. The worker's AMQP queue already holds
			// the prompt; the new pod will consume it on startup.
			if pod.Status.Phase == corev1.PodSucceeded &&
				task.Annotations["codemowers.cloud/wake-pending"] == "true" {
				annPatch := client.MergeFrom(task.DeepCopy())
				delete(task.Annotations, "codemowers.cloud/wake-pending")
				if err := r.Client.Patch(ctx, task, annPatch); err != nil {
					return ctrl.Result{}, err
				}
				statPatch := client.MergeFrom(task.DeepCopy())
				task.Status.WorkerPodName = ""
				task.Status.WorkerPodUID = ""
				if err := r.Status().Patch(ctx, task, statPatch); err != nil {
					return ctrl.Result{}, err
				}
			} else {
				return r.syncPhaseFromPod(ctx, task, &pod)
			}
		} else if !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		} else {
			// Pod disappeared; clear and re-create on next reconcile.
			patch := client.MergeFrom(task.DeepCopy())
			task.Status.WorkerPodName = ""
			task.Status.WorkerPodUID = ""
			if err := r.Status().Patch(ctx, task, patch); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// Ensure the per-task RabbitMQ queue exists and publish the objective once.
	// The queue is durable so the message survives broker restarts and is waiting
	// when the worker subscribes. The annotation guards against re-publishing on
	// operator restart.
	if r.AMQP != nil {
		queue := taskQueue(task.Namespace, task.Name)
		if err := r.AMQP.DeclareQueue(queue); err != nil {
			return ctrl.Result{}, fmt.Errorf("declare task queue: %w", err)
		}
		if task.Annotations["codemowers.cloud/objective-published"] != "true" {
			msg := rabbitmq.PromptMessage{Type: "objective", Body: task.Spec.Objective}
			if err := r.AMQP.Publish(ctx, queue, msg); err != nil {
				return ctrl.Result{}, fmt.Errorf("publish objective: %w", err)
			}
			annPatch := client.MergeFrom(task.DeepCopy())
			if task.Annotations == nil {
				task.Annotations = make(map[string]string)
			}
			task.Annotations["codemowers.cloud/objective-published"] = "true"
			if err := r.Client.Patch(ctx, task, annPatch); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	podName := fmt.Sprintf("agent-worker-%s-%s", task.Name[:min(8, len(task.Name))], randSuffix())
	pod := r.buildWorkerPod(podName, task, agent, "agent-worker")

	if err := r.Create(ctx, pod); err != nil {
		return ctrl.Result{}, fmt.Errorf("create worker pod: %w", err)
	}

	patch := client.MergeFrom(task.DeepCopy())
	task.Status.WorkerPodName = pod.Name
	task.Status.WorkerPodUID = string(pod.UID)
	if err := r.Status().Patch(ctx, task, patch); err != nil {
		return ctrl.Result{}, err
	}

	return r.setPhase(ctx, task, agentsv1alpha1.TaskPhasePending, "WorkerCreated")
}

func (r *TaskReconciler) syncPhaseFromPod(ctx context.Context, task *agentsv1alpha1.Task, pod *corev1.Pod) (ctrl.Result, error) {
	var phase agentsv1alpha1.TaskPhase
	var reason string

	switch pod.Status.Phase {
	case corev1.PodPending:
		phase, reason = agentsv1alpha1.TaskPhasePending, "WorkerPending"
	case corev1.PodRunning:
		phase, reason = agentsv1alpha1.TaskPhaseRunning, "WorkerActive"
	case corev1.PodSucceeded:
		phase, reason = agentsv1alpha1.TaskPhaseWaiting, "WorkerCompleted"
	case corev1.PodFailed:
		phase, reason = agentsv1alpha1.TaskPhaseFailed, "WorkerFailed"
	default:
		return ctrl.Result{}, nil
	}

	return r.setPhase(ctx, task, phase, reason)
}

func (r *TaskReconciler) setPhase(ctx context.Context, task *agentsv1alpha1.Task, phase agentsv1alpha1.TaskPhase, reason string) (ctrl.Result, error) {
	if task.Status.Phase == phase && task.Status.Reason == reason {
		return ctrl.Result{}, nil
	}
	patch := client.MergeFrom(task.DeepCopy())
	task.Status.Phase = phase
	task.Status.Reason = reason
	return ctrl.Result{}, r.Status().Patch(ctx, task, patch)
}

func (r *TaskReconciler) ownerAgent(ctx context.Context, task *agentsv1alpha1.Task) (*agentsv1alpha1.Agent, error) {
	for _, ref := range task.OwnerReferences {
		if ref.Kind == "Agent" && ref.APIVersion == agentsv1alpha1.GroupVersion.String() {
			var agent agentsv1alpha1.Agent
			if err := r.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: ref.Name}, &agent); err != nil {
				return nil, fmt.Errorf("get owner agent %s: %w", ref.Name, err)
			}
			return &agent, nil
		}
	}
	return nil, fmt.Errorf("task %s/%s has no Agent ownerReference", task.Namespace, task.Name)
}

func (r *TaskReconciler) buildWorkerPod(name string, task *agentsv1alpha1.Task, agent *agentsv1alpha1.Agent, sa string) *corev1.Pod {
	ps := agent.Spec.Template.Spec.DeepCopy()

	// Safety overrides regardless of user-supplied values.
	ps.RestartPolicy = corev1.RestartPolicyNever
	ps.HostUsers = boolPtr(false)
	if ps.ServiceAccountName == "" {
		ps.ServiceAccountName = sa
	}

	// Workspace PVC volumes from VolumeClaimTemplates; containers mount by name.
	for _, tmpl := range agent.Spec.VolumeClaimTemplates {
		pvcName, ok := task.Status.WorkspacePVCs[tmpl.Name]
		if !ok {
			continue
		}
		ps.Volumes = append(ps.Volumes, corev1.Volume{
			Name: tmpl.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcName,
				},
			},
		})
	}

	// Credential secret volumes named cred-<secretName>; containers mount explicitly.
	for _, secretName := range agent.Spec.CredentialRefs {
		ps.Volumes = append(ps.Volumes, corev1.Volume{
			Name: "cred-" + secretName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: secretName},
			},
		})
	}

	// Operator-managed env vars prepended to every container; user entries follow.
	opEnv := []corev1.EnvVar{
		{Name: "TASK_NAME", Value: task.Name},
		{Name: "TASK_NAMESPACE", Value: task.Namespace},
		{Name: "AGENT_NAME", Value: agent.Name},
	}
	if r.AMQPURL != "" {
		opEnv = append(opEnv,
			corev1.EnvVar{Name: "AMQP_URL", Value: r.AMQPURL},
			corev1.EnvVar{Name: "TASK_QUEUE", Value: taskQueue(task.Namespace, task.Name)},
		)
	}
	for i := range ps.Containers {
		ps.Containers[i].Env = append(opEnv, ps.Containers[i].Env...)
	}
	for i := range ps.InitContainers {
		ps.InitContainers[i].Env = append(opEnv, ps.InitContainers[i].Env...)
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: task.Namespace,
			Labels: map[string]string{
				"codemowers.cloud/task":  task.Name,
				"codemowers.cloud/agent": agent.Name,
			},
			OwnerReferences: []metav1.OwnerReference{taskOwnerRef(task)},
		},
		Spec: *ps,
	}
}

func (r *TaskReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.Task{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Complete(r)
}

func taskOwnerRef(task *agentsv1alpha1.Task) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         agentsv1alpha1.GroupVersion.String(),
		Kind:               "Task",
		Name:               task.Name,
		UID:                task.UID,
		BlockOwnerDeletion: boolPtr(true),
		Controller:         boolPtr(true),
	}
}

func boolPtr(b bool) *bool { return &b }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func randSuffix() string {
	b := make([]byte, 4)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}
