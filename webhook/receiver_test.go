package main

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

func TestExampleGiteaAndDiscordCEL(t *testing.T) {
	for _, sample := range []struct {
		protocol, binding, payload, eventType, wantSource, wantURL string
		wantKey                                                    []string
	}{
		{"gitea", "gitea-public", `{"repository":{"html_url":"https://gitea.com/team/repo","owner":{"login":"team"},"name":"repo"},"issue":{"number":7,"title":"Broken"},"sender":{"login":"alice"}}`, "issues", "gitea", "https://gitea.com/team/repo", []string{"forge", "gitea", "https://gitea.com", "team", "repo", "issue", "7"}},
		{"gitea-issue-comment", "gitea-public", `{"action":"created","repository":{"html_url":"https://gitea.com/team/repo","owner":{"login":"team"},"name":"repo"},"issue":{"number":7,"title":"Broken","body":"Investigate the certificate chain","pull_request":null},"comment":{"body":"bingo bongo","html_url":"https://gitea.com/team/repo/issues/7#issuecomment-6"},"is_pull":false,"sender":{"login":"alice"}}`, "issue_comment", "gitea", "https://gitea.com/team/repo/issues/7#issuecomment-6", []string{"forge", "gitea", "https://gitea.com", "team", "repo", "issue", "7"}},
		{"gitea-pr-comment", "gitea-public", `{"action":"created","repository":{"html_url":"https://gitea.com/team/repo","owner":{"login":"team"},"name":"repo"},"issue":{"number":8,"pull_request":null},"comment":{"body":"review"},"is_pull":true,"sender":{"login":"alice"}}`, "issue_comment", "gitea", "https://gitea.com/team/repo", []string{"forge", "gitea", "https://gitea.com", "team", "repo", "pull_request", "8"}},
		{"discord-events", "discord-app", `{"application_id":"123456789012345678","type":1,"event":{"type":"LOBBY_MESSAGE_CREATE","timestamp":"2026-10-18T00:00:00Z","data":{"content":"hello","author":{"username":"alice"}}}}`, "", "discord", "", nil},
	} {
		t.Run(sample.protocol, func(t *testing.T) {
			protocolName := sample.protocol
			if strings.HasPrefix(protocolName, "gitea-") {
				protocolName = "gitea"
			}
			protocol, _ := sampleReceiver(t, "ClusterWebhookReceiver", protocolName)
			_, binding := sampleReceiver(t, "WebhookReceiver", sample.binding)
			raw := []byte(sample.payload)
			var body any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			env, err := receiverEnv()
			if err != nil {
				t.Fatal(err)
			}
			headers := map[string]string{"x-gitea-event": sample.eventType, "x-gitea-event-type": "", "x-gitea-delivery": "delivery"}
			vars := map[string]any{"rawBody": raw, "body": body, "headers": headers, "binding": map[string]any{"match": binding.Spec.Match}, "event": map[string]any{}}
			if protocol.Spec.Event.AcceptCEL != "" {
				accepted, err := evalReceiver(env, protocol.Spec.Event.AcceptCEL, vars)
				if err != nil || accepted != true {
					t.Fatalf("accept=%v err=%v", accepted, err)
				}
			}
			value, err := evalReceiver(env, protocol.Spec.Event.EventsCEL, vars)
			if err != nil {
				t.Fatal(err)
			}
			events := value.([]any)
			if len(events) != 1 {
				t.Fatalf("events=%v", events)
			}
			normalized, err := normalizeReceiverEvent(binding, protocol, events[0].(map[string]any))
			if err != nil {
				t.Fatal(err)
			}
			if normalized.Source != sample.wantSource || normalized.URL != sample.wantURL || !equalKey(normalized.Key, sample.wantKey) {
				t.Fatalf("normalized=%+v", normalized)
			}
			if sample.protocol == "gitea-issue-comment" && !strings.Contains(normalized.Text, "Investigate the certificate chain\n\nComment:\nbingo bongo") {
				t.Fatalf("original post missing from event text: %q", normalized.Text)
			}
		})
	}
	protocol, _ := sampleReceiver(t, "ClusterWebhookReceiver", "discord-events")
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"application_id":"123456789012345678","type":0}`)
	request := httptest.NewRequest(http.MethodPost, "/hooks/discord-events", strings.NewReader(string(raw)))
	request.Header.Set("X-Signature-Timestamp", "1792310400")
	request.Header.Set("X-Signature-Ed25519", hex.EncodeToString(ed25519.Sign(private, append([]byte("1792310400"), raw...))))
	env, _ := receiverEnv()
	vars := map[string]any{"rawBody": raw, "headers": receiverHeaders(request), "body": map[string]any{}, "binding": map[string]any{"match": map[string]string{}}, "event": map[string]any{}}
	valid, err := checkReceiverSignature(request, protocol, []byte(hex.EncodeToString(public)), raw, vars, env)
	if err != nil || !valid {
		t.Fatalf("discord signature valid=%v err=%v", valid, err)
	}
}

func TestExampleSlackIgnoresBotButKeepsHumanThreadReply(t *testing.T) {
	protocol, _ := sampleReceiver(t, "ClusterWebhookReceiver", "slack-events")
	_, binding := sampleReceiver(t, "WebhookReceiver", "slack-codemowers")
	env, err := receiverEnv()
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct {
		name, payload string
		wantEvents    int
	}{
		{"bot", `{"type":"event_callback","team_id":"T0BTAN5EF5E","event_id":"bot-1","event":{"type":"message","subtype":"bot_message","bot_id":"B1","channel":"C1","ts":"2"}}`, 0},
		{"human mention", `{"type":"event_callback","team_id":"T0BTAN5EF5E","event_id":"human-1","event":{"type":"message","user":"U1","text":"<@UAGENT> please investigate","channel":"C1","ts":"2.1","thread_ts":"1"}}`, 1},
	} {
		t.Run(sample.name, func(t *testing.T) {
			var body any
			if err := json.Unmarshal([]byte(sample.payload), &body); err != nil {
				t.Fatal(err)
			}
			vars := map[string]any{"rawBody": []byte(sample.payload), "body": body,
				"headers": map[string]string{}, "binding": map[string]any{"match": binding.Spec.Match}, "event": map[string]any{}}
			accepted, err := evalReceiver(env, protocol.Spec.Event.AcceptCEL, vars)
			if err != nil || accepted != true {
				t.Fatalf("accept=%v err=%v", accepted, err)
			}
			value, err := evalReceiver(env, protocol.Spec.Event.EventsCEL, vars)
			if err != nil {
				t.Fatal(err)
			}
			events := value.([]any)
			if len(events) != sample.wantEvents {
				t.Fatalf("events=%v", events)
			}
			if sample.wantEvents == 1 {
				normalized, err := normalizeReceiverEvent(binding, protocol, events[0].(map[string]any))
				if err != nil || !equalKey(normalized.Key, []string{"slack", "T0BTAN5EF5E", "C1", "1"}) {
					t.Fatalf("normalized=%+v err=%v", normalized, err)
				}
			}
		})
	}
}

func sampleReceiver(t *testing.T, kind, name string) (receiverProtocol, receiverBinding) {
	t.Helper()
	prefix := "receiver-"
	if kind == "ClusterWebhookReceiver" {
		prefix = "cluster-"
	}
	contents, err := os.ReadFile("../examples/webhooks/" + prefix + name + ".yaml")
	if os.IsNotExist(err) {
		contents, err = os.ReadFile("../examples/webhooks/optional/" + prefix + name + ".yaml")
	}
	if err != nil {
		t.Fatal(err)
	}
	var protocol receiverProtocol
	var binding receiverBinding
	var doc map[string]any
	if err := yaml.Unmarshal(contents, &doc); err != nil {
		t.Fatal(err)
	}
	metadata, _ := doc["metadata"].(map[string]any)
	if doc["kind"] != kind || metadata["name"] != name {
		t.Fatalf("wrong CR in %s%s.yaml", prefix, name)
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if kind == "ClusterWebhookReceiver" {
		if err = json.Unmarshal(encoded, &protocol); err != nil {
			t.Fatal(err)
		}
	} else {
		if err = json.Unmarshal(encoded, &binding); err != nil {
			t.Fatal(err)
		}
	}
	if kind == "ClusterWebhookReceiver" && protocol.Spec.Event.EventsCEL == "" {
		t.Fatalf("missing protocol %s", name)
	}
	if kind == "WebhookReceiver" && binding.Spec.AgentRef == "" {
		t.Fatalf("missing receiver %s", name)
	}
	return protocol, binding
}
func TestExampleGitHubCEL(t *testing.T) {
	protocol, _ := sampleReceiver(t, "ClusterWebhookReceiver", "github")
	_, binding := sampleReceiver(t, "WebhookReceiver", "github-codemowers-agent")
	body := []byte(`{"repository":{"html_url":"https://github.com/codemowers/agent","owner":{"login":"codemowers"},"name":"agent"},"issue":{"number":42,"title":"Fix bug","html_url":"https://github.com/codemowers/agent/issues/42"},"sender":{"login":"lauri"},"action":"opened"}`)
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	env, err := receiverEnv()
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]any{"rawBody": body, "body": parsed, "headers": map[string]string{"x-github-hook-id": "698893022", "x-github-delivery": "delivery", "x-github-event": "issues"}, "binding": map[string]any{"match": binding.Spec.Match}, "event": map[string]any{}}
	accept, err := evalReceiver(env, protocol.Spec.Event.AcceptCEL, vars)
	if err != nil || accept != true {
		t.Fatalf("accept=%v err=%v", accept, err)
	}
	result, err := evalReceiver(env, protocol.Spec.Event.EventsCEL, vars)
	if err != nil {
		t.Fatal(err)
	}
	events, ok := result.([]any)
	if !ok || len(events) != 1 {
		t.Fatalf("events=%T %v", result, result)
	}
	event, ok := events[0].(map[string]any)
	if !ok {
		t.Fatalf("event=%T %v", events[0], events[0])
	}
	normalized, err := normalizeReceiverEvent(binding, protocol, event)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"forge", "github", "https://github.com", "codemowers", "agent", "issue", "42"}
	if !equalKey(normalized.Key, want) || normalized.URL != "https://github.com/codemowers/agent/issues/42" {
		t.Fatalf("normalized=%+v want key=%v", normalized, want)
	}
	event["url"] = "javascript:alert(1)"
	withoutUnsafeURL, err := normalizeReceiverEvent(binding, protocol, event)
	if err != nil || withoutUnsafeURL.URL != "" {
		t.Fatalf("unsafe event URL was retained: %+v err=%v", withoutUnsafeURL, err)
	}
	vars["headers"].(map[string]string)["x-github-hook-id"] = "other"
	accept, err = evalReceiver(env, protocol.Spec.Event.AcceptCEL, vars)
	if err != nil || accept != false {
		t.Fatalf("wrong hook accepted=%v err=%v", accept, err)
	}
}
func equalKey(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func TestExampleSlackSignatureAndChallenge(t *testing.T) {
	protocol, _ := sampleReceiver(t, "ClusterWebhookReceiver", "slack-events")
	raw := []byte(`{"type":"url_verification","challenge":"challenge-value"}`)
	request := httptest.NewRequest(http.MethodPost, "/hooks/team/slack", strings.NewReader(string(raw)))
	request.Header.Set("X-Slack-Request-Timestamp", "1792310400")
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write([]byte("v0:1792310400:"))
	mac.Write(raw)
	request.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	var body any
	_ = json.Unmarshal(raw, &body)
	env, err := receiverEnv()
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]any{"rawBody": raw, "body": body, "headers": receiverHeaders(request), "binding": map[string]any{"match": map[string]string{}}, "event": map[string]any{}}
	// Freshness is tested independently; use the sample expression here.
	protocol.Spec.Verification.Freshness = nil
	valid, err := checkReceiverSignature(request, protocol, []byte("secret"), raw, vars, env)
	if err != nil || !valid {
		t.Fatalf("signature valid=%v err=%v", valid, err)
	}
	result, err := evalReceiver(env, protocol.Spec.Event.EventsCEL, vars)
	if err != nil {
		t.Fatal(err)
	}
	if events, ok := result.([]any); !ok || len(events) != 0 {
		t.Fatalf("challenge enqueued: %v", result)
	}
	value, err := evalReceiver(env, protocol.Spec.Response.Rules[0].BodyCEL, vars)
	if err != nil || value != "challenge-value" {
		t.Fatalf("challenge=%v err=%v", value, err)
	}
}
func TestExampleAlertmanagerEvents(t *testing.T) {
	protocol, _ := sampleReceiver(t, "ClusterWebhookReceiver", "alertmanager")
	_, binding := sampleReceiver(t, "WebhookReceiver", "alerts-by-instance")
	// One notification for a large group yields one event with a bounded ID.
	alerts := make([]map[string]any, 150)
	for i := range alerts {
		alerts[i] = map[string]any{"fingerprint": fmt.Sprintf("%016x", i), "status": "firing", "labels": map[string]string{"alertname": "CPUHigh"}}
	}
	raw, _ := json.Marshal(map[string]any{
		"status":            "firing",
		"groupKey":          `{}/{}:{alertname="ContainerHighThrottleRate", cluster="kubernetes", region="ee-lte-1", severity="warning"}`,
		"externalURL":       "https://alertmanager.example.test",
		"commonLabels":      map[string]string{"alertname": "CPUHigh"},
		"commonAnnotations": map[string]string{"summary": "CPU high"},
		"alerts":            alerts,
	})
	var body any
	_ = json.Unmarshal(raw, &body)
	env, err := receiverEnv()
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]any{"rawBody": raw, "body": body, "headers": map[string]string{}, "binding": map[string]any{"match": binding.Spec.Match}, "event": map[string]any{}}
	result, err := evalReceiver(env, protocol.Spec.Event.EventsCEL, vars)
	if err != nil {
		t.Fatal(err)
	}
	events := result.([]any)
	if len(events) != 1 {
		t.Fatalf("events=%d", len(events))
	}
	event := events[0].(map[string]any)
	vars["event"] = event
	key, err := evalReceiver(env, binding.Spec.Routing.KeyCEL, vars)
	if err != nil {
		t.Fatal(err)
	}
	event["key"] = key
	normalized, err := normalizeReceiverEvent(binding, protocol, event)
	if err != nil {
		t.Fatal(err)
	}
	instance := binding.Spec.Match["instanceID"]
	if instance == "" || !equalKey(normalized.Key, []string{"alertmanager", instance}) || normalized.URL != "https://alertmanager.example.test" ||
		normalized.Text != "[firing:150] CPUHigh: CPU high" {
		t.Fatalf("normalized=%+v", normalized)
	}
}
