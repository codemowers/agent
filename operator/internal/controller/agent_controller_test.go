package controller

import (
	"context"
	"strings"
	"testing"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestWorkerPodVersionChangesPodConfigHash(t *testing.T) {
	task := &agentsv1alpha1.Task{}
	agent := &agentsv1alpha1.Agent{}
	if workerConfigHashWithVersion(task, agent, "1") == workerConfigHashWithVersion(task, agent, "2") {
		t.Fatal("worker Pod version did not change configuration hash")
	}
}

func TestRuntimeSettingsUseQueueWithoutReplacingPod(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	idle := int32(30)
	task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "team", Generation: 2},
		Spec: agentsv1alpha1.TaskSpec{IdleSeconds: &idle, Model: &agentsv1alpha1.ModelSpec{Backend: "claude", Name: "model-b", CredentialKeyRef: "key"}}}
	agent := &agentsv1alpha1.Agent{Spec: agentsv1alpha1.AgentSpec{Model: agentsv1alpha1.ModelSpec{Backend: "claude", Name: "model-a", CredentialKeyRef: "key"}}}
	initial := task.DeepCopy()
	initial.Spec.IdleSeconds = int32Ptr(60)
	initial.Spec.Model.Name = "model-a"
	if workerConfigHash(task, agent) != workerConfigHash(initial, agent) {
		t.Fatal("idle or same-backend model ID change would replace worker")
	}
	initial.Spec.Model.Backend = "codex"
	if workerConfigHash(task, agent) == workerConfigHash(initial, agent) {
		t.Fatal("backend change would leave stale worker credentials")
	}
	store := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(task).WithObjects(task).Build()
	broker := &coalesceBroker{}
	r := &TaskReconciler{Client: store, AMQP: broker}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{workerRuntimeGenerationAnnotation: "1"}}}
	changed, err := r.publishRuntimeConfig(context.Background(), task, agent, pod)
	if err != nil || !changed || len(broker.messages) != 1 || broker.messages[0].Type != "config" ||
		broker.messages[0].IdleSeconds == nil || *broker.messages[0].IdleSeconds != 30 || broker.messages[0].Model != "model-b" {
		t.Fatalf("runtime settings were not queued: changed=%t err=%v messages=%+v", changed, err, broker.messages)
	}
}

func TestLegacyAccessBindingCleanupOnlyDeletesOwnedBinding(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "developer", Namespace: "team", UID: types.UID("agent-uid")}}
	owned := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "agent-access-developer", Namespace: "team",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: agentsv1alpha1.GroupVersion.String(), Kind: "Agent", Name: "developer", UID: agent.UID}}}}
	store := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owned).Build()
	reconciler := &AgentReconciler{Client: store, APIReader: store}
	if err := reconciler.removeLegacyAccessBinding(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(owned), &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("owned binding remains: %v", err)
	}
	unowned := owned.DeepCopy()
	unowned.ResourceVersion = ""
	unowned.OwnerReferences[0].UID = types.UID("other-agent")
	if err := store.Create(context.Background(), unowned); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.removeLegacyAccessBinding(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(unowned), &rbacv1.RoleBinding{}); err != nil {
		t.Fatalf("unowned binding removed: %v", err)
	}
}

func TestDirectTaskCopiesAgentIdleTimeoutBeforeStartingWorker(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "developer", Namespace: "team", UID: types.UID("agent-uid")},
		Spec: agentsv1alpha1.AgentSpec{IdleSeconds: int32Ptr(45), Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}}}}
	task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "team"},
		Spec: agentsv1alpha1.TaskSpec{AgentRef: agent.Name, Objective: "Maintain the application"}}
	store := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent, task).Build()
	reconciler := &TaskReconciler{Client: store}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}); err != nil {
		t.Fatal(err)
	}
	var actual agentsv1alpha1.Task
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(task), &actual); err != nil {
		t.Fatal(err)
	}
	if owner := metav1.GetControllerOf(&actual); owner == nil || owner.Name != agent.Name || owner.UID != agent.UID || actual.Labels["codemowers.cloud/agent"] != agent.Name {
		t.Fatalf("declarative Task did not acquire Agent ownership: %+v, labels: %+v", owner, actual.Labels)
	}
	if actual.Spec.IdleSeconds == nil || *actual.Spec.IdleSeconds != 45 {
		t.Fatalf("direct Task did not copy idle timeout: %+v", actual.Spec.IdleSeconds)
	}
	if actual.Spec.Resources == nil || actual.Spec.Resources.Requests.Memory().String() != "256Mi" {
		t.Fatalf("direct Task did not copy resources: %+v", actual.Spec.Resources)
	}
}

