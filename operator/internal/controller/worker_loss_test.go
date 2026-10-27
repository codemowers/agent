package controller

import (
	"context"
	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func TestUnexpectedWorkerLossSuspendsDespiteQueuedMessages(t *testing.T) {
	for _, missing := range []bool{false, true} {
		name := "failed"
		if missing {
			name = "missing"
		}
		t.Run(name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			_ = agentsv1alpha1.AddToScheme(scheme)
			task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "team", UID: types.UID("task-uid")}, Status: agentsv1alpha1.TaskStatus{WorkerPodName: "worker", WorkerPodUID: "pod-uid", Events: agentsv1alpha1.TaskEvents{WakePending: true}}}
			builder := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(task).WithObjects(task)
			if !missing {
				builder = builder.WithObjects(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "team", UID: "pod-uid", OwnerReferences: []metav1.OwnerReference{taskOwnerRef(task)}}, Status: corev1.PodStatus{Phase: corev1.PodFailed}})
			}
			store := builder.Build()
			r := &TaskReconciler{Client: store}
			if _, err := r.reconcileWorkerPod(context.Background(), task, &agentsv1alpha1.Agent{}); err != nil {
				t.Fatal(err)
			}
			var actual agentsv1alpha1.Task
			if err := store.Get(context.Background(), client.ObjectKeyFromObject(task), &actual); err != nil {
				t.Fatal(err)
			}
			if !actual.Spec.Suspend || actual.Status.Phase != agentsv1alpha1.TaskPhasePaused || !actual.Status.Events.WakePending {
				t.Fatalf("worker loss did not preserve queued work in suspension: %+v", actual)
			}
		})
	}
}
