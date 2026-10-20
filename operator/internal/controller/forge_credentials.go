package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// The unsuffixed token is safe to choose only when an Agent defines one
// provider of that type. Multi-provider Agents expose explicitly named tokens.
func forgeCredentialEnv(providers []agentsv1alpha1.GitProvider) []corev1.EnvVar {
	type entry struct {
		provider agentsv1alpha1.GitProvider
		base     string
		suffix   string
	}
	entries := make([]entry, 0, len(providers))
	counts := make(map[string]int)
	hostCounts := make(map[string]int)
	seen := make(map[string]bool)
	for _, provider := range providers {
		base := ""
		switch provider.Type {
		case "github":
			base = "GH_TOKEN"
		case "gitea":
			base = "GITEA_TOKEN"
		default:
			continue
		}
		identity := provider.Type + "\x00" + provider.ServerURL + "\x00" + provider.CredentialKeyRef
		if seen[identity] {
			continue
		}
		seen[identity] = true
		counts[base]++
		suffix := forgeHostSuffix(provider.ServerURL)
		entries = append(entries, entry{provider: provider, base: base, suffix: suffix})
		if suffix != "" {
			hostCounts[base+"_"+suffix]++
		}
	}
	result := make([]corev1.EnvVar, 0, len(entries))
	for _, item := range entries {
		if item.provider.CredentialKeyRef == "" || item.suffix == "" {
			continue
		}
		name := item.base
		if counts[item.base] > 1 {
			name += "_" + item.suffix
			if hostCounts[name] > 1 {
				sum := sha256.Sum256([]byte(item.provider.ServerURL + "\x00" + item.provider.CredentialKeyRef))
				name += "_" + strings.ToUpper(hex.EncodeToString(sum[:4]))
			}
		}
		result = append(result, corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: agentsv1alpha1.ProviderTokensSecretName},
				Key:                  item.provider.CredentialKeyRef,
			},
		}})
	}
	return result
}

func forgeHostSuffix(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	var suffix strings.Builder
	lastUnderscore := false
	for _, char := range strings.ToUpper(parsed.Hostname()) {
		if char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' {
			suffix.WriteRune(char)
			lastUnderscore = false
		} else if !lastUnderscore {
			suffix.WriteByte('_')
			lastUnderscore = true
		}
	}
	result := strings.Trim(suffix.String(), "_")
	if port := parsed.Port(); port != "" {
		result += "_" + port
	}
	return result
}

func managedForgeCredentialName(name string) bool {
	return name == "AGENT_FORGE_TOKEN" || name == "GH_TOKEN" || name == "GITEA_TOKEN" ||
		strings.HasPrefix(name, "GH_TOKEN_") || strings.HasPrefix(name, "GITEA_TOKEN_")
}