func TestTaskRefCannotReparentExistingOwner(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "developer", Namespace: "team", UID: types.UID("new-agent")}}
	task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "team",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: agentsv1alpha1.GroupVersion.String(), Kind: "Agent", Name: "other", UID: types.UID("old-agent")}}},
		Spec: agentsv1alpha1.TaskSpec{AgentRef: agent.Name, Objective: "Investigate failures"}}
	store := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent, task).Build()
	reconciler := &TaskReconciler{Client: store}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}); err == nil {
		t.Fatal("Task with conflicting Agent owner was reparented")
	}
}

func TestWorkerCredentialsStayInWorkerContainer(t *testing.T) {
	task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "team", UID: types.UID("task-uid")},
		Spec: agentsv1alpha1.TaskSpec{IdleSeconds: int32Ptr(5), Resources: &corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}}, ExternalReferences: []agentsv1alpha1.ExternalReference{{
			Source: "github", ServerURL: "https://github.com", RepositoryOwner: "team", RepositoryName: "project", Kind: "issue", ID: "8"}}}}
	agent := &agentsv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "developer"},
		Spec: agentsv1alpha1.AgentSpec{
			Model:        agentsv1alpha1.ModelSpec{Backend: "claude", CredentialKeyRef: "agent-cred-model"},
			IdleSeconds:  int32Ptr(60),
			GitProviders: []agentsv1alpha1.GitProvider{{Type: "github", ServerURL: "https://github.com", CredentialKeyRef: "agent-cred-git"}},
			MCPEndpoints: []agentsv1alpha1.MCPEndpoint{{Name: "driftmower", URL: "http://driftmower-mcp/mcp", TokenFile: "/var/run/agent-mcp/driftmower"}},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers:     []corev1.Container{{Name: "worker", Image: "codemowers/agent:latest"}, {Name: "helper", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m")}}}},
				InitContainers: []corev1.Container{{Name: "init"}},
			}},
		},
	}
	pod := (&TaskReconciler{AMQPURL: "amqp://broker", WorkerImage: "registry/agent:build"}).buildWorkerPod("worker-pod", task, agent, "agent-worker", nil)
	if pod.Spec.Containers[0].Image != "registry/agent:build" {
		t.Fatal("worker did not use the configured runner image")
	}
	if len(pod.Spec.Containers[0].Env) == 0 {
		t.Fatal("worker did not receive execution environment")
	}
	if len(pod.Spec.Containers[1].Env) != 0 || len(pod.Spec.InitContainers[0].Env) != 0 {
		t.Fatal("execution credentials leaked to another container")
	}
	brokerToken := false
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == "agent-rabbitmq-token" && volume.Projected.Sources[0].ServiceAccountToken.Audience == agentsv1alpha1.RabbitMQAudience {
			brokerToken = true
		}
	}
	queue := ""
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "TASK_QUEUE" {
			queue = env.Value
		}
	}
	if !brokerToken || queue != agentsv1alpha1.TaskQueue(task, agent) || !strings.HasPrefix(queue, "system:serviceaccount:") {
		t.Fatalf("worker cannot log in to its own broker queue: token=%v queue=%q", brokerToken, queue)
	}
	projected := false
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == "agent-driftmower-token" && volume.Projected != nil {
			projected = true
		}
	}
	if !projected {
		t.Fatal("Driftmower token was not projected for the worker")
	}
	gitCredential := false
	modelCredential := false
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "GH_TOKEN" && env.ValueFrom != nil && env.ValueFrom.SecretKeyRef.Name == agentsv1alpha1.ProviderTokensSecretName && env.ValueFrom.SecretKeyRef.Key == "agent-cred-git" {
			gitCredential = true
		}
		if env.Name == "ANTHROPIC_API_KEY" && env.ValueFrom != nil && env.ValueFrom.SecretKeyRef.Name == agentsv1alpha1.ProviderTokensSecretName && env.ValueFrom.SecretKeyRef.Key == "agent-cred-model" {
			modelCredential = true
		}
	}
	if !gitCredential || !modelCredential {
		t.Fatal("configured provider credential keys were not selected")
	}
	idleSeconds := ""
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "AGENT_IDLE_SECONDS" {
			idleSeconds = env.Value
		}
	}
	if idleSeconds != "5" {
		t.Fatalf("Task idle override missing from worker: %q", idleSeconds)
	}
	if pod.Spec.Containers[0].Resources.Limits.Memory().String() != "1Gi" ||
		pod.Spec.Containers[1].Resources.Requests.Cpu().String() != "25m" {
		t.Fatalf("worker resources or helper resources wrong: %+v", pod.Spec.Containers)
	}
	if pod.Annotations[workerConfigHashAnnotation] != workerConfigHash(task, agent) {
		t.Fatal("worker config hash missing")
	}
	beforeReference := workerConfigHash(task, agent)
	task.Status.ExternalReferences = append(task.Status.ExternalReferences, agentsv1alpha1.ExternalReference{
		Source: "imap", Kind: "thread", ID: "<root@example.test>", MailboxRef: "mail", MailboxUID: "mail-uid",
	})
	if workerConfigHash(task, agent) != beforeReference {
		t.Fatal("observed external reference changed worker configuration")
	}
	task.Spec.ExternalReferences[0].ServerURL = "https://unconfigured.example.test"
	other := (&TaskReconciler{}).buildWorkerPod("other-pod", task, agent, "agent-worker", nil)
	if other.Annotations[workerConfigHashAnnotation] == pod.Annotations[workerConfigHashAnnotation] {
		t.Fatal("changing Task repository reference did not change worker configuration hash")
	}
	defaultCredential := false
	for _, env := range other.Spec.Containers[0].Env {
		if env.Name == "GH_TOKEN" {
			defaultCredential = true
		}
		if env.Name == "AGENT_FORGE_TOKEN" {
			t.Fatal("Task-specific credential forwarded to an unconfigured server")
		}
	}
	if !defaultCredential {
		t.Fatal("single Agent GitHub provider was not available to an unbound Task")
	}
	task.Spec.ExternalReferences[0].ServerURL = "https://github.com"
	agent.Spec.GitProviders = append(agent.Spec.GitProviders,
		agentsv1alpha1.GitProvider{Type: "github", ServerURL: "https://ghe.example.test", CredentialKeyRef: "enterprise-key"},
		agentsv1alpha1.GitProvider{Type: "gitea", ServerURL: "https://gitea.com", CredentialKeyRef: "gitea-key"})
	agent.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "GH_TOKEN", Value: "stale-template-value"}}
	multiple := (&TaskReconciler{}).buildWorkerPod("multiple-pod", task, agent, "agent-worker", nil)
	if multiple.Annotations[workerConfigHashAnnotation] == pod.Annotations[workerConfigHashAnnotation] {
		t.Fatal("changing Agent Git providers did not change worker configuration hash")
	}
	names := map[string]string{}
	for _, env := range multiple.Spec.Containers[0].Env {
		if env.Name == "GH_TOKEN" {
			t.Fatal("ambiguous GH_TOKEN survived on a multi-GitHub worker")
		}
		if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
			names[env.Name] = env.ValueFrom.SecretKeyRef.Key
		}
	}
	for name, key := range map[string]string{
		"GH_TOKEN_GITHUB_COM":       "agent-cred-git",
		"GH_TOKEN_GHE_EXAMPLE_TEST": "enterprise-key",
		"GITEA_TOKEN":               "gitea-key",
		"AGENT_FORGE_TOKEN":         "agent-cred-git",
	} {
		if names[name] != key {
			t.Fatalf("missing or wrong %s: %v", name, names)
		}
	}
}

