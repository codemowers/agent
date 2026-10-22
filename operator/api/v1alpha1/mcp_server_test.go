package v1alpha1

import "testing"

func TestServiceAudience(t *testing.T) {
	for url, want := range map[string]string{
		"http://agent-devices.harness.svc.cluster.local:8080/mcp": "agent-devices.harness",
		"http://drift.team.svc:3000/mcp":                          "drift.team",
		"http://agent-operator-wake.harness.svc.cluster.local":    "agent-operator-wake.harness",
		"https://mcp.codemowers.io/mcp":                           "",
		"http://a.b.cluster.local/mcp":                            "",
		"http://pod.a.b.svc.cluster.local/mcp":                    "",
		"ftp://agent-devices.harness.svc.cluster.local/mcp":       "",
	} {
		if got := ServiceAudience(url); got != want {
			t.Errorf("%s: got %q, want %q", url, got, want)
		}
	}
	cluster := ClusterMCPServerSpec{MCPServerSpec: MCPServerSpec{URL: "http://drift.team.svc/mcp"}, Audience: "http://drift.team.svc/mcp"}
	if cluster.TokenAudience() != "http://drift.team.svc/mcp" || cluster.MCPServerSpec.TokenAudience() != "" {
		t.Fatal("explicit cluster audience or token opt-in ignored")
	}
}
