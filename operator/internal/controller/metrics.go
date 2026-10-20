package controller

import (
	"context"
	"time"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TaskMetrics reads Task status on every scrape. No counter state is kept in
// the process, so an operator restart or leader handoff cannot skew counts.
type TaskMetrics struct {
	Reader client.Reader
	count  *prometheus.Desc
}

func NewTaskMetrics(reader client.Reader) *TaskMetrics {
	return &TaskMetrics{Reader: reader, count: prometheus.NewDesc(
		"agent_operator_tasks", "Current Tasks by namespace, Task phase, worker Pod state and worker activity.",
		[]string{"namespace", "task_phase", "pod_state", "worker_activity"}, nil)}
}

func (m *TaskMetrics) Describe(ch chan<- *prometheus.Desc) { ch <- m.count }

func (m *TaskMetrics) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var tasks agentsv1alpha1.TaskList
	if err := m.Reader.List(ctx, &tasks); err != nil {
		ch <- prometheus.NewInvalidMetric(m.count, err)
		return
	}
	type key struct{ namespace, phase, podState, activity string }
	counts := make(map[key]float64)
	for _, task := range tasks.Items {
		phase := string(task.Status.Phase)
		if phase == "" {
			phase = "Pending"
		}
		activity := task.Status.Activity
		podState := "stopped"
		if task.Status.WorkerPodName != "" {
			switch phase {
			case "Running":
				podState = "running"
			case "Pending":
				podState = "pending"
			}
		}
		if activity == "" {
			activity = "stopped"
			if podState == "running" || podState == "pending" {
				activity = "unknown"
			}
		}
		counts[key{task.Namespace, phase, podState, activity}]++
	}
	for key, count := range counts {
		ch <- prometheus.MustNewConstMetric(m.count, prometheus.GaugeValue, count, key.namespace, key.phase, key.podState, key.activity)
	}
}
