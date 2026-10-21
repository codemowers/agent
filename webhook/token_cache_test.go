package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"
)

func testToken(audience string, expires time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"aud":[%q],"exp":%d}`, audience, expires.Unix())))
	return "e30." + payload + ".sig"
}

func TestTokenReviewCacheSurvivesAPIOutage(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	calls, apiDown := 0, false
	cache := newTokenReviewCache(func(context.Context, string, string) (string, error) {
		calls++
		if apiDown {
			return "", errors.New("API unavailable")
		}
		return "system:serviceaccount:monitoring:alertmanager", nil
	})
	cache.now = func() time.Time { return now }
	token := testToken("agent-webhook", now.Add(time.Hour))

	for range 2 {
		if user, err := cache.ReviewToken(context.Background(), token, "agent-webhook"); err != nil || user == "" {
			t.Fatalf("review: %q %v", user, err)
		}
	}
	if calls != 1 {
		t.Fatalf("got %d TokenReviews, want 1", calls)
	}
	apiDown = true
	now = now.Add(4 * time.Minute)
	if user, err := cache.ReviewToken(context.Background(), token, "agent-webhook"); err != nil || user == "" {
		t.Fatalf("cached review during outage: %q %v", user, err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := cache.ReviewToken(context.Background(), token, "agent-webhook"); err == nil {
		t.Fatal("expired cache entry must not authenticate during an outage")
	}
}

func TestTokenReviewCacheScreensTokensBeforeReview(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cache := newTokenReviewCache(func(context.Context, string, string) (string, error) {
		t.Fatal("screened token reached TokenReview")
		return "", nil
	})
	cache.now = func() time.Time { return now }
	for name, token := range map[string]string{
		"not a JWT":      "opaque",
		"other audience": testToken("agent-rabbitmq", now.Add(time.Hour)),
		"expired":        testToken("agent-webhook", now.Add(-time.Second)),
	} {
		if user, err := cache.ReviewToken(context.Background(), token, "agent-webhook"); user != "" || err != nil {
			t.Fatalf("%s: got %q %v", name, user, err)
		}
	}
}

func TestStaleCacheServesLastValueOnlyWhileUnavailable(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cache := newStaleCache[string](func() time.Time { return now })
	fetch := func(value string, err error) func() (string, error) {
		return func() (string, error) { return value, err }
	}
	if v, _ := cache.get("k", fetch("v1", nil)); v != "v1" {
		t.Fatal(v)
	}
	now = now.Add(time.Minute)
	if v, err := cache.get("k", fetch("", errors.New("timeout"))); v != "v1" || err != nil {
		t.Fatalf("outage: %q %v", v, err)
	}
	if _, err := cache.get("k", fetch("", errNotFound)); !errors.Is(err, errNotFound) {
		t.Fatal("deleted object must not be served from cache")
	}
	if _, err := cache.get("k", fetch("", errors.New("timeout"))); err == nil {
		t.Fatal("deleted object came back during an outage")
	}
}
