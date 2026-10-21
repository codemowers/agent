package v1alpha1

import "testing"

func TestTaskRoutingKeysDeepCopy(t *testing.T) {
	task := &Task{Spec: TaskSpec{RoutingKeys: []string{"https://github.com/team/repo/issues/1"}}}
	copy := task.DeepCopy()
	copy.Spec.RoutingKeys[0] = "other"
	if task.Spec.RoutingKeys[0] != "https://github.com/team/repo/issues/1" {
		t.Fatal("routing keys share backing storage after DeepCopy")
	}
}

func TestCanonicalKey(t *testing.T) {
	for raw, want := range map[string]string{
		"HTTPS://GitHub.com/team/repo/issues/1": "https://github.com/team/repo/issues/1",
		"http://am.example.test/":               "http://am.example.test",
		"https://git.example.test:3000/a/b/":    "https://git.example.test:3000/a/b",
		"mid:root@example.test":                 "mid:root@example.test",
	} {
		if got, err := CanonicalKey(raw); err != nil || got != want {
			t.Errorf("CanonicalKey(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "github", "https://u@github.com/a", "https://github.com/a?b=1", "https://github.com/a#b", "https://github.com/a/../b", "ftp://example.test/a", "https://github.com/a b"} {
		if got, err := CanonicalKey(raw); err == nil {
			t.Errorf("CanonicalKey(%q) = %q, want error", raw, got)
		}
	}
}

func TestKeyHasPrefix(t *testing.T) {
	key := "https://github.com/team/repo/issues/1"
	for prefix, want := range map[string]bool{
		"https://github.com":        true,
		"https://github.com/":       true,
		"https://github.com/team":   true,
		"https://github.com/te":     false,
		"https://github.com/team-x": false,
		key:                         true,
		"":                          false,
	} {
		if got := KeyHasPrefix(key, prefix); got != want {
			t.Errorf("KeyHasPrefix(%q) = %v, want %v", prefix, got, want)
		}
	}
	if !KeyHasPrefix("mid:root@example.test", "mid:") {
		t.Error("opaque scheme prefix did not match")
	}
}
