package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	tokenReviewTTL         = 5 * time.Minute
	tokenReviewRejectedTTL = time.Minute
	tokenReviewCacheSize   = 1024
)

type tokenReviewEntry struct {
	username string
	expires  time.Time
}

// tokenReviewCache keeps TokenReview results so that a brief API server
// outage does not reject senders whose tokens were already accepted. Entries
// are keyed by the token's hash, never the token, and expire with it.
type tokenReviewCache struct {
	review  func(ctx context.Context, token, audience string) (string, error)
	now     func() time.Time
	mu      sync.Mutex
	entries map[[sha256.Size]byte]tokenReviewEntry
}

func newTokenReviewCache(review func(context.Context, string, string) (string, error)) *tokenReviewCache {
	return &tokenReviewCache{review: review, now: time.Now, entries: map[[sha256.Size]byte]tokenReviewEntry{}}
}

// tokenClaims reads the unverified audience and expiry of a JWT. They only
// screen out tokens the API server would reject and bound the cache entry;
// the TokenReview remains the authentication.
func tokenClaims(token string) (audiences []string, expires time.Time, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, time.Time{}, false
	}
	var claims struct {
		Aud any   `json:"aud"`
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return nil, time.Time{}, false
	}
	switch aud := claims.Aud.(type) {
	case string:
		audiences = []string{aud}
	case []any:
		for _, a := range aud {
			if s, ok := a.(string); ok {
				audiences = append(audiences, s)
			}
		}
	}
	return audiences, time.Unix(claims.Exp, 0), true
}

func (c *tokenReviewCache) ReviewToken(ctx context.Context, token, audience string) (string, error) {
	now := c.now()
	audiences, expires, ok := tokenClaims(token)
	if !ok || !slices.Contains(audiences, audience) || !now.Before(expires) {
		return "", nil
	}
	key := sha256.Sum256([]byte(audience + "\x00" + token))
	c.mu.Lock()
	entry, hit := c.entries[key]
	c.mu.Unlock()
	if hit && now.Before(entry.expires) {
		return entry.username, nil
	}
	username, err := c.review(ctx, token, audience)
	if err != nil {
		return "", err
	}
	ttl := tokenReviewTTL
	if username == "" {
		ttl = tokenReviewRejectedTTL
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= tokenReviewCacheSize {
		for k, e := range c.entries {
			if !now.Before(e.expires) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= tokenReviewCacheSize {
			clear(c.entries)
		}
	}
	if until := now.Add(ttl); until.Before(expires) {
		expires = until
	}
	c.entries[key] = tokenReviewEntry{username: username, expires: expires}
	return username, nil
}
