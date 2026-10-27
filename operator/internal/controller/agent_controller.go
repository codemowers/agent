// Package controller implements the Agent and Task reconcilers.
package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/codemowers/agent/operator/internal/rabbitmq"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
)

// AgentReconciler reconciles Agent CRs: counts owned Tasks and updates status.
type AgentReconciler struct {
	client.Client
	APIReader       client.Reader
	Scheme          *runtime.Scheme
	ForgeHTTPClient *http.Client
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
		client.MatchingLabels{"codemowers.ai/agent": agent.Name},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("list tasks: %w", err)
	}
	if err := r.removeLegacyAccessBinding(ctx, &agent); err != nil {
		return ctrl.Result{}, err
	}
	probeDue := agent.Status.ForgesCheckedAt == nil || agent.Status.ForgesObservedGeneration != agent.Generation || agent.Status.TaskCount != len(taskList.Items)
	if agent.Status.TaskCount == len(taskList.Items) && !probeDue {
		return ctrl.Result{}, nil
	}

	patch := client.MergeFrom(agent.DeepCopy())
	agent.Status.TaskCount = len(taskList.Items)
	if probeDue {
		probeCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		agent.Status.Forges = r.probeForges(probeCtx, &agent)
		cancel()
		now := metav1.Now()
		agent.Status.ForgesCheckedAt = &now
		agent.Status.ForgesObservedGeneration = agent.Generation
	}
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
	// KubectlTag is the API server version, read at startup, that untagged
	// kubectl image volumes are pinned to.
	KubectlTag string
	// HarnessNamespace hosts the operator and Device bridge Services that
	// workers of every Agent namespace call.
	HarnessNamespace string
}

func (r *TaskReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var task agentsv1alpha1.Task
	if err := r.Get(ctx, req.NamespacedName, &task); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !task.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalizeTaskQueue(ctx, &task)
	}

	// Resolve owning Agent.
	agent, err := r.ownerAgent(ctx, &task)
	if err != nil {
		return ctrl.Result{}, err
	}
	if changed, err := r.ensureTaskAgentOwnership(ctx, &task, agent); err != nil {
		return ctrl.Result{}, err
	} else if changed {
		return ctrl.Result{Requeue: true}, nil
	}
	if task.Spec.IdleSeconds == nil || task.Spec.Resources == nil || task.Spec.Model == nil {
		patch := client.MergeFrom(task.DeepCopy())
		if task.Spec.IdleSeconds == nil {
			task.Spec.IdleSeconds = int32Ptr(agentsv1alpha1.AgentIdleSeconds(agent))
		}
		if task.Spec.Resources == nil {
			task.Spec.Resources = agentsv1alpha1.AgentWorkerResources(agent)
		}
		if task.Spec.Model == nil {
			model := agent.Spec.Model
			task.Spec.Model = &model
		}
		if err := r.Patch(ctx, &task, patch); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if task.Status.Completed() && !task.Spec.Suspend {
		// Completion stops the worker; the operator owns Task specs.
		patch := client.MergeFrom(task.DeepCopy())
		task.Spec.Suspend = true
		if err := r.Patch(ctx, &task, patch); err != nil {
			return ctrl.Result{}, err
		}
	}
	if task.Spec.Suspend {
		return r.reconcileSuspended(ctx, &task)
	}
	coalesceDelay, err := r.flushCoalescedWake(ctx, &task)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Ensure one PVC per VolumeClaimTemplate entry.
	if len(agent.Spec.VolumeClaimTemplates) > 0 {
		if err := r.ensureWorkspacePVCs(ctx, &task, agent); err != nil {
			return ctrl.Result{}, err
		}
	}

	result, err := r.reconcileWorkerPod(ctx, &task, agent)
	if err == nil && coalesceDelay > 0 && (result.RequeueAfter == 0 || result.RequeueAfter > coalesceDelay) && !result.Requeue {
		result.RequeueAfter = coalesceDelay
	}
	return result, err
}

const taskQueueFinalizer = "codemowers.ai/task-queue"

// trackTaskQueue records a Task queue before it is declared, so deleting the
// Task deletes the queue too.
func (r *TaskReconciler) trackTaskQueue(ctx context.Context, task *agentsv1alpha1.Task, queue string) error {
	return trackTaskQueue(ctx, r.Client, r.AMQP, task, queue)
}

func trackTaskQueue(ctx context.Context, c client.Client, amqp AMQPManager, task *agentsv1alpha1.Task, queue string) error {
	if !controllerutil.ContainsFinalizer(task, taskQueueFinalizer) {
		patch := client.MergeFrom(task.DeepCopy())
		controllerutil.AddFinalizer(task, taskQueueFinalizer)
		if err := c.Patch(ctx, task, patch); err != nil {
			return fmt.Errorf("add Task queue finalizer: %w", err)
		}
	}
	if task.Status.Queue == queue {
		return nil
	}
	patch := client.MergeFrom(task.DeepCopy())
	previous := task.Status.Queue
	task.Status.Queue = queue
	if err := c.Status().Patch(ctx, task, patch); err != nil {
		return fmt.Errorf("record Task queue: %w", err)
	}
	if previous != "" && amqp != nil {
		// A changed worker ServiceAccount renames the queue.
		return amqp.DeleteQueue(previous)
	}
	return nil
}

