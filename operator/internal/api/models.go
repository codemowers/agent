package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var modelNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,120}$`)
var githubUsernamePattern = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,37}[a-zA-Z0-9])?$`)
var numericUsernamePattern = regexp.MustCompile(`^[0-9]+$`)

func githubUsername(value string) string {
	if githubUsernamePattern.MatchString(value) && !numericUsernamePattern.MatchString(value) {
		return value
	}
	return ""
}

func agentGitHubUsername(agent *agentsv1alpha1.Agent) string {
	if value := githubUsername(agent.Annotations["codemowers.cloud/github-username"]); value != "" {
		return value
	}
	for _, provider := range agent.Spec.GitProviders {
		if provider.Type == "github" && strings.TrimRight(provider.ServerURL, "/") == "https://github.com" {
			return githubUsername(agent.Name)
		}
	}
	return ""
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	identity, ok := h.authenticatedCaller(w, r)
	if !ok {
		return
	}
	name := identity.name
	if name == "" {
		name = identity.username
	}
	if name == "" {
		name = identity.user
	}
	jsonOK(w, map[string]string{"name": name, "subject": identity.user, "githubUsername": githubUsername(identity.username)})
}

type modelChoice struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

func (h *Handler) listAgentModels(w http.ResponseWriter, r *http.Request) {
	identity, ok := h.authenticatedCaller(w, r)
	if !ok {
		return
	}
	var agent agentsv1alpha1.Agent
	if err := h.Client.Get(r.Context(), client.ObjectKey{Namespace: r.PathValue("namespace"), Name: r.PathValue("name")}, &agent); err != nil {
		jsonError(w, err, http.StatusNotFound)
		return
	}
	allowed, err := h.canAccessAgent(r, identity, &agent)
	if err != nil {
		jsonError(w, err, http.StatusServiceUnavailable)
		return
	}
	if !allowed {
		jsonError(w, fmt.Errorf("access to Agent denied"), http.StatusForbidden)
		return
	}
	result := struct {
		Backend string        `json:"backend"`
		Default string        `json:"default"`
		Models  []modelChoice `json:"models"`
		Warning string        `json:"warning,omitempty"`
	}{Backend: agent.Spec.Model.Backend, Default: agent.Spec.Model.Name, Models: []modelChoice{{Name: "", Label: "Agent default"}}}
	var secret corev1.Secret
	if agent.Spec.Model.CredentialKeyRef == "" {
		result.Warning = "Model discovery unavailable: no credential key configured"
		jsonOK(w, result)
		return
	}
	if h.APIReader == nil {
		result.Warning = "Model discovery unavailable: Secret reader unavailable"
		jsonOK(w, result)
		return
	}
	if err := h.APIReader.Get(r.Context(), client.ObjectKey{Namespace: agent.Namespace, Name: agentsv1alpha1.ProviderTokensSecretName}, &secret); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			result.Warning = "Model discovery unavailable: provider token Secret not found"
		case apierrors.IsForbidden(err):
			result.Warning = "Model discovery unavailable: provider token Secret access denied"
		default:
			result.Warning = "Model discovery unavailable: provider token Secret read failed"
		}
		jsonOK(w, result)
		return
	}
	if len(secret.Data[agent.Spec.Model.CredentialKeyRef]) == 0 {
		result.Warning = "Model discovery unavailable: configured credential key missing"
		jsonOK(w, result)
		return
	}
	models, err := discoverModels(r.Context(), agent.Spec.Model.Backend, string(secret.Data[agent.Spec.Model.CredentialKeyRef]), h.ModelHTTPClient)
	if err != nil {
		result.Warning = "Model discovery unavailable: " + err.Error()
	} else {
		result.Models = append(result.Models, models...)
	}
	jsonOK(w, result)
}

func discoverModels(ctx context.Context, backend, token string, httpClient *http.Client) ([]modelChoice, error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	endpoint := "https://api.anthropic.com/v1/models?limit=100"
	if backend == "codex" {
		endpoint = "https://api.openai.com/v1/models"
	} else if backend != "claude" {
		return nil, fmt.Errorf("unknown backend")
	}
	seen := make(map[string]bool)
	models := make([]modelChoice, 0)
	for page := 0; endpoint != "" && page < 10; page++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		if backend == "claude" {
			request.Header.Set("x-api-key", token)
			request.Header.Set("anthropic-version", "2023-06-01")
		} else {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := httpClient.Do(request)
		if err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return nil, fmt.Errorf("provider returned HTTP %d", response.StatusCode)
		}
		var body struct {
			Data []struct {
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body)
		response.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("invalid provider response")
		}
		for _, item := range body.Data {
			if !modelNamePattern.MatchString(item.ID) || seen[item.ID] || !codingModel(backend, item.ID) {
				continue
			}
			seen[item.ID] = true
			label := item.DisplayName
			if label == "" {
				label = item.ID
			}
			models = append(models, modelChoice{Name: item.ID, Label: label})
		}
		if backend != "claude" || !body.HasMore {
			endpoint = ""
			break
		}
		if body.LastID == "" {
			return nil, fmt.Errorf("invalid provider pagination")
		}
		endpoint = "https://api.anthropic.com/v1/models?limit=100&after_id=" + url.QueryEscape(body.LastID)
	}
	if endpoint != "" {
		return nil, fmt.Errorf("provider model list exceeded page limit")
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	return models, nil
}

func codingModel(backend, id string) bool {
	if backend == "claude" {
		return strings.HasPrefix(id, "claude-")
	}
	if !strings.HasPrefix(id, "gpt-") && !strings.HasPrefix(id, "codex-") && !strings.HasPrefix(id, "o1") && !strings.HasPrefix(id, "o3") && !strings.HasPrefix(id, "o4") {
		return false
	}
	for _, excluded := range []string{"audio", "image", "realtime", "transcribe", "search", "embedding", "tts"} {
		if strings.Contains(id, excluded) {
			return false
		}
	}
	return true
}
