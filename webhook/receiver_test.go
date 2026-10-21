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

// loadExample decodes an example manifest of the given kind into out.
func loadExample(t *testing.T, path, kind string, out any) {
	t.Helper()
	contents, err := os.ReadFile("../examples/webhooks/" + path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(contents, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["kind"] != kind {
		t.Fatalf("%s is a %v, not a %s", path, doc["kind"], kind)
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, out); err != nil {
		t.Fatal(err)
	}
}
func exampleProtocol(t *testing.T, path string) receiverProtocol {
	var v receiverProtocol
	loadExample(t, path, "WebhookProtocol", &v)
	return v
}
func exampleReceiver(t *testing.T, path, kind string) webhookReceiver {
	var v webhookReceiver
	loadExample(t, path, kind, &v)
	return v
}
func exampleBinding(t *testing.T, path string) receiverBinding {
	var v receiverBinding
	loadExample(t, path, "WebhookBinding", &v)
	return v
}
func receiverVars(raw []byte, body any, headers map[string]string, receiver webhookReceiver) map[string]any {
	return map[string]any{"rawBody": raw, "body": body, "headers": headers, "receiver": map[string]any{"match": receiver.Spec.Match}, "event": map[string]any{}}
}
func evalEvents(t *testing.T, protocol receiverProtocol, vars map[string]any) []any {
	t.Helper()
	env, err := receiverEnv()
	if err != nil {
		t.Fatal(err)
	}
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
	return value.([]any)
}

func TestExampleGiteaAndDiscordCEL(t *testing.T) {
	gitea := exampleProtocol(t, "protocol-gitea.yaml")
	public := exampleReceiver(t, "optional/receiver-gitea-public.yaml", "WebhookReceiver")
	binding := exampleBinding(t, "optional/binding-gitea-public.yaml")
	for _, sample := range []struct {
		name, payload, eventType, wantType, wantURL, wantKey string
	}{
		{"issue", `{"repository":{"html_url":"https://gitea.com/team/repo","full_name":"Team/Repo"},"issue":{"number":7,"title":"Broken"},"sender":{"login":"alice"}}`, "issues", "issues", "https://gitea.com/team/repo", "https://gitea.com/team/repo/issues/7"},
		{"issue-comment", `{"action":"created","repository":{"html_url":"https://gitea.com/team/repo","full_name":"Team/Repo"},"issue":{"number":7,"title":"Broken","body":"Investigate the certificate chain","pull_request":null},"comment":{"body":"bingo bongo","html_url":"https://gitea.com/team/repo/issues/7#issuecomment-6"},"is_pull":false,"sender":{"login":"alice"}}`, "issue_comment", "issue_comment.created", "https://gitea.com/team/repo/issues/7#issuecomment-6", "https://gitea.com/team/repo/issues/7"},
		{"pr-comment", `{"action":"created","repository":{"html_url":"https://gitea.com/team/repo","full_name":"Team/Repo"},"issue":{"number":8,"pull_request":null},"comment":{"body":"review"},"is_pull":true,"sender":{"login":"alice"}}`, "issue_comment", "issue_comment.created", "https://gitea.com/team/repo", "https://gitea.com/team/repo/issues/8"},
		{"push", `{"repository":{"html_url":"https://gitea.com/team/repo","full_name":"Team/Repo"},"sender":{"login":"alice"}}`, "push", "push", "https://gitea.com/team/repo", "https://gitea.com/team/repo"},
	} {
		t.Run(sample.name, func(t *testing.T) {
			raw := []byte(sample.payload)
			var body any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			headers := map[string]string{"x-gitea-event": sample.eventType, "x-gitea-event-type": "", "x-gitea-delivery": "delivery"}
			events := evalEvents(t, gitea, receiverVars(raw, body, headers, public))
			if len(events) != 1 {
				t.Fatalf("events=%v", events)
			}
			normalized, err := normalizeReceiverEvent(binding, gitea, events[0].(map[string]any))
			if err != nil {
				t.Fatal(err)
			}
			if normalized.Type != sample.wantType || normalized.URL != sample.wantURL || normalized.Key != sample.wantKey {
				t.Fatalf("normalized=%+v", normalized)
			}
			if sample.name == "issue-comment" && !strings.Contains(normalized.Text, "Investigate the certificate chain\n\nComment:\nbingo bongo") {
				t.Fatalf("original post missing from event text: %q", normalized.Text)
			}
		})
	}
	discord := exampleProtocol(t, "optional/protocol-discord-events.yaml")
	app := exampleReceiver(t, "optional/receiver-discord-app.yaml", "WebhookReceiver")
	raw := []byte(`{"application_id":"123456789012345678","type":1,"event":{"type":"LOBBY_MESSAGE_CREATE","timestamp":"2026-10-18T00:00:00Z","data":{"content":"hello","author":{"username":"alice"}}}}`)
	var body any
	_ = json.Unmarshal(raw, &body)
	if events := evalEvents(t, discord, receiverVars(raw, body, map[string]string{}, app)); len(events) != 1 {
		t.Fatalf("discord events=%v", events)
	}
	public2, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/hooks/agents/discord-app", strings.NewReader(string(raw)))
	request.Header.Set("X-Signature-Timestamp", "1792310400")
	request.Header.Set("X-Signature-Ed25519", hex.EncodeToString(ed25519.Sign(private, append([]byte("1792310400"), raw...))))
	env, _ := receiverEnv()
	valid, err := checkReceiverSignature(request, discord, public2, raw, receiverVars(raw, body, receiverHeaders(request), app), env)
	if err != nil || !valid {
		t.Fatalf("discord signature valid=%v err=%v", valid, err)
	}
}

func TestExampleSlackIgnoresBotButKeepsHumanThreadReply(t *testing.T) {
	protocol := exampleProtocol(t, "protocol-slack-events.yaml")
	receiver := exampleReceiver(t, "receiver-slack-codemowers.yaml", "WebhookReceiver")
	binding := exampleBinding(t, "binding-slack-codemowers.yaml")
	for _, sample := range []struct {
		name, payload string
		wantEvents    int
	}{
		{"bot", `{"type":"event_callback","team_id":"T0BTAN5EF5E","event_id":"bot-1","event":{"type":"message","subtype":"bot_message","bot_id":"B1","channel":"C1","ts":"2"}}`, 0},
		{"human mention", `{"type":"event_callback","team_id":"T0BTAN5EF5E","event_id":"human-1","event":{"type":"message","user":"U1","text":"<@UAGENT> please investigate","channel":"C1","ts":"2.1","thread_ts":"1.0"}}`, 1},
	} {
		t.Run(sample.name, func(t *testing.T) {
			var body any
			if err := json.Unmarshal([]byte(sample.payload), &body); err != nil {
				t.Fatal(err)
			}
			events := evalEvents(t, protocol, receiverVars([]byte(sample.payload), body, map[string]string{}, receiver))
			if len(events) != sample.wantEvents {
				t.Fatalf("events=%v", events)
			}
			if sample.wantEvents == 1 {
				normalized, err := normalizeReceiverEvent(binding, protocol, events[0].(map[string]any))
				if err != nil || normalized.Key != "https://slack.com/archives/C1/p10" {
					t.Fatalf("normalized=%+v err=%v", normalized, err)
				}
			}
		})
	}
}

func TestExampleGitHubCEL(t *testing.T) {
	protocol := exampleProtocol(t, "protocol-github.yaml")
	receiver := exampleReceiver(t, "clusterreceiver-github.yaml", "ClusterWebhookReceiver")
	binding := exampleBinding(t, "binding-github-codemowers-agent.yaml")
	raw := []byte(`{"repository":{"html_url":"https://github.com/codemowers/agent","full_name":"Codemowers/Agent"},"issue":{"number":42,"title":"Fix bug","html_url":"https://github.com/codemowers/agent/issues/42"},"sender":{"login":"lauri"},"action":"opened"}`)
	var body any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	events := evalEvents(t, protocol, receiverVars(raw, body, map[string]string{"x-github-delivery": "delivery", "x-github-event": "issues"}, receiver))
	if len(events) != 1 {
		t.Fatalf("events=%v", events)
	}
	event := events[0].(map[string]any)
	normalized, err := normalizeReceiverEvent(binding, protocol, event)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Key != "https://github.com/codemowers/agent/issues/42" || normalized.Type != "issues.opened" || normalized.URL != "https://github.com/codemowers/agent/issues/42" {
		t.Fatalf("normalized=%+v", normalized)
	}
	event["url"] = "javascript:alert(1)"
	if withoutUnsafeURL, err := normalizeReceiverEvent(binding, protocol, event); err != nil || withoutUnsafeURL.URL != "" {
		t.Fatalf("unsafe event URL was retained: %+v err=%v", withoutUnsafeURL, err)
	}
	// Events of another server do not match this receiver.
	other := []byte(`{"repository":{"html_url":"https://github.enterprise.example/a/b","full_name":"a/b"}}`)
	var otherBody any
	_ = json.Unmarshal(other, &otherBody)
	env, _ := receiverEnv()
	if accepted, err := evalReceiver(env, protocol.Spec.Event.AcceptCEL, receiverVars(other, otherBody, map[string]string{"x-github-event": "push"}, receiver)); err != nil || accepted != false {
		t.Fatalf("other server accepted=%v err=%v", accepted, err)
	}
}

func TestEventKeyMustStayInsideProtocolPrefixes(t *testing.T) {
	protocol := exampleProtocol(t, "protocol-github.yaml")
	binding := exampleBinding(t, "binding-github-codemowers-agent.yaml")
	for key, ok := range map[string]bool{
		"https://GitHub.com/team/repo/issues/1":   true,
		"https://github.com.evil.test/a/issues/1": false,
		"https://git.codemowers.io/a/b/issues/1":  false,
		"mid:root@example.test":                   false,
	} {
		_, err := normalizeReceiverEvent(binding, protocol, map[string]any{"deliveryID": "d", "type": "issues", "key": key})
		if (err == nil) != ok {
			t.Errorf("key %q: err=%v", key, err)
		}
	}
	if _, err := normalizeReceiverEvent(binding, protocol, map[string]any{"deliveryID": "d", "type": "issues", "key": []any{"forge"}}); err == nil {
		t.Error("list key accepted")
	}
}

func TestExampleSlackSignatureAndChallenge(t *testing.T) {
	protocol := exampleProtocol(t, "protocol-slack-events.yaml")
	receiver := exampleReceiver(t, "receiver-slack-codemowers.yaml", "WebhookReceiver")
	raw := []byte(`{"type":"url_verification","challenge":"challenge-value"}`)
	request := httptest.NewRequest(http.MethodPost, "/hooks/agents/slack-codemowers", strings.NewReader(string(raw)))
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
	vars := receiverVars(raw, body, receiverHeaders(request), receiver)
	// Freshness is tested independently; use the sample expression here.
	protocol.Spec.Signature.Freshness = nil
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
	protocol := exampleProtocol(t, "protocol-alertmanager.yaml")
	receiver := exampleReceiver(t, "receiver-alertmanager.yaml", "WebhookReceiver")
	binding := exampleBinding(t, "binding-alertmanager.yaml")
	// One notification for a large group yields one event with a bounded ID.
	alerts := make([]map[string]any, 150)
	for i := range alerts {
		alerts[i] = map[string]any{"fingerprint": fmt.Sprintf("%016x", i), "status": "firing", "labels": map[string]string{"alertname": "CPUHigh"}}
	}
	raw, _ := json.Marshal(map[string]any{
		"status":            "firing",
		"externalURL":       "https://alertmanager.example.test",
		"commonLabels":      map[string]string{"alertname": "CPUHigh"},
		"commonAnnotations": map[string]string{"summary": "CPU high"},
		"alerts":            alerts,
	})
	var body any
	_ = json.Unmarshal(raw, &body)
	events := evalEvents(t, protocol, receiverVars(raw, body, map[string]string{}, receiver))
	if len(events) != 1 {
		t.Fatalf("events=%d", len(events))
	}
	normalized, err := normalizeReceiverEvent(binding, protocol, events[0].(map[string]any))
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Key != "https://alertmanager.example.test" || normalized.URL != "https://alertmanager.example.test" ||
		normalized.Text != "[firing:150] CPUHigh: CPU high" {
		t.Fatalf("normalized=%+v", normalized)
	}
}
