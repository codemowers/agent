// Package controller implements the Agent and Task reconcilers.
package controller

import (
	"context"
	"crypto/rand"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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
	logger := log.FromContext(ctx)

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
		client.MatchingFields{"metadata.ownerReferences[0].name": agent.Name},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("list tasks: %w", err)
	}

	patch := client.MergeFrom(agent.DeepCopy())
	agent.Status.TaskCount = len(taskList.Items)
	if err := r.Status().Patch(ctx, &agent, patch); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("reconciled agent", "tasks", agent.Status.TaskCount)
	return ctrl.Result{}, nil
}

func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.Agent{}).
		Owns(&agentsv1alpha1.Task{}).
		Complete(r)
}

// TaskReconciler reconciles Task CRs: manages worker Pod and workspace PVC lifecycle.
type TaskReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	WorkerImage string // default; overridden by agent spec
}

func (r *TaskReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

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

	if err := r.ensureWorkspacePVC(ctx, &task, agent); err != nil {
		return ctrl.Result{}, err
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

func (r *TaskReconciler) ensureWorkspacePVC(ctx context.Context, task *agentsv1alpha1.Task, agent *agentsv1alpha1.Agent) error {
	if task.Status.WorkspacePVC != "" {
		return nil
	}
	pvcName := "agent-workspace-" + string(task.UID)[:8]
	storageClass := "retain"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: task.Namespace,
			Labels: map[string]string{
				"codemowers.cloud/task": task.Name,
				"codemowers.cloud/agent": agent.Name,
			},
			OwnerReferences: []metav1.OwnerReference{taskOwnerRef(task)},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &storageClass,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: *resourceQuantity("10Gi"),
				},
			},
		},
	}
	if err := r.Create(ctx, pvc); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create workspace PVC: %w", err)
	}
	patch := client.MergeFrom(task.DeepCopy())
	task.Status.WorkspacePVC = pvcName
	return r.Status().Patch(ctx, task, patch)
}

func (r *TaskReconciler) reconcileWorkerPod(ctx context.Context, task *agentsv1alpha1.Task, agent *agentsv1alpha1.Agent) (ctrl.Result, error) {
	// Check if a worker pod already exists for this task.
	if task.Status.WorkerPodName != "" {
		var pod corev1.Pod
		err := r.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: task.Status.WorkerPodName}, &pod)
		if err == nil {
			return r.syncPhaseFromPod(ctx, task, &pod)
		}
		if !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		// Pod disappeared; clear and re-create on next reconcile.
		patch := client.MergeFrom(task.DeepCopy())
		task.Status.WorkerPodName = ""
		task.Status.WorkerPodUID = ""
		if err := r.Status().Patch(ctx, task, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	workerImage := agent.Spec.WorkerImage
	sa := agent.Spec.WorkerServiceAccountName
	if sa == "" {
		sa = "agent-worker"
	}

	podName := fmt.Sprintf("agent-worker-%s-%s", task.Name[:min(8, len(task.Name))], randSuffix())
	pod := r.buildWorkerPod(podName, task, agent, workerImage, sa)

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

func (r *TaskReconciler) buildWorkerPod(name string, task *agentsv1alpha1.Task, agent *agentsv1alpha1.Agent, image, sa string) *corev1.Pod {
	volumes := []corev1.Volume{
		{
			Name: "workspace",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: task.Status.WorkspacePVC,
				},
			},
		},
	}
	volumeMounts := []corev1.VolumeMount{
		{Name: "workspace", MountPath: "/workspace"},
	}

	// Mount allowed credentials.
	for _, secretName := range agent.Spec.CredentialRefs {
		volName := "cred-" + secretName
		volumes = append(volumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: secretName},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      volName,
			MountPath: "/secrets/" + secretName,
			ReadOnly:  true,
		})
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
		Spec: corev1.PodSpec{
			ServiceAccountName: sa,
			RestartPolicy:      corev1.RestartPolicyNever,
			HostUsers:          boolPtr(false),
			Containers: []corev1.Container{
				{
					Name:         "worker",
					Image:        image,
					VolumeMounts: volumeMounts,
					Env: []corev1.EnvVar{
						{Name: "TASK_NAME", Value: task.Name},
						{Name: "TASK_NAMESPACE", Value: task.Namespace},
						{Name: "AGENT_NAME", Value: agent.Name},
					},
				},
			},
			Volumes: volumes,
		},
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

func resourceQuantity(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

func randSuffix() string {
	b := make([]byte, 4)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}