func TestWorkersReceiveScopedDeviceToken(t *testing.T) {
	task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "team"}}
	agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "support"}, Spec: agentsv1alpha1.AgentSpec{
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{AutomountServiceAccountToken: boolPtr(false),
			Containers: []corev1.Container{{Name: "worker"}}}},
	}}
	reconciler := &TaskReconciler{WorkerImage: "agent:test"}
	pod := reconciler.buildWorkerPod("worker", task, agent, "support-sa", nil)
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("worker changed the disabled default API token mount")
	}
	found := false
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == "agent-devices-token" {
			found = true
			if volume.Projected == nil || volume.Projected.Sources[0].ServiceAccountToken.Audience != "agent-devices-mcp" {
				t.Fatal("device token has the wrong audience")
			}
		}
	}
	if !found {
		t.Fatal("device token missing")
	}
}

func TestDisabledDeviceBridgeOmitsWorkerAccess(t *testing.T) {
	task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "team"}}
	agent := &agentsv1alpha1.Agent{Spec: agentsv1alpha1.AgentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{Name: "worker"}},
	}}}}
	r := &TaskReconciler{DisableDevices: true}
	pod := r.buildWorkerPod("worker", task, agent, "worker", nil)
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == "agent-devices-token" {
			t.Fatal("disabled bridge projected a Device token")
		}
	}
	for _, mount := range pod.Spec.Containers[0].VolumeMounts {
		if mount.Name == "agent-devices-token" {
			t.Fatal("disabled bridge mounted a Device token")
		}
	}
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "AGENT_MCP_ENDPOINTS" && strings.Contains(env.Value, `"name":"devices"`) {
			t.Fatal("disabled bridge exposed the Devices MCP endpoint")
		}
	}
	if r.workerConfigHash(task, agent) == workerConfigHash(task, agent) {
		t.Fatal("bridge setting did not change worker configuration hash")
	}
}

