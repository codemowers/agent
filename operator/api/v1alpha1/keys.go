package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"

	"k8s.io/apimachinery/pkg/types"
)

// MaxKeyLength bounds a canonical routing key.
const MaxKeyLength = 1024

// CanonicalKey normalizes a resource URI used as a routing key. HTTP(S) keys
// keep their path but lose a trailing slash; scheme and host are lowercased.
// Opaque URIs such as mid:root@example.test are kept as given.
func CanonicalKey(raw string) (string, error) {
	if raw == "" || len(raw) > MaxKeyLength || strings.ContainsFunc(raw, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return "", errors.New("invalid key")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery {
		return "", errors.New("invalid key URI")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Opaque != "" {
		return parsed.Scheme + ":" + parsed.Opaque, nil
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" || parsed.Hostname() == "" {
		return "", errors.New("key URI needs an HTTP(S) host or an opaque part")
	}
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	if strings.Contains(parsed.Path, "//") || strings.Contains(parsed.Path+"/", "/../") || strings.Contains(parsed.Path+"/", "/./") {
		return "", errors.New("key URI path is not canonical")
	}
	return parsed.String(), nil
}

// KeyHasPrefix matches a canonical key against a prefix at a boundary, so
// https://github.com/team matches https://github.com/team/repo but not
// https://github.com/team-other.
func KeyHasPrefix(key, prefix string) bool {
	if prefix == "" || !strings.HasPrefix(key, prefix) {
		return false
	}
	if len(key) == len(prefix) || strings.HasSuffix(prefix, "/") || strings.HasSuffix(prefix, ":") {
		return true
	}
	return key[len(prefix)] == '/'
}

// TaskNameForKey derives the deterministic name of a Task created for a
// canonical key, so concurrent creators of the same key collide.
func TaskNameForKey(agentUID types.UID, key string) string {
	sum := sha256.Sum256([]byte(string(agentUID) + "\x00" + key))
	return "task-" + hex.EncodeToString(sum[:12])
}
