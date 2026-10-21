package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// createTaskByPolicy creates a Task for an unclaimed key when the most
// specific TaskPolicy of the Agent allows it. An event alone cannot choose a
// repository, Agent credential, or assignee.
func (c *EventConsumer) createTaskByPolicy(ctx context.Context, agent *agentsv1alpha1.Agent, ev intakeEvent) (bool, error) {
	var policies agentsv1alpha1.TaskPolicyList
	if err := c.APIReader.List(ctx, &policies, client.InNamespace(ev.Namespace)); err != nil {
		return false, err
	}
	var policy *agentsv1alpha1.TaskPolicy
	tie := false
	for i := range policies.Items {
		candidate := &policies.Items[i]
		if candidate.Spec.AgentRef != agent.Name || !agentsv1alpha1.KeyHasPrefix(ev.Key, candidate.Spec.KeyPrefix) ||
			len(candidate.Spec.EventTypes) > 0 && !slices.Contains(candidate.Spec.EventTypes, ev.Type) {
			continue
		}
		switch {
		case policy == nil || len(candidate.Spec.KeyPrefix) > len(policy.Spec.KeyPrefix):
			policy, tie = candidate, false
		case len(candidate.Spec.KeyPrefix) == len(policy.Spec.KeyPrefix):
			tie = true
		}
	}
	if policy == nil {
		return false, nil
	}
	logger := log.FromContext(ctx).WithValues("namespace", ev.Namespace, "taskPolicy", policy.Name)
	if tie {
		logger.Info("ambiguous TaskPolicy, not creating a Task")
		return false, nil
	}
	source := ev.URL
	if source == "" {
		source = ev.Key
	}
	task := &agentsv1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: agentsv1alpha1.TaskNameForKey(agent.UID, ev.Key), Namespace: ev.Namespace,
			Labels:      map[string]string{"codemowers.cloud/agent": agent.Name},
			Annotations: map[string]string{"codemowers.cloud/initial-delivery-id": ev.Receipt},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: agentsv1alpha1.GroupVersion.String(), Kind: "Agent", Name: agent.Name,
				UID: agent.UID, Controller: boolPtr(true), BlockOwnerDeletion: boolPtr(true),
			}},
		},
		Spec: agentsv1alpha1.TaskSpec{
			AgentRef:    agent.Name,
			Objective:   fmt.Sprintf("%s: %s. Read the latest updates and decide what to do next.", ev.Text, source),
			Suspend:     agent.Spec.Suspended,
			IdleSeconds: int32Ptr(agentsv1alpha1.AgentIdleSeconds(agent)),
			Resources:   agentsv1alpha1.AgentWorkerResources(agent),
			RoutingKeys: []string{ev.Key},
		},
	}
	switch policy.Spec.Verify {
	case "none":
		// Mail headers never create a Task by themselves.
		if ev.Source == "imap" {
			return false, nil
		}
	case "forgeAssignment":
		item, ok, err := c.verifyForgeAssignment(ctx, agent, ev.Key)
		if err != nil || !ok {
			return false, err
		}
		task.Spec.Objective = fmt.Sprintf("%s: %s. Read the latest updates and decide what to do next.", item.title, item.url)
		task.Spec.ExternalReferences = []agentsv1alpha1.ExternalReference{item.reference}
	default:
		logger.Info("unsupported TaskPolicy verify, not creating a Task", "verify", policy.Spec.Verify)
		return false, nil
	}
	if ev.Coalesce > 0 {
		task.Annotations["codemowers.cloud/coalesce-until"] = strconv.FormatInt(time.Now().Add(ev.Coalesce).UnixMilli(), 10)
		task.Annotations["codemowers.cloud/coalesce-trailing"] = "false"
	}
	if err := c.Client.Create(ctx, task); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return true, nil
		}
		return false, fmt.Errorf("create policy Task: %w", err)
	}
	return true, nil
}

type forgeAccount struct {
	Login    string `json:"login"`
	Username string `json:"username"`
}

type forgeItem struct {
	title     string
	url       string
	reference agentsv1alpha1.ExternalReference
}

