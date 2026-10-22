package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
)

func TestPollSeedsThenWakesBindingsOnce(t *testing.T) {
	items := `<item><title>Week 41</title><link>https://example.test/41</link><guid>w41</guid></item>`
	feedServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v2"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		etag := `"v1"`
		if strings.Contains(items, "w42") {
			etag = `"v2"`
		}
		w.Header().Set("ETag", etag)
		w.Write([]byte(`<rss version="2.0"><channel><title>LWKD &amp; more</title>` + items + `</channel></rss>`))
	}))
	defer feedServer.Close()
	var wakes []map[string]string
	wakeStatus := http.StatusServiceUnavailable
	wakeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		body["path"], body["key"], body["auth"] = r.URL.Path, r.Header.Get("Idempotency-Key"), r.Header.Get("Authorization")
		wakes = append(wakes, body)
		w.WriteHeader(wakeStatus)
	}))
	defer wakeServer.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("wake-token\n"), 0o600)

	scheme := runtime.NewScheme()
	agentsv1alpha1.AddToScheme(scheme)
	feed := &agentsv1alpha1.Feed{ObjectMeta: metav1.ObjectMeta{Name: "lwkd", Namespace: "agents"}, Spec: agentsv1alpha1.FeedSpec{URL: feedServer.URL}}
	binding := &agentsv1alpha1.FeedBinding{ObjectMeta: metav1.ObjectMeta{Name: "watch", Namespace: "agents"},
		Spec: agentsv1alpha1.FeedBindingSpec{FeedRef: "lwkd", TaskRef: "platform-watch"}}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agentsv1alpha1.Feed{}).WithObjects(feed, binding).Build()
	r := &FeedReconciler{Client: k8s, HTTP: feedServer.Client(), WakeURL: wakeServer.URL, TokenFile: tokenFile}
	ctx := context.Background()
	poll := func() *agentsv1alpha1.Feed {
		t.Helper()
		var current agentsv1alpha1.Feed
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(feed), &current); err != nil {
			t.Fatal(err)
		}
		current.Status.LastPolledAt = nil // poll now, whatever the interval
		if err := k8s.Status().Update(ctx, &current); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Reconcile(ctx, reconcileRequest(feed)); err != nil {
			t.Fatal(err)
		}
		k8s.Get(ctx, client.ObjectKeyFromObject(feed), &current)
		return &current
	}

	if seeded := poll(); len(wakes) != 0 || len(seeded.Status.Items) != 1 || seeded.Status.ETag != `"v1"` {
		t.Fatalf("first poll should only seed: wakes=%v status=%+v", wakes, seeded.Status)
	}
	items = `<item><title>Week &lt;b&gt;42&lt;/b&gt;</title><link>https://example.test/42</link><guid>w42</guid></item>` + items
	if failed := poll(); len(wakes) != 1 || len(failed.Status.Items) != 1 || failed.Status.Conditions[0].Status != metav1.ConditionFalse {
		t.Fatalf("failed wake must keep the cursor: wakes=%v status=%+v", wakes, failed.Status)
	}
	wakeStatus = http.StatusAccepted
	if woken := poll(); len(wakes) != 2 || len(woken.Status.Items) != 2 || woken.Status.ETag != `"v2"` {
		t.Fatalf("retry should wake and advance: wakes=%v status=%+v", wakes, woken.Status)
	}
	if text := wakes[1]["text"]; !strings.HasPrefix(text, "LWKD & more: 1 new item, first seen ") || !strings.HasSuffix(text, "\nWeek 42 — https://example.test/42") {
		t.Fatalf("wake text = %q", text)
	}
	want := map[string]string{"url": "https://example.test/42",
		"path": "/internal/v1/namespaces/agents/tasks/platform-watch/wake", "auth": "Bearer wake-token"}
	for k, v := range want {
		if wakes[1][k] != v {
			t.Fatalf("wake %s = %q, want %q", k, wakes[1][k], v)
		}
	}
	if wakes[0]["key"] != wakes[1]["key"] || !strings.HasPrefix(wakes[1]["key"], "feed-") {
		t.Fatalf("retried wake must reuse its key: %q %q", wakes[0]["key"], wakes[1]["key"])
	}
	if unchanged := poll(); len(wakes) != 2 || unchanged.Status.Conditions[0].Status != metav1.ConditionTrue {
		t.Fatalf("304 must not wake: wakes=%v", wakes)
	}
}

func TestPublicIPRejectsClusterAddresses(t *testing.T) {
	for address, public := range map[string]bool{"140.82.112.3": true, "2606:4700::1111": true, "10.0.0.1": false,
		"100.64.1.1": false, "127.0.0.1": false, "169.254.169.254": false, "fd10::1": false, "::1": false} {
		if publicIP(net.ParseIP(address)) != public {
			t.Fatalf("%s public = %v", address, !public)
		}
	}
}

func reconcileRequest(feed *agentsv1alpha1.Feed) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKeyFromObject(feed)}
}

func TestFeedsMCPListsOwnNamespaceItemsSince(t *testing.T) {
	scheme := runtime.NewScheme()
	agentsv1alpha1.AddToScheme(scheme)
	older, newer := metav1.NewTime(time.Unix(1000, 0)), metav1.NewTime(time.Unix(2000, 0))
	feeds := []client.Object{
		&agentsv1alpha1.Feed{ObjectMeta: metav1.ObjectMeta{Name: "lwkd", Namespace: "agents"}, Status: agentsv1alpha1.FeedStatus{Items: []agentsv1alpha1.FeedItem{
			{ID: "b", Title: "Week 42", Link: "https://example.test/42", FirstSeen: newer},
			{ID: "a", Title: "Week 41", Link: "https://example.test/41", FirstSeen: older}}}},
		&agentsv1alpha1.Feed{ObjectMeta: metav1.ObjectMeta{Name: "private", Namespace: "other"}, Status: agentsv1alpha1.FeedStatus{Items: []agentsv1alpha1.FeedItem{
			{ID: "x", Title: "Other namespace", FirstSeen: newer}}}},
	}
	kube := kubefake.NewSimpleClientset()
	kube.PrependReactor("create", "tokenreviews", func(action ktesting.Action) (bool, runtime.Object, error) {
		review := action.(ktesting.CreateAction).GetObject().(*authnv1.TokenReview)
		review.Status.Authenticated = review.Spec.Token == "worker" && review.Spec.Audiences[0] == "agent-feeds.harness"
		review.Status.User.Username = "system:serviceaccount:agents:agent-worker-krtek"
		return true, review, nil
	})
	server := &feedsMCP{audience: "agent-feeds.harness", reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(feeds...).Build(), reviews: kube.AuthenticationV1()}
	call := func(token, arguments string) (int, string) {
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_feed_items","arguments":`+arguments+`}}`))
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response.Code, response.Body.String()
	}
	if code, _ := call("other", `{}`); code != http.StatusUnauthorized {
		t.Fatalf("token for another audience accepted: %d", code)
	}
	_, all := call("worker", `{}`)
	if !strings.Contains(all, "Week 42") || !strings.Contains(all, "Week 41") || strings.Contains(all, "Other namespace") {
		t.Fatalf("items not scoped to the worker's namespace: %s", all)
	}
	_, recent := call("worker", `{"since":"1970-01-01T00:20:00Z"}`)
	if !strings.Contains(recent, "Week 42") || strings.Contains(recent, "Week 41") {
		t.Fatalf("since did not filter by firstSeen: %s", recent)
	}
}
