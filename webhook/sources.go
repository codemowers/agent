package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"cel.dev/cel-go/cel"
)

const (
	githubMetaRefresh = time.Hour
	githubMetaStale   = 24 * time.Hour
)

// clientAddr is the request's client address. The rightmost X-Forwarded-For
// entry is the address the trusted proxy (Traefik) saw, so it is used only
// when the TCP peer is one; any other peer's header is ignored.
func clientAddr(r *http.Request, trusted []netip.Prefix) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	peer = peer.Unmap()
	if !containsAddr(trusted, peer) {
		return peer, true
	}
	forwarded := r.Header.Values("X-Forwarded-For")
	if len(forwarded) == 0 {
		return peer, true
	}
	entries := strings.Split(forwarded[len(forwarded)-1], ",")
	client, err := netip.ParseAddr(strings.TrimSpace(entries[len(entries)-1]))
	if err != nil {
		return netip.Addr{}, false
	}
	return client.Unmap(), true
}

func containsAddr(prefixes []netip.Prefix, addr netip.Addr) bool {
	return slices.ContainsFunc(prefixes, func(p netip.Prefix) bool { return p.Contains(addr) })
}

func parsePrefixes(values []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			addr, addrErr := netip.ParseAddr(value)
			if addrErr != nil {
				return nil, fmt.Errorf("invalid CIDR %q", value)
			}
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		out = append(out, prefix.Masked())
	}
	return out, nil
}

type githubMetaEntry struct {
	hooks   []netip.Prefix
	etag    string
	fetched time.Time
}

// githubMetaCache keeps the hooks address ranges of GitHub meta APIs. Only
// the hooks ranges count: GitHub Actions runners use other ranges, so anyone's
// workflow cannot pose as a webhook delivery.
type githubMetaCache struct {
	fetch   func(ctx context.Context, url, etag string) (hooks []string, newETag string, notModified bool, err error)
	now     func() time.Time
	mu      sync.Mutex
	entries map[string]githubMetaEntry
}

func newGitHubMetaCache(client *http.Client) *githubMetaCache {
	return &githubMetaCache{now: time.Now, entries: map[string]githubMetaEntry{},
		fetch: func(ctx context.Context, url, etag string) ([]string, string, bool, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return nil, "", false, err
			}
			req.Header.Set("Accept", "application/vnd.github+json")
			req.Header.Set("User-Agent", userAgent)
			if etag != "" {
				req.Header.Set("If-None-Match", etag)
			}
			resp, err := client.Do(req)
			if err != nil {
				return nil, "", false, err
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusNotModified {
				return nil, etag, true, nil
			}
			if resp.StatusCode != http.StatusOK {
				return nil, "", false, fmt.Errorf("GitHub meta returned %d", resp.StatusCode)
			}
			var meta struct {
				Hooks []string `json:"hooks"`
			}
			if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&meta); err != nil {
				return nil, "", false, err
			}
			return meta.Hooks, resp.Header.Get("ETag"), false, nil
		}}
}

// Hooks returns the hooks ranges, refreshed hourly. When GitHub is unreachable
// the last good list keeps serving for up to a day.
func (c *githubMetaCache) Hooks(ctx context.Context, url string) ([]netip.Prefix, error) {
	now := c.now()
	c.mu.Lock()
	entry, hit := c.entries[url]
	c.mu.Unlock()
	if hit && now.Sub(entry.fetched) < githubMetaRefresh {
		return entry.hooks, nil
	}
	hooks, etag, notModified, err := c.fetch(ctx, url, entry.etag)
	if err == nil && !notModified {
		var parsed []netip.Prefix
		if parsed, err = parsePrefixes(hooks); err == nil && len(parsed) == 0 {
			err = errors.New("GitHub meta lists no hooks ranges")
		}
		entry.hooks, entry.etag = parsed, etag
	}
	if err != nil {
		if hit && now.Sub(entry.fetched) < githubMetaStale {
			return entry.hooks, nil
		}
		return nil, err
	}
	entry.fetched = now
	c.mu.Lock()
	c.entries[url] = entry
	c.mu.Unlock()
	return entry.hooks, nil
}

// authenticateSender requires one passing credential (the secret signature,
// a listed ServiceAccount's token or the Ed25519 signature) and, when the
// receiver restricts addresses, an allowed client address. It returns the
// HTTP status to answer with on failure.
func (p *publisher) authenticateSender(r *http.Request, receiver webhookReceiver, protocol receiverProtocol, raw []byte, vars map[string]any, env *cel.Env) (int, error) {
	senders := receiver.Spec.Senders
	unavailable := false
	authenticated := false
	if senders.SecretKeyRef != nil && protocol.Spec.Signature != nil && protocol.Spec.Signature.Algorithm == "hmacSha256" {
		key, err := p.resolver.ReceiverKey(r.Context(), receiver)
		if err != nil {
			unavailable = true
		} else if authenticated, err = checkReceiverSignature(r, protocol, key, raw, vars, env); err != nil {
			return 503, errors.New("invalid signature configuration")
		}
	}
	if !authenticated && senders.Ed25519PublicKey != "" && protocol.Spec.Signature != nil && protocol.Spec.Signature.Algorithm == "ed25519" {
		key, err := hex.DecodeString(senders.Ed25519PublicKey)
		if err != nil {
			return 503, errors.New("invalid Ed25519 key")
		}
		var signatureErr error
		if authenticated, signatureErr = checkReceiverSignature(r, protocol, key, raw, vars, env); signatureErr != nil {
			return 503, errors.New("invalid signature configuration")
		}
	}
	if !authenticated && len(senders.ServiceAccounts) > 0 {
		if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && token != "" {
			username, err := p.resolver.ReviewToken(r.Context(), token, senders.Audience)
			if err != nil {
				unavailable = true
			}
			for _, sa := range senders.ServiceAccounts {
				namespace, name, _ := strings.Cut(sa, "/")
				if username != "" && username == "system:serviceaccount:"+namespace+":"+name {
					authenticated = true
				}
			}
		}
	}
	if !authenticated {
		// A dependency outage is retried by senders; a plain rejection is final.
		if unavailable {
			return 503, errors.New("sender verification unavailable")
		}
		return http.StatusUnauthorized, errors.New("sender not authenticated")
	}
	if senders.GitHubMeta == "" && len(senders.CIDRs) == 0 {
		return 0, nil
	}
	allowed, err := parsePrefixes(senders.CIDRs)
	if err != nil {
		return 503, errors.New("invalid receiver CIDRs")
	}
	if senders.GitHubMeta != "" {
		hooks, err := p.githubMeta.Hooks(r.Context(), senders.GitHubMeta)
		if err != nil {
			return 503, errors.New("GitHub meta unavailable")
		}
		allowed = append(allowed, hooks...)
	}
	if addr, ok := clientAddr(r, p.trustedProxies); !ok || !containsAddr(allowed, addr) {
		return http.StatusForbidden, errors.New("sender address not allowed")
	}
	return 0, nil
}
