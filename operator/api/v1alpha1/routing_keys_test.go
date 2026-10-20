package v1alpha1

import "testing"

func TestTaskRoutingKeysDeepCopy(t *testing.T) {
	task := &Task{Spec: TaskSpec{RoutingKeys: [][]string{{"forge", "github", "https://github.com", "team", "repo", "issue", "1"}}}}
	copy := task.DeepCopy()
	copy.Spec.RoutingKeys[0][3] = "other"
	if task.Spec.RoutingKeys[0][3] != "team" {
		t.Fatal("routing key segments share backing storage after DeepCopy")
	}
}
