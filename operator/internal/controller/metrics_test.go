package controller

import (
	"testing"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestTaskMetricsCountsByNamespaceAndActivity(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objects := []agentsv1alpha1.Task{
		{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "one"}, Status: agentsv1alpha1.TaskStatus{Phase: agentsv1alpha1.TaskPhaseRunning, WorkerPodName: "pod-a", Activity: "working"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "one"}, Status: agentsv1alpha1.TaskStatus{Phase: agentsv1alpha1.TaskPhaseRunning, WorkerPodName: "pod-b", Activity: "working"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "two"}, Status: agentsv1alpha1.TaskStatus{Phase: agentsv1alpha1.TaskPhaseWaiting, Activity: "stopped"}},
	}
	store := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&objects[0], &objects[1], &objects[2]).Build()
	registry := prometheus.NewRegistry()
	if err := registry.Register(NewTaskMetrics(store)); err != nil {
		t.Fatal(err)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 1 || len(families[0].Metric) != 2 {
		t.Fatalf("wrong metric groups: %+v", families)
	}
	for _, metric := range families[0].Metric {
		labels := map[string]string{}
		for _, label := range metric.Label {
			labels[label.GetName()] = label.GetValue()
		}
		want := float64(1)
		if labels["namespace"] == "one" && labels["task_phase"] == "Running" && labels["pod_state"] == "running" && labels["worker_activity"] == "working" {
			want = 2
		}
		if metric.GetGauge().GetValue() != want {
			t.Fatalf("wrong count for %+v: %v", labels, metric.GetGauge().GetValue())
		}
	}
}