func (r *TaskReconciler) finalizeTaskQueue(ctx context.Context, task *agentsv1alpha1.Task) error {
	if !controllerutil.ContainsFinalizer(task, taskQueueFinalizer) {
		return nil
	}
	if task.Status.Queue != "" {
		if r.AMQP == nil {
			return fmt.Errorf("RabbitMQ unavailable to delete Task queue")
		}
		if err := r.AMQP.DeleteQueue(task.Status.Queue); err != nil {
			return fmt.Errorf("delete Task queue: %w", err)
		}
	}
	patch := client.MergeFrom(task.DeepCopy())
	controllerutil.RemoveFinalizer(task, taskQueueFinalizer)
	return r.Patch(ctx, task, patch)
}

// flushCoalescedWake ends expired event windows. A window that collected more
// events sends one follow-up prompt and opens the next window, so a busy key
// wakes its Task at most once per window; a quiet one closes. Task status is
// durable, so a restart does not lose a deadline or a trailing wake. It
// returns the delay until the next open window ends.
func (r *TaskReconciler) flushCoalescedWake(ctx context.Context, task *agentsv1alpha1.Task) (time.Duration, error) {
	var next time.Duration
	var expired []string
	for key, window := range task.Status.Events.Windows {
		if delay := time.Until(window.Until.Time); delay > 0 {
			if next == 0 || delay < next {
				next = delay
			}
			continue
		}
		expired = append(expired, key)
	}
	if len(expired) == 0 {
		return next, nil
	}
	slices.Sort(expired)
	patch := client.MergeFrom(task.DeepCopy())
	for _, key := range expired {
		window := task.Status.Events.Windows[key]
		if window.Trailing {
			if r.AMQP == nil {
				return 0, fmt.Errorf("RabbitMQ unavailable for coalesced wake")
			}
			agent, err := r.ownerAgent(ctx, task)
			if err != nil {
				return 0, err
			}
			queue := agentsv1alpha1.TaskQueue(task, agent)
			if err := r.trackTaskQueue(ctx, task, queue); err != nil {
				return 0, err
			}
			if err := r.AMQP.DeclareQueue(queue); err != nil {
				return 0, err
			}
			sum := sha256.Sum256([]byte(key))
			if err := r.AMQP.Publish(ctx, queue, rabbitmq.PromptMessage{
				Type: "event", Body: "New events on " + key + " — read the latest updates and decide what to do next.", RoutingKey: key,
				MessageID:  fmt.Sprintf("coalesce-%s-%x-%d", task.UID, sum[:8], window.Until.Unix()),
				ReceivedAt: time.Now().UnixMilli(),
			}); err != nil {
				return 0, err
			}
			task.Status.Events.WakePending = true
			task.Status.Events.Windows[key] = agentsv1alpha1.EventWindow{Binding: window.Binding,
				Until: metav1.NewTime(time.Now().Add(agentsv1alpha1.CoalesceWindow(task, agent)))}
			continue
		}
		delete(task.Status.Events.Windows, key)
	}
	if err := r.Status().Patch(ctx, task, patch); err != nil {
		return 0, err
	}
	for _, window := range task.Status.Events.Windows {
		if delay := time.Until(window.Until.Time); delay > 0 && (next == 0 || delay < next) {
			next = delay
		}
	}
	return next, nil
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
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				if changed, err := r.recordWorkerResults(ctx, task, &pod); err != nil {
					return ctrl.Result{}, err
				} else if changed {
					return ctrl.Result{Requeue: true}, nil
				}
				if err := r.recordWorkerActivity(ctx, task, &pod, "exit", workerExitDescription(&pod)); err != nil {
					return ctrl.Result{}, err
				}
			}
		} else if !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(task.Namespace),
		client.MatchingLabels{"codemowers.ai/task": task.Name}); err != nil {
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
			if err := r.recordWorkerActivity(ctx, task, pod, "stop", "Stopping worker "+pod.Name); err != nil {
				return ctrl.Result{}, err
			}
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
	if task.Status.Completed() {
		return r.setPhase(ctx, task, agentsv1alpha1.TaskPhaseCompleted, "CompletedByAgent")
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
		pvcName := agentsv1alpha1.ResourcePrefix(agent) + name + "-" + taskUID8
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
	if !task.Status.RetryWorker {
		patch := client.MergeFrom(task.DeepCopy())
		task.Status.RetryWorker = true
		if err := r.Status().Patch(ctx, task, patch); err != nil {
			return false, fmt.Errorf("mark worker image retry: %w", err)
		}
	}
	if pod.DeletionTimestamp == nil {
		if err := r.recordWorkerActivity(ctx, task, pod, "stop", "Stopping worker "+pod.Name); err != nil {
			return false, err
		}
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
			if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed &&
				pod.Annotations[workerConfigHashAnnotation] != workerConfigHash(task, agent) {
				if !task.Status.RetryWorker {
					patch := client.MergeFrom(task.DeepCopy())
					task.Status.RetryWorker = true
					if err := r.Status().Patch(ctx, task, patch); err != nil {
						return ctrl.Result{}, err
					}
				}
				if pod.DeletionTimestamp == nil {
					if err := r.recordWorkerActivity(ctx, task, &pod, "stop", "Stopping worker "+pod.Name); err != nil {
						return ctrl.Result{}, err
					}
					if err := r.Delete(ctx, &pod); err != nil && !errors.IsNotFound(err) {
						return ctrl.Result{}, err
					}
				}
				return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
			}
			if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
				published, err := r.publishRuntimeConfig(ctx, task, agent, &pod)
				if err != nil {
					return ctrl.Result{}, err
				}
				if published {
					return ctrl.Result{Requeue: true}, nil
				}
			}
			if pod.Status.Phase == corev1.PodFailed && !task.Status.RetryWorker {
				if changed, err := r.recordWorkerResults(ctx, task, &pod); err != nil {
					return ctrl.Result{}, err
				} else if changed {
					return ctrl.Result{Requeue: true}, nil
				}
				if err := r.recordWorkerActivity(ctx, task, &pod, "exit", workerExitDescription(&pod)); err != nil {
					return ctrl.Result{}, err
				}
				return r.suspendLostWorker(ctx, task, "WorkerFailed")
			}
			// If the pod completed and a new wake signal arrived (event consumer
			// set status.events.wakePending), clear the pod name so we
			// spawn a fresh worker below. The worker's AMQP queue already holds
			// the prompt; the new pod will consume it on startup.
			if (pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed) &&
				task.Status.Events.WakePending {
				if changed, err := r.recordWorkerResults(ctx, task, &pod); err != nil {
					return ctrl.Result{}, err
				} else if changed {
					return ctrl.Result{Requeue: true}, nil
				}
				if err := r.recordWorkerActivity(ctx, task, &pod, "exit", workerExitDescription(&pod)); err != nil {
					return ctrl.Result{}, err
				}
				if r.AMQP == nil {
					return r.setPhase(ctx, task, agentsv1alpha1.TaskPhasePending, "RabbitMQUnavailable")
				}
				depth, err := r.AMQP.QueueDepth(agentsv1alpha1.TaskQueue(task, agent))
				if err != nil {
					return ctrl.Result{}, fmt.Errorf("inspect task queue: %w", err)
				}
				statPatch := client.MergeFrom(task.DeepCopy())
				task.Status.Events.WakePending = false
				if depth == 0 {
					if err := r.Status().Patch(ctx, task, statPatch); err != nil {
						return ctrl.Result{}, err
					}
					return r.syncPhaseFromPod(ctx, task, &pod)
				}
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
			// A vanished worker requires explicit Resume. Queued prompts stay pending.
			if !task.Status.RetryWorker {
				return r.suspendLostWorker(ctx, task, "WorkerDisappeared")
			}
			patch := client.MergeFrom(task.DeepCopy())
			task.Status.RetryWorker = false
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
		client.MatchingLabels{"codemowers.ai/task": task.Name}); err != nil {
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
		if err := r.recordWorkerActivity(ctx, task, existing, "start", "Starting worker "+existing.Name); err != nil {
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
	// when the worker subscribes. Task status guards against re-publishing on
	// operator restart.
	if r.AMQP != nil {
		queue := agentsv1alpha1.TaskQueue(task, agent)
		if err := r.trackTaskQueue(ctx, task, queue); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.AMQP.DeclareQueue(queue); err != nil {
			return ctrl.Result{}, fmt.Errorf("declare task queue: %w", err)
		}
		if !task.Status.ObjectivePublished {
			msg := rabbitmq.PromptMessage{Type: "objective", Body: task.Spec.Objective, MessageID: string(task.UID) + ":objective"}
			if err := r.AMQP.Publish(ctx, queue, msg); err != nil {
				return ctrl.Result{}, fmt.Errorf("publish objective: %w", err)
			}
			patch := client.MergeFrom(task.DeepCopy())
			task.Status.ObjectivePublished = true
			if err := r.Status().Patch(ctx, task, patch); err != nil {
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

	podName := workerPodName(agentsv1alpha1.ResourcePrefix(agent), task.Name, randSuffix())
	sa := agentsv1alpha1.WorkerServiceAccount(agent)
	pod := r.buildWorkerPod(podName, task, agent, sa, r.authorizedMailboxes(ctx, agent, task.Namespace, sa),
		r.authorizedMCPServers(ctx, task.Namespace, sa)...)
	r.pinKubectlImages(pod)

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
	if err := r.recordWorkerActivity(ctx, task, pod, "start", "Starting worker "+pod.Name); err != nil {
		return ctrl.Result{}, err
	}

	return r.setPhase(ctx, task, agentsv1alpha1.TaskPhasePending, "WorkerCreated")
}

func (r *TaskReconciler) publishRuntimeConfig(ctx context.Context, task *agentsv1alpha1.Task, agent *agentsv1alpha1.Agent, pod *corev1.Pod) (bool, error) {
	initial, _ := strconv.ParseInt(pod.Annotations[workerRuntimeGenerationAnnotation], 10, 64)
	if task.Generation <= initial || task.Generation <= task.Status.RuntimePublishedGeneration {
		return false, nil
	}
	if r.AMQP == nil {
		return false, fmt.Errorf("RabbitMQ unavailable for runtime configuration")
	}
	queue := agentsv1alpha1.TaskQueue(task, agent)
	if err := r.AMQP.DeclareQueue(queue); err != nil {
		return false, err
	}
	model := agent.Spec.Model
	if task.Spec.Model != nil {
		model = *task.Spec.Model
	}
	idle := taskIdleSeconds(task, agent)
	if err := r.AMQP.Publish(ctx, queue, rabbitmq.PromptMessage{
		Type: "config", Generation: task.Generation, IdleSeconds: &idle, Model: model.Name,
		DisabledMCPs: append([]string{}, task.Spec.DisabledMCPs...),
	}); err != nil {
		return false, err
	}
	patch := client.MergeFrom(task.DeepCopy())
	task.Status.RuntimePublishedGeneration = task.Generation
	if err := r.Status().Patch(ctx, task, patch); err != nil {
		return false, err
	}
	return true, nil
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// Unexpected worker loss suspends the Task even when messages are queued.
// Operator-authorized replacements retain their existing RetryWorker path.
func (r *TaskReconciler) suspendLostWorker(ctx context.Context, task *agentsv1alpha1.Task, reason string) (ctrl.Result, error) {
	if !task.Spec.Suspend {
		patch := client.MergeFrom(task.DeepCopy())
		task.Spec.Suspend = true
		if err := r.Patch(ctx, task, patch); err != nil {
			return ctrl.Result{}, err
		}
	}
	return r.setPhase(ctx, task, agentsv1alpha1.TaskPhasePaused, reason)
}

func (r *TaskReconciler) syncPhaseFromPod(ctx context.Context, task *agentsv1alpha1.Task, pod *corev1.Pod) (ctrl.Result, error) {
	var phase agentsv1alpha1.TaskPhase
	var reason string

	switch pod.Status.Phase {
	case corev1.PodPending:
		phase, reason = agentsv1alpha1.TaskPhasePending, "WorkerPending"
	case corev1.PodRunning:
		phase, reason = agentsv1alpha1.TaskPhaseRunning, "WorkerActive"
		// A ready worker that has not reported a turn yet waits for work.
		if task.Status.Activity == "starting" && podReady(pod) {
			// The lock keeps a stale cache from overwriting a reported turn.
			patch := client.MergeFromWithOptions(task.DeepCopy(), client.MergeFromWithOptimisticLock{})
			task.Status.Activity = "idle"
			if err := r.Status().Patch(ctx, task, patch); err != nil {
				return ctrl.Result{}, err
			}
		}
	case corev1.PodSucceeded:
		phase, reason = agentsv1alpha1.TaskPhaseWaiting, "WorkerCompleted"
	case corev1.PodFailed:
		phase, reason = agentsv1alpha1.TaskPhaseFailed, "WorkerFailed"
	default:
		return ctrl.Result{}, nil
	}
	if phase == agentsv1alpha1.TaskPhaseWaiting || phase == agentsv1alpha1.TaskPhaseFailed {
		if changed, err := r.recordWorkerResults(ctx, task, pod); err != nil {
			return ctrl.Result{}, err
		} else if changed {
			return ctrl.Result{Requeue: true}, nil
		}
		if err := r.recordWorkerActivity(ctx, task, pod, "exit", workerExitDescription(pod)); err != nil {
			return ctrl.Result{}, err
		}
	}

	return r.setPhase(ctx, task, phase, reason)
}

func (r *TaskReconciler) recordWorkerResults(ctx context.Context, task *agentsv1alpha1.Task, pod *corev1.Pod) (bool, error) {
	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Terminated == nil || status.State.Terminated.Message == "" {
			continue
		}
		var report struct {
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal([]byte(status.State.Terminated.Message), &report); err == nil && report.Summary != "" &&
			(task.Status.Progress == nil || !strings.HasPrefix(task.Status.Progress.Summary, report.Summary)) {
			patch := client.MergeFrom(task.DeepCopy())
			task.Status.Progress = &agentsv1alpha1.TaskProgress{Summary: report.Summary, UpdatedAt: metav1.Now()}
			if err := r.Status().Patch(ctx, task, patch); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

func (r *TaskReconciler) setPhase(ctx context.Context, task *agentsv1alpha1.Task, phase agentsv1alpha1.TaskPhase, reason string) (ctrl.Result, error) {
	activity := task.Status.Activity
	if phase == agentsv1alpha1.TaskPhaseRunning {
		if activity == "" || activity == "stopped" {
			activity = "starting"
		}
	} else if phase == agentsv1alpha1.TaskPhasePending && task.Status.WorkerPodName != "" {
		activity = "starting"
	} else if phase != agentsv1alpha1.TaskPhasePending {
		activity = "stopped"
	}
	if task.Status.Phase == phase && task.Status.Reason == reason && task.Status.Activity == activity {
		return ctrl.Result{}, nil
	}
	patch := client.MergeFrom(task.DeepCopy())
	task.Status.Phase = phase
	task.Status.Reason = reason
	task.Status.Activity = activity
	return ctrl.Result{}, r.Status().Patch(ctx, task, patch)
}

func (r *TaskReconciler) ownerAgent(ctx context.Context, task *agentsv1alpha1.Task) (*agentsv1alpha1.Agent, error) {
	if task.Spec.AgentRef == "" {
		return nil, fmt.Errorf("task %s/%s has no spec.agentRef", task.Namespace, task.Name)
	}
	var agent agentsv1alpha1.Agent
	if err := r.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: task.Spec.AgentRef}, &agent); err != nil {
		return nil, fmt.Errorf("get referenced Agent %s: %w", task.Spec.AgentRef, err)
	}
	return &agent, nil
}

func (r *TaskReconciler) ensureTaskAgentOwnership(ctx context.Context, task *agentsv1alpha1.Task, agent *agentsv1alpha1.Agent) (bool, error) {
	owner := metav1.GetControllerOf(task)
	if owner != nil && (owner.APIVersion != agentsv1alpha1.GroupVersion.String() || owner.Kind != "Agent" || owner.Name != agent.Name || owner.UID != agent.UID) {
		return false, fmt.Errorf("task %s/%s controller owner does not match spec.agentRef", task.Namespace, task.Name)
	}
	matchingRef := -1
	for i, ref := range task.OwnerReferences {
		if ref.APIVersion != agentsv1alpha1.GroupVersion.String() || ref.Kind != "Agent" {
			continue
		}
		if ref.Name != agent.Name || ref.UID != agent.UID {
			return false, fmt.Errorf("task %s/%s Agent owner does not match spec.agentRef", task.Namespace, task.Name)
		}
		matchingRef = i
	}
	if owner != nil && task.Labels["codemowers.ai/agent"] == agent.Name {
		return false, nil
	}
	patch := client.MergeFrom(task.DeepCopy())
	if matchingRef < 0 {
		task.OwnerReferences = append(task.OwnerReferences, metav1.OwnerReference{
			APIVersion: agentsv1alpha1.GroupVersion.String(), Kind: "Agent", Name: agent.Name, UID: agent.UID,
			Controller: boolPtr(true), BlockOwnerDeletion: boolPtr(true),
		})
	} else if owner == nil {
		task.OwnerReferences[matchingRef].Controller = boolPtr(true)
		task.OwnerReferences[matchingRef].BlockOwnerDeletion = boolPtr(true)
	}
	if task.Labels == nil {
		task.Labels = make(map[string]string)
	}
	task.Labels["codemowers.ai/agent"] = agent.Name
	if err := r.Patch(ctx, task, patch); err != nil {
		return false, fmt.Errorf("set Task Agent ownership: %w", err)
	}
	return true, nil
}

func taskIdleSeconds(task *agentsv1alpha1.Task, agent *agentsv1alpha1.Agent) int32 {
	if task.Spec.IdleSeconds != nil {
		return *task.Spec.IdleSeconds
	}
	return agentsv1alpha1.AgentIdleSeconds(agent)
}

const (
	workerConfigHashAnnotation        = "codemowers.ai/worker-config-hash"
	workerVersionAnnotation           = "codemowers.ai/operator-worker-version"
	workerRuntimeGenerationAnnotation = "codemowers.ai/worker-runtime-generation"
	// Bump when the operator/runner contract requires replacement of every
	// active worker Pod. The Task informer enqueues existing Tasks at startup.
	WorkerPodVersion = "27"
)

func workerResources(task *agentsv1alpha1.Task, agent *agentsv1alpha1.Agent) *corev1.ResourceRequirements {
	if task.Spec.Resources != nil {
		return task.Spec.Resources.DeepCopy()
	}
	return agentsv1alpha1.AgentWorkerResources(agent)
}

func workerConfigHash(task *agentsv1alpha1.Task, agent *agentsv1alpha1.Agent) string {
	return workerConfigHashWithVersion(task, agent, WorkerPodVersion)
}

// workerConfigHashWithVersion covers what the worker Pod is built from.
// Attached MCP servers and Mailboxes are excluded: RBAC changes reach a
// worker with its next Pod.
func workerConfigHashWithVersion(task *agentsv1alpha1.Task, agent *agentsv1alpha1.Agent, version string) string {
	model := agent.Spec.Model
	if task.Spec.Model != nil {
		model = *task.Spec.Model
	}
	data, _ := json.Marshal(struct {
		Version          string                       `json:"version"`
		Resources        *corev1.ResourceRequirements `json:"resources"`
		GitProviders     []agentsv1alpha1.GitProvider `json:"gitProviders"`
		CheckoutEnv      []corev1.EnvVar              `json:"checkoutEnv"`
		Backend          string                       `json:"backend"`
		CredentialKeyRef string                       `json:"credentialKeyRef"`
		BaseURL          string                       `json:"baseURL,omitempty"`
		Template         corev1.PodTemplateSpec       `json:"template"`
	}{version, workerResources(task, agent), agent.Spec.GitProviders, checkoutEnvironment(task, agent), model.Backend, model.CredentialKeyRef, model.BaseURL, agent.Spec.Template})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func checkoutEnvironment(task *agentsv1alpha1.Task, agent *agentsv1alpha1.Agent) []corev1.EnvVar {
	for _, ref := range task.Spec.ExternalReferences {
		if ref.RepositoryOwner == "" || ref.RepositoryName == "" {
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
			return []corev1.EnvVar{
				{Name: "AGENT_FORGE_PROVIDER", Value: provider.Type},
				{Name: "AGENT_FORGE_URL", Value: serverURL},
				{Name: "AGENT_REPOSITORY", Value: ref.RepositoryOwner + "/" + ref.RepositoryName},
				{Name: "AGENT_FORGE_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: agentsv1alpha1.ProviderTokensSecretName}, Key: provider.CredentialKeyRef}}},
			}
		}
	}
	return nil
}

func (r *TaskReconciler) recordWorkerActivity(ctx context.Context, task *agentsv1alpha1.Task, pod *corev1.Pod, action, content string) error {
	identity := string(pod.UID)
	if identity == "" {
		identity = pod.Name
	}
	if action == "exit" && task.Status.LastWorkerExit == identity {
		return nil
	}
	patch := client.MergeFrom(task.DeepCopy())
	// A clean exit after the operator stopped the worker repeats the stop line.
	stopped := slices.ContainsFunc(task.Status.Conversation, func(m agentsv1alpha1.TaskMessage) bool { return m.ID == "worker-stop:"+identity })
	changed := false
	if action != "exit" || pod.Status.Phase == corev1.PodFailed || !stopped {
		changed = task.Status.AppendMessage(agentsv1alpha1.TaskMessage{ID: "worker-" + action + ":" + identity,
			Role: "system", Source: "operator", Actor: "Operator", Content: content, CreatedAt: metav1.Now()})
	}
	if action == "exit" {
		task.Status.LastWorkerExit = identity
		changed = true
	}
	if !changed {
		return nil
	}
	return r.Status().Patch(ctx, task, patch)
}

// workerExitDescription names a clean exit briefly and gives a failure's
// reason and exit code.
func workerExitDescription(pod *corev1.Pod) string {
	if pod.Status.Phase != corev1.PodFailed {
		return "Worker " + pod.Name + " exited"
	}
	verb := "failed"
	detail := pod.Status.Reason
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != "worker" || status.State.Terminated == nil {
			continue
		}
		terminated := status.State.Terminated
		if terminated.Reason != "" {
			detail = terminated.Reason
		}
		if detail == "" {
			detail = "exit"
		}
		detail += fmt.Sprintf(" (exit code %d)", terminated.ExitCode)
		break
	}
	if detail == "" {
		return "Worker " + pod.Name + " " + verb
	}
	return "Worker " + pod.Name + " " + verb + ": " + detail
}

// authorizedMailboxes lists the namespace's Mailboxes on which the worker
// ServiceAccount holds the send or read verb. RBAC is the only attachment
// knob; an RBAC change reaches a worker with its next Pod.
func (r *TaskReconciler) authorizedMailboxes(ctx context.Context, agent *agentsv1alpha1.Agent, namespace, sa string) []string {
	var mailboxes agentsv1alpha1.MailboxList
	if err := r.List(ctx, &mailboxes, client.InNamespace(namespace)); err != nil {
		log.FromContext(ctx).Error(err, "list Mailboxes for worker attachment")
		return nil
	}
	var names []string
	for _, mailbox := range mailboxes.Items {
		if mailbox.Spec.AgentRef != "" && mailbox.Spec.AgentRef != agent.Name {
			continue
		}
		for _, verb := range []string{"send", "read"} {
			review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
				User:   "system:serviceaccount:" + namespace + ":" + sa,
				Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + namespace, "system:authenticated"},
				ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: namespace, Verb: verb,
					Group: agentsv1alpha1.GroupVersion.Group, Resource: "mailboxes", Name: mailbox.Name},
			}}
			if err := r.Create(ctx, review); err != nil {
				log.FromContext(ctx).Error(err, "review Mailbox access", "mailbox", mailbox.Name)
				break
			}
			if review.Status.Allowed {
				names = append(names, mailbox.Name)
				break
			}
		}
	}
	return names
}

// mcpServer is an advertised MCP server attached to a worker; a non-empty
// Audience sends it a projected token.
type mcpServer struct {
	Name, URL, Audience string
}

// authorizedMCPServers lists the ClusterMCPServers and the namespace's
// MCPServers on which the worker ServiceAccount holds the custom verb "use".
// A namespaced server overrides a cluster one of the same name. Like Mailbox
// attachment, an RBAC change reaches a worker with its next Pod.
func (r *TaskReconciler) authorizedMCPServers(ctx context.Context, namespace, sa string) []mcpServer {
	logger := log.FromContext(ctx)
	byName := map[string]mcpServer{}
	allowed := func(scope, resource, name string) bool {
		review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   "system:serviceaccount:" + namespace + ":" + sa,
			Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + namespace, "system:authenticated"},
			ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: scope, Verb: "use",
				Group: agentsv1alpha1.GroupVersion.Group, Resource: resource, Name: name},
		}}
		if err := r.Create(ctx, review); err != nil {
			logger.Error(err, "review MCP server access", "server", name)
			return false
		}
		return review.Status.Allowed
	}
	var cluster agentsv1alpha1.ClusterMCPServerList
	if err := r.List(ctx, &cluster); err != nil {
		logger.Error(err, "list ClusterMCPServers for worker attachment")
	}
	for _, server := range cluster.Items {
		if allowed("", "clustermcpservers", server.Name) {
			byName[server.Name] = mcpServer{server.Name, server.Spec.URL, server.Spec.TokenAudience()}
		}
	}
	var local agentsv1alpha1.MCPServerList
	if err := r.List(ctx, &local, client.InNamespace(namespace)); err != nil {
		logger.Error(err, "list MCPServers for worker attachment")
	}
	for _, server := range local.Items {
		if allowed(namespace, "mcpservers", server.Name) {
			byName[server.Name] = mcpServer{server.Name, server.Spec.URL, server.Spec.TokenAudience()}
		}
	}
	servers := make([]mcpServer, 0, len(byName))
	for _, server := range byName {
		servers = append(servers, server)
	}
	slices.SortFunc(servers, func(a, b mcpServer) int { return strings.Compare(a.Name, b.Name) })
	return servers
}

// KubectlTag returns the release part of an API server version, such as
// v1.36.5 for v1.36.5+k3s1, since image tags cannot carry build metadata.
func KubectlTag(gitVersion string) (string, error) {
	tag := regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+`).FindString(gitVersion)
	if tag == "" {
		return "", fmt.Errorf("unrecognized API server version %q", gitVersion)
	}
	return tag, nil
}

// pinKubectlImages tags image volumes such as registry.k8s.io/kubectl, given
// without a tag or digest, with the API server's version, so workers get a
// kubectl matching the cluster they run in.
func (r *TaskReconciler) pinKubectlImages(pod *corev1.Pod) {
	for i := range pod.Spec.Volumes {
		if image := pod.Spec.Volumes[i].Image; image != nil && strings.HasSuffix(image.Reference, "/kubectl") {
			image.Reference += ":" + r.KubectlTag
		}
	}
}

func (r *TaskReconciler) buildWorkerPod(name string, task *agentsv1alpha1.Task, agent *agentsv1alpha1.Agent, sa string, mailboxes []string, servers ...mcpServer) *corev1.Pod {
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
		{Name: "AGENT_WORKER_POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}},
		{Name: "AGENT_NAME", Value: agent.Name},
		{Name: "AGENT_RESOURCE_PREFIX", Value: agent.Spec.ResourcePrefix},
		{Name: "AGENT_INSTRUCTIONS", Value: agent.Spec.Instructions},
		{Name: "AGENT_MODE", Value: "task-worker"},
		{Name: "AGENT_IDLE_SECONDS", Value: strconv.FormatInt(int64(taskIdleSeconds(task, agent)), 10)},
		{Name: "AGENT_TASK_GENERATION", Value: strconv.FormatInt(task.Generation, 10)},
		{Name: "AGENT_OPERATOR_MCP_URL", Value: agentsv1alpha1.OperatorMCPURL(r.HarnessNamespace)},
		{Name: "AGENT_OPERATOR_MCP_TOKEN_FILE", Value: agentsv1alpha1.WorkerTokenDir + "/agent_operator"},
	}
	if task.Status.Progress != nil && task.Status.Progress.Summary != "" {
		opEnv = append(opEnv, corev1.EnvVar{Name: "AGENT_PREVIOUS_SUMMARY", Value: task.Status.Progress.Summary})
	}
	if task.Status.LastResultAt != nil {
		opEnv = append(opEnv, corev1.EnvVar{Name: "AGENT_LAST_RESULT_AT", Value: task.Status.LastResultAt.Time.Format(time.RFC3339Nano)})
	} else if task.Status.Progress != nil && !task.Status.Progress.UpdatedAt.IsZero() {
		opEnv = append(opEnv, corev1.EnvVar{Name: "AGENT_LAST_RESULT_AT", Value: task.Status.Progress.UpdatedAt.Time.Format(time.RFC3339Nano)})
	}
	// One projected token per service, each for the service's own audience,
	// so no service can replay a token against another.
	token := func(audience, path string) corev1.VolumeProjection {
		return corev1.VolumeProjection{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
			Audience: audience, ExpirationSeconds: int64Ptr(3600), Path: path}}
	}
	tokens := []corev1.VolumeProjection{
		token(agentsv1alpha1.ServiceAudience(agentsv1alpha1.OperatorMCPURL(r.HarnessNamespace)), "agent_operator"),
		token(agentsv1alpha1.RabbitMQAudience, "rabbitmq"),
	}
	endpoints := make([]agentsv1alpha1.MCPEndpoint, len(servers))
	for i, server := range servers {
		endpoints[i] = agentsv1alpha1.MCPEndpoint{Name: server.Name, URL: server.URL}
		if server.Audience != "" {
			endpoints[i].TokenFile = agentsv1alpha1.WorkerTokenDir + "/mcp/" + server.Name
			tokens = append(tokens, token(server.Audience, "mcp/"+server.Name))
		}
	}
	for _, mailbox := range mailboxes {
		tokens = append(tokens, token(agentsv1alpha1.ServiceAudience(agentsv1alpha1.MailboxMCPURL(mailbox, task.Namespace)), "mcp/mailbox-"+mailbox))
	}
	endpointsJSON, _ := json.Marshal(agentsv1alpha1.WorkerMCPEndpoints(r.HarnessNamespace, task.Namespace, endpoints, mailboxes))
	opEnv = append(opEnv, corev1.EnvVar{Name: "AGENT_MCP_ENDPOINTS", Value: string(endpointsJSON)})
	disabledJSON, _ := json.Marshal(append([]string{}, task.Spec.DisabledMCPs...))
	opEnv = append(opEnv, corev1.EnvVar{Name: "AGENT_DISABLED_MCPS", Value: string(disabledJSON)})
	model := agent.Spec.Model
	if task.Spec.Model != nil {
		model = *task.Spec.Model
	}
	credentialName := map[string]string{"claude": "ANTHROPIC_API_KEY", "codex": "OPENAI_API_KEY", "opencode": "AGENT_MODEL_API_KEY"}[model.Backend]
	opEnv = append(opEnv, corev1.EnvVar{Name: "AGENT_BACKEND", Value: model.Backend},
		corev1.EnvVar{Name: credentialName, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: agentsv1alpha1.ProviderTokensSecretName}, Key: model.CredentialKeyRef}}})
	if model.Name != "" {
		opEnv = append(opEnv, corev1.EnvVar{Name: "AGENT_MODEL", Value: model.Name})
	}
	if model.BaseURL != "" {
		opEnv = append(opEnv, corev1.EnvVar{Name: "AGENT_MODEL_BASE_URL", Value: model.BaseURL})
	}
	opEnv = append(opEnv, forgeCredentialEnv(agent.Spec.GitProviders)...)
	opEnv = append(opEnv, checkoutEnvironment(task, agent)...)
	if r.AMQPURL != "" {
		opEnv = append(opEnv,
			corev1.EnvVar{Name: "AMQP_URL", Value: r.AMQPURL},
			corev1.EnvVar{Name: "TASK_QUEUE", Value: agentsv1alpha1.TaskQueue(task, agent)},
			corev1.EnvVar{Name: "AGENT_RABBITMQ_TOKEN_FILE", Value: agentsv1alpha1.WorkerTokenDir + "/rabbitmq"},
		)
	}
	for i := range ps.Containers {
		if ps.Containers[i].Name == "worker" {
			ps.Containers[i].Image = r.WorkerImage
			ps.Containers[i].Resources = *workerResources(task, agent)
			filtered := ps.Containers[i].Env[:0]
			for _, env := range ps.Containers[i].Env {
				if env.Name != "AGENT_IDLE_SECONDS" && !managedForgeCredentialName(env.Name) {
					filtered = append(filtered, env)
				}
			}
			ps.Containers[i].Env = filtered
			ps.Containers[i].Env = append(ps.Containers[i].Env, opEnv...)
			ps.Containers[i].VolumeMounts = append(ps.Containers[i].VolumeMounts, corev1.VolumeMount{
				Name: "agent-tokens", MountPath: agentsv1alpha1.WorkerTokenDir, ReadOnly: true})
			// The worker serves /metrics and, once its workspace and broker are
			// ready to take work, /readyz.
			ps.Containers[i].Ports = append(ps.Containers[i].Ports, corev1.ContainerPort{
				Name: "metrics", ContainerPort: agentsv1alpha1.WorkerMetricsPort, Protocol: corev1.ProtocolTCP})
			ps.Containers[i].ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/readyz", Port: intstr.FromString("metrics")}}, PeriodSeconds: 2, FailureThreshold: 1}
		}
	}
	ps.Volumes = append(ps.Volumes, corev1.Volume{Name: "agent-tokens", VolumeSource: corev1.VolumeSource{
		Projected: &corev1.ProjectedVolumeSource{Sources: tokens}}})

	annotations := make(map[string]string, len(agent.Spec.Template.ObjectMeta.Annotations)+1)
	for key, value := range agent.Spec.Template.ObjectMeta.Annotations {
		annotations[key] = value
	}
	annotations[workerConfigHashAnnotation] = workerConfigHash(task, agent)
	annotations[workerVersionAnnotation] = WorkerPodVersion
	annotations[workerRuntimeGenerationAnnotation] = strconv.FormatInt(task.Generation, 10)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       task.Namespace,
			Labels:          mergeManagedLabels(agent.Spec.Template.ObjectMeta.Labels, task, agent),
			Annotations:     annotations,
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
	out["codemowers.ai/task"] = task.Name
	out["codemowers.ai/agent"] = agent.Name
	return out
}

func (r *TaskReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.Task{}).
		Watches(&agentsv1alpha1.Agent{}, handler.EnqueueRequestsFromMapFunc(r.tasksForAgent),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&corev1.Pod{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Complete(r)
}

func (r *TaskReconciler) tasksForAgent(ctx context.Context, object client.Object) []reconcile.Request {
	agent, ok := object.(*agentsv1alpha1.Agent)
	if !ok {
		return nil
	}
	var tasks agentsv1alpha1.TaskList
	if err := r.List(ctx, &tasks, client.InNamespace(agent.Namespace), client.MatchingLabels{"codemowers.ai/agent": agent.Name}); err != nil {
		log.FromContext(ctx).Error(err, "list Agent tasks for worker configuration update", "agent", agent.Name)
		return nil
	}
	requests := make([]reconcile.Request, 0, len(tasks.Items))
	for _, task := range tasks.Items {
		for _, owner := range task.OwnerReferences {
			if owner.Kind == "Agent" && owner.APIVersion == agentsv1alpha1.GroupVersion.String() && owner.UID == agent.UID {
				requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&task)})
				break
			}
		}
	}
	return requests
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
func int32Ptr(n int32) *int32 { return &n }
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

// workerPodName starts with the Agent's name prefix and leaves room for a Task
// prefix and a fresh incarnation ID on every worker restart.
func workerPodName(prefix, taskName, incarnation string) string {
	taskPrefix := strings.TrimRight(taskName[:min(8, len(taskName))], "-")
	if maxPrefixLength := 63 - len(taskPrefix) - len("-") - len(incarnation); len(prefix) > maxPrefixLength {
		prefix = strings.TrimRight(prefix[:maxPrefixLength-1], "-") + "-"
	}
	return prefix + taskPrefix + "-" + incarnation
}