// verifyForgeAssignment reads a key of the form
// https://<forge>/<owner>/<repo>/issues/<number> and checks with the Agent's
// own credential that the issue or pull request is open and assigned to it.
func (c *EventConsumer) verifyForgeAssignment(ctx context.Context, agent *agentsv1alpha1.Agent, key string) (forgeItem, bool, error) {
	parsed, err := url.Parse(key)
	if err != nil || parsed.Scheme != "https" {
		return forgeItem{}, false, nil
	}
	segments := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	if len(segments) != 4 || segments[0] == "" || segments[1] == "" || segments[2] != "issues" {
		return forgeItem{}, false, nil
	}
	if _, err := strconv.ParseUint(segments[3], 10, 64); err != nil {
		return forgeItem{}, false, nil
	}
	owner, repo, number := segments[0], segments[1], segments[3]
	var provider *agentsv1alpha1.GitProvider
	for i := range agent.Spec.GitProviders {
		candidate := &agent.Spec.GitProviders[i]
		server, err := url.Parse(candidate.ServerURL)
		if err == nil && strings.EqualFold(server.Host, parsed.Host) {
			provider = candidate
			break
		}
	}
	if provider == nil || provider.CredentialKeyRef == "" {
		return forgeItem{}, false, nil
	}
	if c.APIReader == nil {
		return forgeItem{}, false, fmt.Errorf("secret API reader is unavailable")
	}
	var secret corev1.Secret
	if err := c.APIReader.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: agentsv1alpha1.ProviderTokensSecretName}, &secret); err != nil {
		return forgeItem{}, false, fmt.Errorf("get configured forge credential: %w", err)
	}
	token := string(secret.Data[provider.CredentialKeyRef])
	if token == "" {
		return forgeItem{}, false, fmt.Errorf("configured forge credential has no token")
	}
	apiURL, err := forgeAPIURL(*provider)
	if err != nil {
		return forgeItem{}, false, err
	}
	httpClient := &http.Client{}
	if c.HTTPClient != nil {
		*httpClient = *c.HTTPClient
	}
	httpClient.Timeout = 10 * time.Second
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	login := probedForgeLogin(agent, *provider)
	if login == "" {
		var account forgeAccount
		if err := forgeGet(ctx, httpClient, apiURL+"/user", token, &account); err != nil {
			return forgeItem{}, false, fmt.Errorf("verify forge account: %w", err)
		}
		login = account.Login
		if login == "" {
			login = account.Username
		}
		if login == "" {
			return forgeItem{}, false, fmt.Errorf("configured forge account has no login")
		}
	}
	// GitHub and Gitea serve pull requests through the issues endpoint too.
	var item struct {
		State       string          `json:"state"`
		Title       string          `json:"title"`
		HTMLURL     string          `json:"html_url"`
		PullRequest json.RawMessage `json:"pull_request"`
		Assignees   []forgeAccount  `json:"assignees"`
	}
	itemURL := fmt.Sprintf("%s/repos/%s/%s/issues/%s", apiURL, url.PathEscape(owner), url.PathEscape(repo), number)
	if err := forgeGet(ctx, httpClient, itemURL, token, &item); err != nil {
		if errors.Is(err, errForgeNotFound) {
			return forgeItem{}, false, nil
		}
		return forgeItem{}, false, fmt.Errorf("verify forge item: %w", err)
	}
	if item.State != "open" || !slices.ContainsFunc(item.Assignees, func(a forgeAccount) bool {
		return strings.EqualFold(a.Login, login) || strings.EqualFold(a.Username, login)
	}) {
		return forgeItem{}, false, nil
	}
	kind := "issue"
	if len(item.PullRequest) > 0 && string(item.PullRequest) != "null" {
		kind = "pull_request"
	}
	itemHTML := item.HTMLURL
	if html, err := url.Parse(itemHTML); err != nil || html.Scheme != "https" || !strings.EqualFold(html.Host, parsed.Host) {
		itemHTML = key
	}
	return forgeItem{title: item.Title, url: itemHTML, reference: agentsv1alpha1.ExternalReference{
		Source: provider.Type, ServerURL: provider.ServerURL, RepositoryOwner: owner,
		RepositoryName: repo, Kind: kind, ID: number,
	}}, true, nil
}

// probedForgeLogin reuses the Agent's current forge snapshot to avoid an
// account lookup per event.
func probedForgeLogin(agent *agentsv1alpha1.Agent, provider agentsv1alpha1.GitProvider) string {
	if agent.Status.ForgesObservedGeneration != agent.Generation {
		return ""
	}
	for _, forge := range agent.Status.Forges {
		if forge.Status == "available" && forge.Type == provider.Type && forge.ServerURL == provider.ServerURL {
			return forge.Login
		}
	}
	return ""
}

func forgeAPIURL(provider agentsv1alpha1.GitProvider) (string, error) {
	server, err := url.Parse(provider.ServerURL)
	if err != nil || server.Scheme != "https" || server.Host == "" || server.User != nil || server.RawQuery != "" || server.Fragment != "" {
		return "", fmt.Errorf("invalid configured forge URL")
	}
	base := strings.TrimRight(server.String(), "/")
	if provider.Type == "gitea" {
		return base + "/api/v1", nil
	}
	if base == "https://github.com" {
		return "https://api.github.com", nil
	}
	return base + "/api/v3", nil
}

func forgeGet(ctx context.Context, httpClient *http.Client, endpoint, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codemowers-agent (+https://github.com/codemowers/agent)")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound {
			return errForgeNotFound
		}
		return fmt.Errorf("forge API returned %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

var errForgeNotFound = errors.New("forge item not found")
