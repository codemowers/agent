package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// createTaskForAssignedIssue verifies current forge state before accepting a
// signed webhook as the first event of a Task. A delivery alone cannot choose
// a repository, Agent credential, or assignee.
func (c *EventConsumer) createTaskForAssignedIssue(ctx context.Context, namespace, agentName string, ev parsedEvent, messageID string) (bool, error) {
	if (ev.source != "github" && ev.source != "gitea") || (ev.kind != "issue" && ev.kind != "pull_request") {
		return false, nil
	}
	if _, err := strconv.ParseUint(ev.id, 10, 64); err != nil {
		return false, nil
	}
	var agent agentsv1alpha1.Agent
	if err := c.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: agentName}, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get event Agent: %w", err)
	}
	eventHost := ev.serverURL
	if ev.source == "github" && eventHost == "" {
		eventHost = "github.com"
	}
	var provider *agentsv1alpha1.GitProvider
	for i := range agent.Spec.GitProviders {
		candidate := &agent.Spec.GitProviders[i]
		if candidate.Type == ev.source && serverHost(candidate.ServerURL) == eventHost {
			provider = candidate
			break
		}
	}
	if provider == nil || provider.CredentialSecretRef == "" {
		return false, nil
	}
	if c.APIReader == nil {
		return false, fmt.Errorf("secret API reader is unavailable")
	}
	var secret corev1.Secret
	if err := c.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: provider.CredentialSecretRef}, &secret); err != nil {
		return false, fmt.Errorf("get configured forge credential: %w", err)
	}
	token := string(secret.Data["token"])
	if token == "" {
		return false, fmt.Errorf("configured forge credential has no token")
	}
	apiURL, err := forgeAPIURL(*provider)
	if err != nil {
		return false, err
	}
	httpClient := &http.Client{}
	if c.HTTPClient != nil {
		*httpClient = *c.HTTPClient
	}
	httpClient.Timeout = 10 * time.Second
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	var account struct {
		Login    string `json:"login"`
		Username string `json:"username"`
	}
	if err := forgeGet(ctx, httpClient, apiURL+"/user", token, &account); err != nil {
		return false, fmt.Errorf("verify forge account: %w", err)
	}
	login := account.Login
	if login == "" {
		login = account.Username
	}
	if login == "" {
		return false, fmt.Errorf("configured forge account has no login")
	}
	resource := "issues"
	if ev.kind == "pull_request" {
		resource = "pulls"
	}
	itemURL := fmt.Sprintf("%s/repos/%s/%s/%s/%s", apiURL, url.PathEscape(ev.owner),
		url.PathEscape(ev.repo), resource, ev.id)
	var item struct {
		State     string `json:"state"`
		Title     string `json:"title"`
		HTMLURL   string `json:"html_url"`
		Assignees []struct {
			Login    string `json:"login"`
			Username string `json:"username"`
		} `json:"assignees"`
	}
	if err := forgeGet(ctx, httpClient, itemURL, token, &item); err != nil {
		if errors.Is(err, errForgeNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("verify forge item: %w", err)
	}
	if item.State != "open" {
		return false, nil
	}
	assigned := false
	for _, assignee := range item.Assignees {
		if strings.EqualFold(assignee.Login, login) || strings.EqualFold(assignee.Username, login) {
			assigned = true
			break
		}
	}
	if !assigned {
		return false, nil
	}
	threadURL := item.HTMLURL
	thread, err := url.Parse(threadURL)
	if err != nil || thread.Scheme != "https" || serverHost(threadURL) != serverHost(provider.ServerURL) {
		threadURL = strings.TrimRight(provider.ServerURL, "/") + "/" + url.PathEscape(ev.owner) + "/" + url.PathEscape(ev.repo) + "/" + resource + "/" + ev.id
	}
	key := fmt.Sprintf("%s/%s/%s/%s/%s/%s/%s", agent.UID, ev.source, eventHost, ev.owner, ev.repo, ev.kind, ev.id)
	sum := sha256.Sum256([]byte(key))
	name := "task-" + hex.EncodeToString(sum[:12])
	task := &agentsv1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			Labels:      map[string]string{"codemowers.cloud/agent": agent.Name},
			Annotations: map[string]string{"codemowers.cloud/initial-delivery-id": messageID},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: agentsv1alpha1.GroupVersion.String(), Kind: "Agent", Name: agent.Name,
				UID: agent.UID, Controller: boolPtr(true), BlockOwnerDeletion: boolPtr(true),
			}},
		},
		Spec: agentsv1alpha1.TaskSpec{
			Objective: fmt.Sprintf("%s: %s. Read the latest updates and decide what to do next.", item.Title, threadURL),
			Suspend:   agent.Spec.Suspended,
			ExternalReferences: []agentsv1alpha1.ExternalReference{{
				Source: ev.source, ServerURL: provider.ServerURL, RepositoryOwner: ev.owner,
				RepositoryName: ev.repo, Kind: ev.kind, ID: ev.id,
			}},
		},
	}
	if err := c.Client.Create(ctx, task); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return true, nil
		}
		return false, fmt.Errorf("create assigned Task: %w", err)
	}
	return true, nil
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