func TestWorkersAttachOnlyAuthorizedMailboxes(t *testing.T) {
	task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "team"}}
	agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "support"}, Spec: agentsv1alpha1.AgentSpec{
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker"}}}}}}
	reconciler := &TaskReconciler{WorkerImage: "agent:test"}
	hasMailboxToken := func(pod *corev1.Pod) bool {
		for _, volume := range pod.Spec.Volumes {
			if volume.Name == "agent-mailbox-token" {
				return volume.Projected.Sources[0].ServiceAccountToken.Audience == "agent-mailbox-mcp"
			}
		}
		return false
	}
	endpoints := func(pod *corev1.Pod) string {
		for _, env := range pod.Spec.Containers[0].Env {
			if env.Name == "AGENT_MCP_ENDPOINTS" {
				return env.Value
			}
		}
		return ""
	}
	if pod := reconciler.buildWorkerPod("worker", task, agent, "sa", nil); hasMailboxToken(pod) || strings.Contains(endpoints(pod), "mailbox-") {
		t.Fatal("worker without Mailbox access received a Mailbox endpoint")
	}
	pod := reconciler.buildWorkerPod("worker", task, agent, "sa", []string{"shared"})
	if !hasMailboxToken(pod) || !strings.Contains(endpoints(pod), `"name":"mailbox-shared","url":"http://agent-mailbox-shared.team.svc.cluster.local:8080/mcp"`) {
		t.Fatalf("authorized Mailbox not attached: %s", endpoints(pod))
	}
}

func TestAgentChangeEnqueuesOwnedTasks(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "developer", Namespace: "team", UID: types.UID("agent-uid")}}
	owned := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "owned", Namespace: "team",
		Labels: map[string]string{"codemowers.cloud/agent": agent.Name}, OwnerReferences: []metav1.OwnerReference{{APIVersion: agentsv1alpha1.GroupVersion.String(), Kind: "Agent", Name: agent.Name, UID: agent.UID}}}}
	unowned := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "unowned", Namespace: "team",
		Labels: map[string]string{"codemowers.cloud/agent": agent.Name}}}
	store := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owned, unowned).Build()
	requests := (&TaskReconciler{Client: store}).tasksForAgent(context.Background(), agent)
	if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(owned) {
		t.Fatalf("wrong Tasks enqueued for Agent change: %+v", requests)
	}
}

