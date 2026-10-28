package v1alpha1

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// The chart's agent-suspend admission policy lets the operator change only
// spec.suspend, so it must compare every other Agent spec field.
func TestAgentSuspendPolicyComparesEverySpecField(t *testing.T) {
	chart, err := os.ReadFile("../../../chart/templates/operator-deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	spec := reflect.TypeFor[AgentSpec]()
	for i := range spec.NumField() {
		name, _, _ := strings.Cut(spec.Field(i).Tag.Get("json"), ",")
		if name == "suspend" {
			continue
		}
		if !strings.Contains(string(chart), "object.spec.?"+name+" == oldObject.spec.?"+name+" ") &&
			!strings.Contains(string(chart), "object.spec.?"+name+" == oldObject.spec.?"+name+"\n") {
			t.Errorf("agent-suspend policy does not compare spec.%s", name)
		}
	}
}
