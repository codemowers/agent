package controller

import (
	"context"
	"testing"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

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

func TestWorkerCredentialsStayInWorkerContainer(t *testing.T) {
	task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "team", UID: types.UID("task-uid")},
		Spec: agentsv1alpha1.TaskSpec{ExternalReferences: []agentsv1alpha1.ExternalReference{{
			Source: "github", ServerURL: "https://github.com", RepositoryOwner: "team", RepositoryName: "project", Kind: "issue", ID: "8",
		}}}}
	agent := &agentsv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "developer"},
		Spec: agentsv1alpha1.AgentSpec{
			Model:        agentsv1alpha1.ModelSpec{Backend: "claude", CredentialSecretRef: "agent-cred-model"},
			GitProviders: []agentsv1alpha1.GitProvider{{Type: "github", ServerURL: "https://github.com", CredentialSecretRef: "agent-cred-git"}},
			MCPEndpoints: []agentsv1alpha1.MCPEndpoint{{Name: "driftmower", URL: "http://driftmower-mcp/mcp", TokenFile: "/var/run/agent-mcp/driftmower"}},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers:     []corev1.Container{{Name: "worker", Image: "codemowers/agent:latest"}, {Name: "helper"}},
				InitContainers: []corev1.Container{{Name: "init"}},
			}},
		},
	}
	pod := (&TaskReconciler{AMQPURL: "amqp://broker", WorkerImage: "registry/agent:build"}).buildWorkerPod("worker-pod", task, agent, "agent-worker")
	if pod.Spec.Containers[0].Image != "registry/agent:build" {
		t.Fatal("worker did not use the configured runner image")
	}
	if len(pod.Spec.Containers[0].Env) == 0 {
		t.Fatal("worker did not receive execution environment")
	}
	if len(pod.Spec.Containers[1].Env) != 0 || len(pod.Spec.InitContainers[0].Env) != 0 {
		t.Fatal("execution credentials leaked to another container")
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
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "GH_TOKEN" && env.ValueFrom != nil && env.ValueFrom.SecretKeyRef.Name == "agent-cred-git" {
			gitCredential = true
		}
	}
	if !gitCredential {
		t.Fatal("configured Git provider credential was not selected")
	}
	task.Spec.ExternalReferences[0].ServerURL = "https://unconfigured.example.test"
	other := (&TaskReconciler{}).buildWorkerPod("other-pod", task, agent, "agent-worker")
	for _, env := range other.Spec.Containers[0].Env {
		if env.Name == "GH_TOKEN" {
			t.Fatal("Git credential forwarded to an unconfigured server")
		}
	}
}

func TestWorkerPodNameIncludesAgentAndIncarnation(t *testing.T) {
	name := workerPodName("matmlazi", "issue-12345", "ab12cd34")
	if name != "agent-matmlazi-issue-12-ab12cd34" {
		t.Fatalf("unexpected worker Pod name: %s", name)
	}
	longName := workerPodName("agent-with-a-very-long-name-that-needs-to-be-shortened-before-the-task-and-incarnation", "task-1234", "ab12cd34")
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
	store := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, pod).Build()
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
	if actual.Annotations["codemowers.cloud/retry-worker"] != "true" {
		t.Fatal("worker retry not recorded")
	}
}
