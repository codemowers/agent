// Package controller implements the Agent and Task reconcilers.
package controller

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/codemowers/agent/operator/internal/rabbitmq"
	corev1 "k8s.io/api/core/v1"
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
	APIReader client.Reader
	Scheme    *runtime.Scheme
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
	if err := r.removeLegacyAccessBinding(ctx, &agent); err != nil {
		return ctrl.Result{}, err
	}
	if agent.Status.TaskCount == len(taskList.Items) {
		return ctrl.Result{}, nil
	}

	patch := client.MergeFrom(agent.DeepCopy())
	agent.Status.TaskCount = len(taskList.Items)
	if err := r.Status().Patch(ctx, &agent, patch); err != nil {
		return ctrl.Result{}, err
	}

	log.FromContext(ctx).Info("reconciled agent", "tasks", agent.Status.TaskCount)
	return ctrl.Result{}, nil
}

func (r *AgentReconciler) removeLegacyAccessBinding(ctx context.Context, agent *agentsv1alpha1.Agent) error {
	var binding rbacv1.RoleBinding
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: "agent-access-" + agent.Name}, &binding); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read legacy access binding: %w", err)
	}
	for _, owner := range binding.OwnerReferences {
		if owner.UID == agent.UID && owner.Kind == "Agent" && owner.APIVersion == agentsv1alpha1.GroupVersion.String() {
			if err := r.Delete(ctx, &binding); err != nil && !errors.IsNotFound(err) {
				return fmt.Errorf("delete legacy access binding: %w", err)
			}
			return nil
		}
	}
	return nil
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
	AMQP        AMQPManager // nil when RabbitMQ is not configured
	AMQPURL     string      // injected into worker pods as AMQP_URL
	WorkerImage string      // approved runner image injected by deployment/Skaffold
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
	// Check the recorded UID before touching a Pod with a reused name.
	if task.Status.WorkerPodName != "" {
		var pod corev1.Pod
		err := r.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: task.Status.WorkerPodName}, &pod)
		if err == nil {
			if !ownedByTask(&pod, task) {
				return r.setPhase(ctx, task, agentsv1alpha1.TaskPhaseFailed, "WorkerOwnerMismatch")
			}
			if task.Status.WorkerPodUID != "" && task.Status.WorkerPodUID != string(pod.UID) {
				return r.setPhase(ctx, task, agentsv1alpha1.TaskPhaseFailed, "WorkerUIDMismatch")
			}
		} else if !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(task.Namespace),
		client.MatchingLabels{"codemowers.cloud/task": task.Name}); err != nil {
		return ctrl.Result{}, err
	}
	stopping := false
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !ownedByTask(pod, task) || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		stopping = true
		if pod.DeletionTimestamp == nil {
			if err := r.Delete(ctx, pod); err != nil && !errors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
	}
	if stopping {
		if _, err := r.setPhase(ctx, task, agentsv1alpha1.TaskPhasePaused, "WorkerStopping"); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	if task.Status.WorkerPodName != "" {
		patch := client.MergeFrom(task.DeepCopy())
		task.Status.WorkerPodName = ""
		task.Status.WorkerPodUID = ""
		if err := r.Status().Patch(ctx, task, patch); err != nil {
			return ctrl.Result{}, err
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
		name := tmpl.Metadata.Name
		if name == "" {
			return fmt.Errorf("volumeClaimTemplates metadata.name is required")
		}
		if _, exists := task.Status.WorkspacePVCs[name]; exists {
			continue
		}
		pvcName := "agent-workspace-" + taskUID8 + "-" + name
		spec := tmpl.Spec.DeepCopy()
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:            pvcName,
				Namespace:       task.Namespace,
				Labels:          mergeManagedLabels(tmpl.Metadata.Labels, task, agent),
				Annotations:     tmpl.Metadata.Annotations,
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

// Replace only a worker that has never started and is stuck pulling an old
// image. Pod updates are unsafe here because admission may inject pull Secrets
// on UPDATE, which Kubernetes rejects as an immutable field change.
func (r *TaskReconciler) replaceUnstartedWorkerImage(ctx context.Context, task *agentsv1alpha1.Task, pod *corev1.Pod) (bool, error) {
	if r.WorkerImage == "" || pod.Status.Phase != corev1.PodPending {
		return false, nil
	}
	oldImage := false
	for _, container := range pod.Spec.Containers {
		if container.Name == "worker" && container.Image != r.WorkerImage {
			oldImage = true
		}
	}
	if !oldImage {
		return false, nil
	}
	blocked := false
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "worker" && status.State.Waiting != nil {
			blocked = status.State.Waiting.Reason == "ErrImagePull" || status.State.Waiting.Reason == "ImagePullBackOff"
		}
	}
	if !blocked {
		return false, nil
	}
	if task.Annotations["codemowers.cloud/retry-worker"] != "true" {
		patch := client.MergeFrom(task.DeepCopy())
		if task.Annotations == nil {
			task.Annotations = make(map[string]string)
		}
		task.Annotations["codemowers.cloud/retry-worker"] = "true"
		if err := r.Client.Patch(ctx, task, patch); err != nil {
			return false, fmt.Errorf("mark worker image retry: %w", err)
		}
	}
	if pod.DeletionTimestamp == nil {
		if err := r.Delete(ctx, pod); err != nil && !errors.IsNotFound(err) {
			return false, fmt.Errorf("delete unstarted worker: %w", err)
		}
	}
	return true, nil
}

func (r *TaskReconciler) reconcileWorkerPod(ctx context.Context, task *agentsv1alpha1.Task, agent *agentsv1alpha1.Agent) (ctrl.Result, error) {
	// Check if a worker pod already exists for this task.
	if task.Status.WorkerPodName != "" {
		var pod corev1.Pod
		err := r.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: task.Status.WorkerPodName}, &pod)
		if err == nil {
			if !ownedByTask(&pod, task) {
				return r.setPhase(ctx, task, agentsv1alpha1.TaskPhaseFailed, "WorkerOwnerMismatch")
			}
			if task.Status.WorkerPodUID != "" && task.Status.WorkerPodUID != string(pod.UID) {
				return r.setPhase(ctx, task, agentsv1alpha1.TaskPhaseFailed, "WorkerUIDMismatch")
			}
			if replaced, err := r.replaceUnstartedWorkerImage(ctx, task, &pod); err != nil {
				return ctrl.Result{}, err
			} else if replaced {
				return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
			}
			// If the pod completed and a new wake signal arrived (event consumer
			// set codemowers.cloud/wake-pending), clear the pod name so we
			// spawn a fresh worker below. The worker's AMQP queue already holds
			// the prompt; the new pod will consume it on startup.
			if (pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed) &&
				task.Annotations["codemowers.cloud/wake-pending"] == "true" {
				if r.AMQP == nil {
					return r.setPhase(ctx, task, agentsv1alpha1.TaskPhasePending, "RabbitMQUnavailable")
				}
				depth, err := r.AMQP.QueueDepth(taskQueue(task.Namespace, task.Name))
				if err != nil {
					return ctrl.Result{}, fmt.Errorf("inspect task queue: %w", err)
				}
				if depth == 0 {
					annPatch := client.MergeFrom(task.DeepCopy())
					delete(task.Annotations, "codemowers.cloud/wake-pending")
					if err := r.Client.Patch(ctx, task, annPatch); err != nil {
						return ctrl.Result{}, err
					}
					return r.syncPhaseFromPod(ctx, task, &pod)
				}
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
			// A vanished worker might still have an uncertain native result.
			// A new prompt does not authorize replacing it against the same PVC.
			if task.Annotations["codemowers.cloud/retry-worker"] != "true" {
				return r.setPhase(ctx, task, agentsv1alpha1.TaskPhasePaused, "WorkerDisappeared")
			}
			annPatch := client.MergeFrom(task.DeepCopy())
			delete(task.Annotations, "codemowers.cloud/retry-worker")
			if err := r.Client.Patch(ctx, task, annPatch); err != nil {
				return ctrl.Result{}, err
			}
			patch := client.MergeFrom(task.DeepCopy())
			task.Status.WorkerPodName = ""
			task.Status.WorkerPodUID = ""
			if err := r.Status().Patch(ctx, task, patch); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	// The API may have created a Pod just before the operator lost the status
	// update. Adopt that exact Task-owned Pod instead of running a second worker
	// against the same workspace.
	var candidates corev1.PodList
	if err := r.List(ctx, &candidates, client.InNamespace(task.Namespace),
		client.MatchingLabels{"codemowers.cloud/task": task.Name}); err != nil {
		return ctrl.Result{}, err
	}
	var existing *corev1.Pod
	for i := range candidates.Items {
		pod := &candidates.Items[i]
		if !ownedByTask(pod, task) || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if existing != nil {
			return r.setPhase(ctx, task, agentsv1alpha1.TaskPhaseFailed, "MultipleWorkers")
		}
		existing = pod
	}
	if existing != nil {
		patch := client.MergeFrom(task.DeepCopy())
		task.Status.WorkerPodName = existing.Name
		task.Status.WorkerPodUID = string(existing.UID)
		if err := r.Status().Patch(ctx, task, patch); err != nil {
			return ctrl.Result{}, err
		}
		return r.syncPhaseFromPod(ctx, task, existing)
	}
	if r.AMQP == nil || r.AMQPURL == "" {
		if _, err := r.setPhase(ctx, task, agentsv1alpha1.TaskPhasePending, "RabbitMQUnavailable"); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
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
			msg := rabbitmq.PromptMessage{Type: "objective", Body: task.Spec.Objective, MessageID: string(task.UID) + ":objective"}
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

	workerContainers := 0
	for _, container := range agent.Spec.Template.Spec.Containers {
		if container.Name == "worker" {
			workerContainers++
		}
	}
	if workerContainers != 1 {
		return r.setPhase(ctx, task, agentsv1alpha1.TaskPhaseFailed, "AgentMissingWorkerContainer")
	}
	if r.WorkerImage == "" {
		return r.setPhase(ctx, task, agentsv1alpha1.TaskPhaseFailed, "WorkerImageMissing")
	}

	podName := workerPodName(agent.Name, task.Name, randSuffix())
	pod := r.buildWorkerPod(podName, task, agent, "agent-worker")

	if err := r.Create(ctx, pod); err != nil {
		// Admission/quota errors are often transient or fixable by the user;
		// surface the cause and retry rather than terminally failing the Task.
		log.FromContext(ctx).Error(err, "create worker pod failed", "pod", podName)
		if _, perr := r.setPhase(ctx, task, agentsv1alpha1.TaskPhasePending, "PodCreateFailed"); perr != nil {
			return ctrl.Result{}, perr
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
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
	if phase == agentsv1alpha1.TaskPhaseWaiting || phase == agentsv1alpha1.TaskPhaseFailed {
		for _, status := range pod.Status.ContainerStatuses {
			if status.State.Terminated == nil || status.State.Terminated.Message == "" {
				continue
			}
			var report struct {
				Summary string `json:"summary"`
			}
			if err := json.Unmarshal([]byte(status.State.Terminated.Message), &report); err == nil && report.Summary != "" &&
				(task.Status.Progress == nil || task.Status.Progress.Summary != report.Summary) {
				patch := client.MergeFrom(task.DeepCopy())
				task.Status.Progress = &agentsv1alpha1.TaskProgress{Summary: report.Summary, UpdatedAt: metav1.Now()}
				if err := r.Status().Patch(ctx, task, patch); err != nil {
					return ctrl.Result{}, err
				}
				break
			}
		}
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
			if ref.UID != agent.UID {
				return nil, fmt.Errorf("task %s/%s Agent owner UID does not match", task.Namespace, task.Name)
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
	if len(agent.Spec.VolumeClaimTemplates) == 0 {
		ps.Volumes = append(ps.Volumes, corev1.Volume{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		for i := range ps.Containers {
			ps.Containers[i].VolumeMounts = append(ps.Containers[i].VolumeMounts,
				corev1.VolumeMount{Name: "workspace", MountPath: "/workspace"})
		}
	}

	// Workspace PVC volumes from VolumeClaimTemplates; containers mount by name.
	for _, tmpl := range agent.Spec.VolumeClaimTemplates {
		name := tmpl.Metadata.Name
		pvcName, ok := task.Status.WorkspacePVCs[name]
		if !ok {
			continue
		}
		ps.Volumes = append(ps.Volumes, corev1.Volume{
			Name: name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcName,
				},
			},
		})
	}

	// Execution credentials belong only to the named coding container.
	opEnv := []corev1.EnvVar{
		{Name: "TASK_NAME", Value: task.Name},
		{Name: "TASK_NAMESPACE", Value: task.Namespace},
		{Name: "AGENT_NAME", Value: agent.Name},
		{Name: "AGENT_MODE", Value: "task-worker"},
	}
	if task.Status.Progress != nil && task.Status.Progress.Summary != "" {
		opEnv = append(opEnv, corev1.EnvVar{Name: "AGENT_PREVIOUS_SUMMARY", Value: task.Status.Progress.Summary})
	}
	if len(agent.Spec.MCPEndpoints) > 0 {
		endpoints, _ := json.Marshal(agent.Spec.MCPEndpoints)
		opEnv = append(opEnv, corev1.EnvVar{Name: "AGENT_MCP_ENDPOINTS", Value: string(endpoints)})
	}
	model := agent.Spec.Model
	if task.Spec.Model != nil {
		model = *task.Spec.Model
	}
	credentialName := "ANTHROPIC_API_KEY"
	if model.Backend == "codex" {
		credentialName = "OPENAI_API_KEY"
	}
	opEnv = append(opEnv, corev1.EnvVar{Name: "AGENT_BACKEND", Value: model.Backend},
		corev1.EnvVar{Name: credentialName, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: model.CredentialSecretRef}, Key: "token"}}})
	if model.Name != "" {
		opEnv = append(opEnv, corev1.EnvVar{Name: "AGENT_MODEL", Value: model.Name})
	}
gitReferences:
	for _, ref := range task.Spec.ExternalReferences {
		if ref.RepositoryOwner == "" || ref.RepositoryName == "" || (ref.Source != "github" && ref.Source != "gitea") {
			continue
		}
		serverURL := strings.TrimRight(ref.ServerURL, "/")
		if serverURL == "" && ref.Source == "github" {
			serverURL = "https://github.com"
		}
		for _, provider := range agent.Spec.GitProviders {
			if provider.Type != ref.Source || strings.TrimRight(provider.ServerURL, "/") != serverURL {
				continue
			}
			opEnv = append(opEnv,
				corev1.EnvVar{Name: "AGENT_FORGE_PROVIDER", Value: provider.Type},
				corev1.EnvVar{Name: "AGENT_FORGE_URL", Value: serverURL},
				corev1.EnvVar{Name: "AGENT_REPOSITORY", Value: ref.RepositoryOwner + "/" + ref.RepositoryName},
				corev1.EnvVar{Name: "GH_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: provider.CredentialSecretRef}, Key: "token"}}},
			)
			break gitReferences
		}
	}
	if r.AMQPURL != "" {
		opEnv = append(opEnv,
			corev1.EnvVar{Name: "AMQP_URL", Value: r.AMQPURL},
			corev1.EnvVar{Name: "TASK_QUEUE", Value: taskQueue(task.Namespace, task.Name)},
		)
	}
	for i := range ps.Containers {
		if ps.Containers[i].Name == "worker" {
			ps.Containers[i].Image = r.WorkerImage
			ps.Containers[i].Env = append(ps.Containers[i].Env, opEnv...)
			for _, endpoint := range agent.Spec.MCPEndpoints {
				if endpoint.TokenFile != "/var/run/agent-mcp/driftmower" {
					continue
				}
				mounted := false
				for _, mount := range ps.Containers[i].VolumeMounts {
					if mount.MountPath == "/var/run/agent-mcp" {
						mounted = true
					}
				}
				if !mounted {
					ps.Volumes = append(ps.Volumes, corev1.Volume{Name: "agent-driftmower-token", VolumeSource: corev1.VolumeSource{
						Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{
							ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
								Audience:          "http://driftmower-mcp.driftmower.svc.cluster.local:3000/mcp",
								ExpirationSeconds: int64Ptr(3600), Path: "driftmower",
							},
						}}},
					}})
					ps.Containers[i].VolumeMounts = append(ps.Containers[i].VolumeMounts,
						corev1.VolumeMount{Name: "agent-driftmower-token", MountPath: "/var/run/agent-mcp", ReadOnly: true})
				}
				break
			}
		}
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       task.Namespace,
			Labels:          mergeManagedLabels(agent.Spec.Template.ObjectMeta.Labels, task, agent),
			Annotations:     agent.Spec.Template.ObjectMeta.Annotations,
			OwnerReferences: []metav1.OwnerReference{taskOwnerRef(task)},
		},
		Spec: *ps,
	}
}

func mergeManagedLabels(labels map[string]string, task *agentsv1alpha1.Task, agent *agentsv1alpha1.Agent) map[string]string {
	out := make(map[string]string, len(labels)+2)
	for key, value := range labels {
		out[key] = value
	}
	out["codemowers.cloud/task"] = task.Name
	out["codemowers.cloud/agent"] = agent.Name
	return out
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

func ownedByTask(pod *corev1.Pod, task *agentsv1alpha1.Task) bool {
	for _, owner := range pod.OwnerReferences {
		if owner.APIVersion == agentsv1alpha1.GroupVersion.String() && owner.Kind == "Task" &&
			owner.Name == task.Name && owner.UID == task.UID {
			return true
		}
	}
	return false
}

func boolPtr(b bool) *bool    { return &b }
func int64Ptr(n int64) *int64 { return &n }

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

// workerPodName keeps the owning Agent visible while leaving room for a Task
// prefix and a fresh incarnation ID on every worker restart.
func workerPodName(agentName, taskName, incarnation string) string {
	taskPrefix := taskName[:min(8, len(taskName))]
	maxAgentLength := 63 - len("agent---") - len(taskPrefix) - len(incarnation)
	if len(agentName) > maxAgentLength {
		agentName = strings.TrimRight(agentName[:maxAgentLength], "-")
	}
	return fmt.Sprintf("agent-%s-%s-%s", agentName, taskPrefix, incarnation)
}