func TestWorkerConfigChangeStopsOwnedPodOnce(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "developer", Namespace: "team", UID: types.UID("agent-uid")},
		Spec: agentsv1alpha1.AgentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker"}}}}}}
	task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "team", UID: types.UID("task-uid")},
		Spec:   agentsv1alpha1.TaskSpec{IdleSeconds: int32Ptr(90), Resources: &corev1.ResourceRequirements{}},
		Status: agentsv1alpha1.TaskStatus{WorkerPodName: "worker", WorkerPodUID: "pod-uid"}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "team", UID: types.UID("pod-uid"),
		OwnerReferences: []metav1.OwnerReference{taskOwnerRef(task)}, Annotations: map[string]string{workerConfigHashAnnotation: "outdated"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	store := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(task).WithObjects(agent, task, pod).Build()
	r := &TaskReconciler{Client: store}
	if _, err := r.reconcileWorkerPod(context.Background(), task, agent); err != nil {
		t.Fatal(err)
	}
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("old worker remains: %v", err)
	}
	var actual agentsv1alpha1.Task
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(task), &actual); err != nil {
		t.Fatal(err)
	}
	if !actual.Status.RetryWorker {
		t.Fatal("replacement not marked")
	}
	if len(actual.Status.Conversation) != 1 || actual.Status.Conversation[0].Content != "Stopping worker worker" {
		t.Fatalf("missing stop activity: %+v", actual.Status.Conversation)
	}
	if err := r.recordWorkerActivity(context.Background(), &actual, pod, "stop", "Stopping worker worker"); err != nil {
		t.Fatal(err)
	}
	if len(actual.Status.Conversation) != 1 {
		t.Fatal("replayed stop activity duplicated")
	}
	if err := r.recordWorkerActivity(context.Background(), &actual, pod, "start", "Starting worker worker"); err != nil {
		t.Fatal(err)
	}
	if len(actual.Status.Conversation) != 2 {
		t.Fatal("start activity missing")
	}
}

func TestWorkerPodNameStartsWithAgentPrefix(t *testing.T) {
	agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "matmlazi"}}
	if name := workerPodName(agentsv1alpha1.ResourcePrefix(agent), "issue-12345", "ab12cd34"); name != "matmlazi-issue-12-ab12cd34" {
		t.Fatalf("unexpected worker Pod name: %s", name)
	}
	agent.Spec.ResourcePrefix = "mat-"
	if name := workerPodName(agentsv1alpha1.ResourcePrefix(agent), "task-1234", "ab12cd34"); name != "mat-task-123-ab12cd34" {
		t.Fatalf("unexpected worker Pod name: %s", name)
	}
	longName := workerPodName("agent-with-a-very-long-name-that-needs-to-be-shortened-before-the-task-and-incarnation-", "task-1234", "ab12cd34")
	if len(longName) > 63 || longName[len(longName)-8:] != "ab12cd34" {
		t.Fatalf("invalid long worker Pod name: %s", longName)
	}
}

func TestBlockedUnstartedWorkerIsReplacedForSkaffoldImage(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "team"}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "team"},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "worker", Image: "codemowers/agent:latest"}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{Name: "worker", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}}}
	store := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(task).WithObjects(task, pod).Build()
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(task), task); err != nil {
		t.Fatal(err)
	}
	reconciler := &TaskReconciler{Client: store, WorkerImage: "zot.example/agent@sha256:1234"}
	replaced, err := reconciler.replaceUnstartedWorkerImage(context.Background(), task, pod)
	if err != nil || !replaced {
		t.Fatalf("replace blocked worker: %v, %v", replaced, err)
	}
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("blocked worker remains: %v", err)
	}
	var actual agentsv1alpha1.Task
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(task), &actual); err != nil {
		t.Fatal(err)
	}
	if !actual.Status.RetryWorker {
		t.Fatal("worker retry not recorded")
	}
}

func TestWorkerExitDescriptionIncludesOOMReason(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-matmlazi-ab12"},
		Status: corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{Name: "worker",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}}}}}}
	if got := workerExitDescription(pod); got != "Worker agent-matmlazi-ab12 stopped: OOMKilled (exit code 137)" {
		t.Fatalf("unexpected exit activity: %s", got)
	}
}

