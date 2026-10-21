package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeResolver struct {
	protocols map[string]receiverProtocol
	receivers map[string]webhookReceiver
	bindings  []receiverBinding
	keys      map[string][]byte
	tokens    map[string]string
}

func (f fakeResolver) Protocol(_ context.Context, name string) (receiverProtocol, error) {
	return f.protocols[name], nil
}
func (f fakeResolver) Receiver(_ context.Context, namespace, name string) (webhookReceiver, error) {
	r, ok := f.receivers[namespace+"/"+name]
	if !ok {
		return r, errNotFound
	}
	return r, nil
}
func (f fakeResolver) Bindings(_ context.Context, r webhookReceiver) ([]receiverBinding, error) {
	var out []receiverBinding
	for _, b := range f.bindings {
		if b.subscribes(r) {
			out = append(out, b)
		}
	}
	return out, nil
}
func (f fakeResolver) ReceiverKey(_ context.Context, r webhookReceiver) ([]byte, error) {
	ref := r.Spec.Senders.SecretKeyRef
	return f.keys[ref.Namespace+"/"+ref.Name+"/"+ref.Key], nil
}
func (f fakeResolver) ReviewToken(_ context.Context, token, audience string) (string, error) {
	if audience != "agent-webhook" {
		return "", nil
	}
	return f.tokens[token], nil
}

func uidBinding(t *testing.T, path, namespace, uid string) receiverBinding {
	b := exampleBinding(t, path)
	b.Metadata.Namespace, b.Metadata.UID = namespace, uid
	return b
}

func githubRequest(t *testing.T, secret, body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/hooks/github", strings.NewReader(body))
	request.SetPathValue("receiver", "github")
	request.Header.Set("X-GitHub-Event", "issues")
	request.Header.Set("X-GitHub-Delivery", "delivery-1")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	request.RemoteAddr = "[fd10::1]:4000"
	request.Header.Set("X-Forwarded-For", "203.0.113.9, 192.30.252.10")
	return request
}

func TestClusterReceiverFansOutByKeyPrefix(t *testing.T) {
	github := exampleReceiver(t, "clusterreceiver-github.yaml", "ClusterWebhookReceiver")
	codemowers := uidBinding(t, "binding-github-codemowers-agent.yaml", "agents", "b1")
	repoScoped := uidBinding(t, "binding-github-codemowers-agent.yaml", "team", "b2")
	repoScoped.Metadata.Name = "other-repo"
	repoScoped.Spec.KeyPrefixes = []string{"https://github.com/codemowers/other"}
	unfiltered := uidBinding(t, "binding-github-codemowers-agent.yaml", "other", "b3")
	unfiltered.Spec.KeyPrefixes = nil
	resolver := fakeResolver{
		protocols: map[string]receiverProtocol{"github": exampleProtocol(t, "protocol-github.yaml")},
		receivers: map[string]webhookReceiver{"/github": github},
		bindings:  []receiverBinding{codemowers, repoScoped, unfiltered},
		keys:      map[string][]byte{"agents/agent-operator-webhook-keys/github-public": []byte("hook-secret")},
	}
	meta := newGitHubMetaCache(nil)
	meta.fetch = func(context.Context, string, string) ([]string, string, bool, error) {
		return []string{"192.30.252.0/22"}, "", false, nil
	}
	var published []normalizedEvent
	p := &publisher{resolver: resolver, githubMeta: meta, trustedProxies: []netip.Prefix{netip.MustParsePrefix("fd10::/16")},
		publish: func(event normalizedEvent, _ string, _ []byte) error {
			published = append(published, event)
			return nil
		}}
	body := `{"action":"opened","repository":{"html_url":"https://github.com/codemowers/agent","full_name":"codemowers/agent"},"issue":{"number":1,"title":"t","html_url":"https://github.com/codemowers/agent/issues/1"},"sender":{"login":"alice"}}`

	response := httptest.NewRecorder()
	p.receiverHandler(response, githubRequest(t, "hook-secret", body))
	if response.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", response.Code, response.Body.String())
	}
	// Only the org-wide binding matches; the repository-scoped binding is for
	// another repository and the unfiltered one may not use a cluster receiver.
	if len(published) != 1 || published[0].Namespace != "agents" || published[0].Key != "https://github.com/codemowers/agent/issues/1" {
		t.Fatalf("published=%+v", published)
	}

	for name, request := range map[string]*http.Request{
		"wrong secret": githubRequest(t, "other-secret", body),
		"outside meta": func() *http.Request {
			r := githubRequest(t, "hook-secret", body)
			r.Header.Set("X-Forwarded-For", "198.51.100.1")
			return r
		}(),
		"untrusted proxy": func() *http.Request {
			r := githubRequest(t, "hook-secret", body)
			r.RemoteAddr = "198.51.100.7:4000"
			return r
		}(),
	} {
		published = nil
		response := httptest.NewRecorder()
		p.receiverHandler(response, request)
		if response.Code < 400 || len(published) != 0 {
			t.Errorf("%s: got %d with %d events", name, response.Code, len(published))
		}
	}
}

