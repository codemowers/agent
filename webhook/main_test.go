package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fixedHookResolver []agentHook

func (hooks fixedHookResolver) Resolve(_ context.Context, _ string) ([]agentHook, error) {
	return hooks, nil
}

func TestSignedGitHubPingIsAcceptedWithoutRepository(t *testing.T) {
	body := `{"zen":"Keep it logically awesome."}`
	mac := hmac.New(sha256.New, []byte("hook-key"))
	mac.Write([]byte(body))
	request := httptest.NewRequest(http.MethodPost, "/hooks/github", strings.NewReader(body))
	request.Header.Set("X-GitHub-Hook-ID", "42")
	request.Header.Set("X-GitHub-Delivery", "delivery-1")
	request.Header.Set("X-GitHub-Event", "ping")
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	receiver := &publisher{resolver: fixedHookResolver{{HookID: "42", ServerURL: "https://github.com", Secret: []byte("hook-key")}}}
	response := httptest.NewRecorder()
	receiver.githubHandler(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("signed ping returned %d: %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/hooks/github", strings.NewReader(body))
	request.Header.Set("X-GitHub-Hook-ID", "42")
	request.Header.Set("X-GitHub-Delivery", "delivery-1")
	request.Header.Set("X-GitHub-Event", "ping")
	request.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
	response = httptest.NewRecorder()
	receiver.githubHandler(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned ping returned %d", response.Code)
	}
}

func TestGitHubSubscriptionKeyMustValidateDelivery(t *testing.T) {
	body := []byte(`{"repository":{"full_name":"codemowers/agent"}}`)
	mac := hmac.New(sha256.New, []byte("real-hook-key"))
	mac.Write(body)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !validHMAC([]byte("real-hook-key"), signature, "sha256=", body) {
		t.Fatal("valid subscription key rejected")
	}
	if validHMAC([]byte("other-namespace-key"), signature, "sha256=", body) ||
		validHMAC([]byte("real-hook-key"), signature, "sha256=", []byte(`{}`)) {
		t.Fatal("invalid subscription received the delivery")
	}
}

func TestSlackRoutingKey(t *testing.T) {
	cases := map[string]string{
		`{"type":"event_callback","team_id":"T1","event_id":"E1","event":{"type":"message","channel":"C1","ts":"1.1"}}`:                   "slack:T1:C1:1.1",
		`{"type":"event_callback","team_id":"T1","event_id":"E2","event":{"type":"message","channel":"C1","ts":"2.2","thread_ts":"1.1"}}`: "slack:T1:C1:1.1",
		`{"type":"event_callback","team_id":"T1","event_id":"E3","event":{"channel_type":"im","user":"U9","channel":"D1","ts":"3"}}`:      "slack:T1:dm:U9",
		`{"type":"event_callback","team_id":"T1","event_id":"E4","event":{"bot_id":"B1","channel":"C1","ts":"4"}}`:                        "",
	}
	for body, want := range cases {
		got, _, _, err := slackRoutingKey([]byte(body))
		if err != nil || got != want {
			t.Errorf("%s: got %q err %v, want %q", body, got, err, want)
		}
	}
	_, _, ch, _ := slackRoutingKey([]byte(`{"type":"url_verification","challenge":"abc"}`))
	if ch != "abc" {
		t.Errorf("challenge %q", ch)
	}
}

func TestSplitAlerts(t *testing.T) {
	ev, err := splitAlerts([]byte(`{"groupKey":"g","alerts":[{"fingerprint":"a","status":"firing"},{"fingerprint":"b","status":"resolved"}]}`))
	if err != nil || len(ev) != 2 || ev[0].key != "alertmanager:a" || ev[1].key != "alertmanager:b" {
		t.Fatalf("%v %v", ev, err)
	}
}

func TestGiteaRoutingKeyUsesSignedRepositoryHost(t *testing.T) {
	key, err := giteaRoutingKey("issues", []byte(`{"repository":{"full_name":"team/project","html_url":"https://forge.example.test/team/project"},"issue":{"number":8}}`))
	if err != nil || key != "gitea.forge.example.test:team/project#issue:8" {
		t.Fatalf("got %q, %v", key, err)
	}
	if _, err := giteaRoutingKey("issues", []byte(`{"repository":{"full_name":"team/project"},"issue":{"number":8}}`)); err == nil {
		t.Fatal("missing signed repository host was accepted")
	}
}

func TestGithubEnterpriseHostMustMatchSignedRepository(t *testing.T) {
	body := []byte(`{"repository":{"full_name":"team/project","html_url":"https://ghe.example.test/team/project"},"issue":{"number":8}}`)
	key, err := githubRoutingKey("ghe.example.test", "issues", body)
	if err != nil || key != "github.ghe.example.test:team/project#issue:8" {
		t.Fatalf("got %q, %v", key, err)
	}
	if _, err := githubRoutingKey("other.example.test", "issues", body); err == nil {
		t.Fatal("unsigned enterprise host override was accepted")
	}
}

func TestPullRequestTimelineCommentRoutesToPullRequest(t *testing.T) {
	body := []byte(`{"repository":{"full_name":"team/project","html_url":"https://github.com/team/project"},"issue":{"number":8,"pull_request":{"url":"https://api.github.com/repos/team/project/pulls/8"}}}`)
	key, err := githubRoutingKey("", "issue_comment", body)
	if err != nil || key != "github:team/project#pr:8" {
		t.Fatalf("got %q, %v", key, err)
	}
}

func TestAgentHookMatchesConfiguredForgeHost(t *testing.T) {
	hook := agentHook{ServerURL: "https://github.com", GitProviders: []struct {
		Type      string `json:"type"`
		ServerURL string `json:"serverURL"`
	}{{Type: "github", ServerURL: "https://github.com"}}}
	if !hook.allowsRepository("github", []byte(`{"repository":{"full_name":"codemowers/agent","html_url":"https://github.com/codemowers/agent"}}`)) {
		t.Fatal("configured repository host was rejected")
	}
	if hook.allowsRepository("github", []byte(`{"repository":{"full_name":"codemowers/agent","html_url":"https://other.example.test/codemowers/agent"}}`)) ||
		hook.allowsRepository("gitea", []byte(`{"repository":{"full_name":"codemowers/agent","html_url":"https://github.com/codemowers/agent"}}`)) {
		t.Fatal("repository outside configured Git provider was accepted")
	}
	hook.GitProviders = append(hook.GitProviders, struct {
		Type      string `json:"type"`
		ServerURL string `json:"serverURL"`
	}{Type: "github", ServerURL: "https://ghe.example.test"})
	if hook.allowsRepository("github", []byte(`{"repository":{"full_name":"codemowers/agent","html_url":"https://ghe.example.test/codemowers/agent"}}`)) {
		t.Fatal("colliding hook ID from another GitHub server was accepted")
	}
	hook.ServerURL = "https://ghe.example.test"
	if !hook.allowsRepository("github", []byte(`{"repository":{"full_name":"codemowers/agent","html_url":"https://ghe.example.test/codemowers/agent"}}`)) {
		t.Fatal("GHES subscription rejected its own server")
	}
}

func TestWebhookAgentNamesRejectPathSegments(t *testing.T) {
	for _, name := range []string{"../other", "agent/other", "agent%2Fother", ""} {
		if validKubernetesName(name) {
			t.Fatalf("unsafe Agent name %q accepted", name)
		}
	}
}

func TestSecretForAgentRejectsPathSegments(t *testing.T) {
	for _, name := range []string{"../other", "agent/other", "agent%2Fother", ""} {
		if got := secretForAgent(name); got != nil {
			t.Fatalf("unsafe Agent path %q accepted", name)
		}
	}
}
