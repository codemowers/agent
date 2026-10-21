package main

import (
	"errors"
	"net/url"
	"strings"
)

// canonicalKey mirrors the operator's v1alpha1.CanonicalKey; the operator
// rejects any key that it would normalize differently.
func canonicalKey(raw string) (string, error) {
	if raw == "" || len(raw) > 1024 || strings.ContainsFunc(raw, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
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

// keyHasPrefix matches at a path boundary, like v1alpha1.KeyHasPrefix.
func keyHasPrefix(key, prefix string) bool {
	if prefix == "" || !strings.HasPrefix(key, prefix) {
		return false
	}
	if len(key) == len(prefix) || strings.HasSuffix(prefix, "/") || strings.HasSuffix(prefix, ":") {
		return true
	}
	return key[len(prefix)] == '/'
}