func TestNamespacedReceiverAuthenticatesServiceAccountTokens(t *testing.T) {
	receiver := exampleReceiver(t, "receiver-alertmanager.yaml", "WebhookReceiver")
	binding := uidBinding(t, "binding-alertmanager.yaml", "agents", "b1")
	foreign := uidBinding(t, "binding-alertmanager.yaml", "elsewhere", "b2")
	protocol := exampleProtocol(t, "protocol-alertmanager.yaml")
	protocol.Spec.Event.AllowedKeyPrefixes = []string{"http://am.example.test"}
	var published []normalizedEvent
	p := &publisher{resolver: fakeResolver{
		protocols: map[string]receiverProtocol{"alertmanager": protocol},
		receivers: map[string]webhookReceiver{"agents/alertmanager": receiver},
		bindings:  []receiverBinding{binding, foreign},
		tokens:    map[string]string{"am": "system:serviceaccount:monitoring:alertmanager", "other": "system:serviceaccount:monitoring:prometheus"},
	}, publish: func(event normalizedEvent, _ string, _ []byte) error {
		published = append(published, event)
		return nil
	}}
	for _, tc := range []struct {
		name, token string
		want        int
	}{{"listed", "am", http.StatusAccepted}, {"missing", "", http.StatusUnauthorized}, {"other sender", "other", http.StatusUnauthorized}} {
		published = nil
		request := httptest.NewRequest(http.MethodPost, "/hooks/agents/alertmanager",
			strings.NewReader(`{"status":"firing","externalURL":"http://am.example.test","alerts":[{}],"commonLabels":{"alertname":"X"},"commonAnnotations":{}}`))
		request.SetPathValue("namespace", "agents")
		request.SetPathValue("receiver", "alertmanager")
		if tc.token != "" {
			request.Header.Set("Authorization", "Bearer "+tc.token)
		}
		response := httptest.NewRecorder()
		p.receiverHandler(response, request)
		if response.Code != tc.want {
			t.Fatalf("%s: got %d: %s", tc.name, response.Code, response.Body.String())
		}
		// A WebhookReceiver serves only bindings of its own namespace.
		if tc.want == http.StatusAccepted && (len(published) != 1 || published[0].Namespace != "agents") {
			t.Fatalf("%s: published=%+v", tc.name, published)
		}
	}
	missing := httptest.NewRequest(http.MethodPost, "/hooks/agents/nope", strings.NewReader("{}"))
	missing.SetPathValue("namespace", "agents")
	missing.SetPathValue("receiver", "nope")
	response := httptest.NewRecorder()
	p.receiverHandler(response, missing)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown receiver got %d", response.Code)
	}
}

func TestGitHubMetaCacheRefreshesAndServesStale(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	calls, fail := 0, false
	cache := newGitHubMetaCache(nil)
	cache.now = func() time.Time { return now }
	cache.fetch = func(_ context.Context, _, etag string) ([]string, string, bool, error) {
		calls++
		if fail {
			return nil, "", false, context.DeadlineExceeded
		}
		if etag == `"v1"` {
			return nil, etag, true, nil
		}
		return []string{"192.30.252.0/22", "2a0a:a440::/29"}, `"v1"`, false, nil
	}
	hooks, err := cache.Hooks(context.Background(), "https://api.github.com/meta")
	if err != nil || !slices.ContainsFunc(hooks, func(p netip.Prefix) bool { return p.Contains(netip.MustParseAddr("192.30.252.10")) }) {
		t.Fatalf("hooks=%v err=%v", hooks, err)
	}
	now = now.Add(2 * time.Hour)
	if hooks, err = cache.Hooks(context.Background(), "https://api.github.com/meta"); err != nil || len(hooks) != 2 || calls != 2 {
		t.Fatalf("not-modified refresh: hooks=%v err=%v calls=%d", hooks, err, calls)
	}
	fail = true
	now = now.Add(2 * time.Hour)
	if hooks, err = cache.Hooks(context.Background(), "https://api.github.com/meta"); err != nil || len(hooks) != 2 {
		t.Fatalf("stale list not served: %v %v", hooks, err)
	}
	now = now.Add(48 * time.Hour)
	if _, err = cache.Hooks(context.Background(), "https://api.github.com/meta"); err == nil {
		t.Fatal("list older than a day served")
	}
}