func TestFailedWorkerIsRetainedAndOOMRecordedOnce(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "developer", Namespace: "team", UID: types.UID("agent-uid")}}
	task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "team", UID: types.UID("task-uid")},
		Status: agentsv1alpha1.TaskStatus{WorkerPodName: "agent-developer-task-ab12", WorkerPodUID: "pod-uid"}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: task.Status.WorkerPodName, Namespace: "team", UID: types.UID("pod-uid"),
		OwnerReferences: []metav1.OwnerReference{taskOwnerRef(task)}, Annotations: map[string]string{workerConfigHashAnnotation: "outdated"}},
		Status: corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{Name: "worker", State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}}}}}}
	store := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(task, pod).WithObjects(task, pod).Build()
	r := &TaskReconciler{Client: store}
	for i := 0; i < 2; i++ {
		var current agentsv1alpha1.Task
		if err := store.Get(context.Background(), client.ObjectKeyFromObject(task), &current); err != nil {
			t.Fatal(err)
		}
		if _, err := r.reconcileWorkerPod(context.Background(), &current, agent); err != nil {
			t.Fatal(err)
		}
	}
	var actual agentsv1alpha1.Task
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(task), &actual); err != nil {
		t.Fatal(err)
	}
	if len(actual.Status.Conversation) != 1 || actual.Status.Conversation[0].Content != "Worker agent-developer-task-ab12 stopped: OOMKilled (exit code 137)" {
		t.Fatalf("OOM exit missing or duplicated: %+v", actual.Status.Conversation)
	}
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); err != nil {
		t.Fatalf("failed pod deleted: %v", err)
	}
}

func TestUntaggedKubectlImagesFollowServerVersion(t *testing.T) {
	tag, err := KubectlTag("v1.36.5+k3s1")
	if err != nil || tag != "v1.36.5" {
		t.Fatalf("tag=%q err=%v", tag, err)
	}
	if _, err := KubectlTag("unknown"); err == nil {
		t.Fatal("accepted an unrecognized server version")
	}
	pod := &corev1.Pod{Spec: corev1.PodSpec{Volumes: []corev1.Volume{
		{Name: "kubectl", VolumeSource: corev1.VolumeSource{Image: &corev1.ImageVolumeSource{Reference: "registry.k8s.io/kubectl"}}},
		{Name: "pinned", VolumeSource: corev1.VolumeSource{Image: &corev1.ImageVolumeSource{Reference: "registry.k8s.io/kubectl:v1.35.0"}}},
		{Name: "go", VolumeSource: corev1.VolumeSource{Image: &corev1.ImageVolumeSource{Reference: "docker.io/library/golang"}}},
	}}}
	(&TaskReconciler{KubectlTag: tag}).pinKubectlImages(pod)
	for i, want := range []string{"registry.k8s.io/kubectl:v1.36.5", "registry.k8s.io/kubectl:v1.35.0", "docker.io/library/golang"} {
		if got := pod.Spec.Volumes[i].Image.Reference; got != want {
			t.Fatalf("volume %d: %s, want %s", i, got, want)
		}
	}
}

func TestFeedsCrawlerAddsWorkerEndpointAndToken(t *testing.T) {
	task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "team"}}
	agent := &agentsv1alpha1.Agent{Spec: agentsv1alpha1.AgentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{Name: "worker"}},
	}}}}
	r := &TaskReconciler{EnableFeeds: true, HarnessNamespace: "harness"}
	pod := r.buildWorkerPod("worker", task, agent, "worker", nil)
	var token bool
	for _, volume := range pod.Spec.Volumes {
		token = token || volume.Name == "agent-feeds-token" && volume.Projected.Sources[0].ServiceAccountToken.Audience == "agent-feeds-mcp"
	}
	var endpoint bool
	for _, env := range pod.Spec.Containers[0].Env {
		endpoint = endpoint || env.Name == "AGENT_MCP_ENDPOINTS" && strings.Contains(env.Value, `"name":"feeds","url":"http://agent-feeds.harness.svc.cluster.local:8080/mcp"`)
	}
	if !token || !endpoint {
		t.Fatalf("feeds MCP not attached: token=%v endpoint=%v", token, endpoint)
	}
	if r.workerConfigHash(task, agent) == (&TaskReconciler{}).workerConfigHash(task, agent) {
		t.Fatal("enabling feeds did not change the worker configuration hash")
	}
}
