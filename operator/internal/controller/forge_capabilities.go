package controller

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
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var forgeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)

type forgeProbe struct {
	provider agentsv1alpha1.GitProvider
	baseURL  string
	apiURL   string
	token    string
	client   *http.Client
}

type probeRepository struct {
	FullName string `json:"full_name"`
	Owner    struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"owner"`
	Permissions struct {
		Push bool `json:"push"`
	} `json:"permissions"`
	Archived bool `json:"archived"`
	Private  bool `json:"private"`
}

func forgeURLs(provider agentsv1alpha1.GitProvider) (string, string, error) {
	u, err := url.Parse(provider.ServerURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (provider.Type == "github" && u.Path != "" && u.Path != "/") {
		return "", "", fmt.Errorf("invalid forge server URL")
	}
	base := strings.TrimRight(u.String(), "/")
	if provider.Type == "github" {
		if u.Hostname() == "github.com" {
			return base, "https://api.github.com", nil
		}
		return base, base + "/api/v3", nil
	}
	if provider.Type == "gitea" {
		return base, base + "/api/v1", nil
	}
	return "", "", fmt.Errorf("unsupported forge type")
}

func (r *AgentReconciler) probeForges(ctx context.Context, agent *agentsv1alpha1.Agent) []agentsv1alpha1.ForgeCapability {
	result := make([]agentsv1alpha1.ForgeCapability, 0, len(agent.Spec.GitProviders))
	var secret corev1.Secret
	secretAvailable := r.APIReader != nil && r.APIReader.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: agentsv1alpha1.CredentialSecret(agent)}, &secret) == nil
	httpClient := r.ForgeHTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	for _, provider := range agent.Spec.GitProviders {
		capability := agentsv1alpha1.ForgeCapability{Type: provider.Type, ServerURL: provider.ServerURL, Status: "unavailable"}
		base, api, err := forgeURLs(provider)
		if err != nil {
			capability.Detail = err.Error()
			result = append(result, capability)
			continue
		}
		capability.ServerURL = base
		key := agentsv1alpha1.GitProviderTokenKey(provider)
		if !secretAvailable || len(secret.Data[key]) == 0 {
			capability.Detail = "Credential Secret unavailable"
			result = append(result, capability)
			continue
		}
		probe := forgeProbe{provider: provider, baseURL: base, apiURL: api, token: string(secret.Data[key]), client: httpClient}
		if err := probe.read(ctx, &capability); err != nil {
			capability.Detail = err.Error()
		} else {
			capability.Status = "available"
		}
		result = append(result, capability)
	}
	return result
}

func (p *forgeProbe) get(ctx context.Context, path string, target any) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.apiURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codemowers-agent (+https://github.com/codemowers/agent)")
	response, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("forge request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("forge returned HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(target); err != nil {
		return nil, fmt.Errorf("invalid forge response")
	}
	return response.Header, nil
}

func (p *forgeProbe) read(ctx context.Context, capability *agentsv1alpha1.ForgeCapability) error {
	var account struct {
		Login    string `json:"login"`
		Username string `json:"username"`
		ID       int64  `json:"id"`
	}
	headers, err := p.get(ctx, "/user", &account)
	if err != nil {
		return err
	}
	capability.Login = account.Login
	if capability.Login == "" {
		capability.Login = account.Username
	}
	if !forgeNamePattern.MatchString(capability.Login) {
		return fmt.Errorf("forge account login unavailable")
	}
	orgs := make(map[string]bool)
	for page := 1; page <= 10; page++ {
		var repositories []probeRepository
		query := fmt.Sprintf("/user/repos?per_page=100&page=%d&sort=pushed", page)
		if p.provider.Type == "github" {
			query += "&affiliation=owner,collaborator,organization_member"
			if _, err := p.get(ctx, query, &repositories); err != nil {
				return err
			}
		} else {
			if account.ID <= 0 {
				return fmt.Errorf("forge account ID unavailable")
			}
			query = fmt.Sprintf("/repos/search?uid=%d&private=true&exclusive=false&limit=100&page=%d&sort=updated&order=desc", account.ID, page)
			var search struct {
				Data []probeRepository `json:"data"`
				OK   bool              `json:"ok"`
			}
			if _, err := p.get(ctx, query, &search); err != nil {
				return err
			}
			if !search.OK {
				return fmt.Errorf("forge repository search failed")
			}
			repositories = search.Data
		}
		for _, repository := range repositories {
			parts := strings.Split(repository.FullName, "/")
			if len(parts) != 2 || !forgeNamePattern.MatchString(parts[0]) || !forgeNamePattern.MatchString(parts[1]) {
				continue
			}
			capability.RepositoryCount++
			if len(capability.Repositories) < 10 {
				capability.Repositories = append(capability.Repositories, agentsv1alpha1.ForgeRepository{
					Name: repository.FullName, URL: p.baseURL + "/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]),
					Writable: repository.Permissions.Push, Archived: repository.Archived, Private: repository.Private,
				})
			}
			if strings.EqualFold(repository.Owner.Type, "Organization") && forgeNamePattern.MatchString(repository.Owner.Login) {
				orgs[repository.Owner.Login] = true
			}
		}
		if len(repositories) < 100 {
			break
		}
		if page == 10 {
			capability.Clipped = true
		}
	}
	if p.provider.Type == "github" {
		var organizations []struct {
			Login string `json:"login"`
		}
		if _, err := p.get(ctx, "/user/orgs?per_page=100", &organizations); err == nil {
			for _, org := range organizations {
				if forgeNamePattern.MatchString(org.Login) {
					orgs[org.Login] = true
				}
			}
		}
	} else {
		var organizations []struct {
			Name     string `json:"name"`
			Username string `json:"username"`
		}
		if _, err := p.get(ctx, "/user/orgs?limit=100", &organizations); err == nil {
			for _, org := range organizations {
				name := org.Username
				if name == "" {
					name = org.Name
				}
				if forgeNamePattern.MatchString(name) {
					orgs[name] = true
				}
			}
		}
	}
	userDestination := agentsv1alpha1.ForgeDestination{Owner: capability.Login, Kind: "user", Policy: "allowed", TokenPermission: "unknown"}
	if p.provider.Type == "github" {
		userDestination.URL = p.baseURL + "/new?owner=" + url.QueryEscape(capability.Login)
		for _, scope := range strings.Split(headers.Get("X-OAuth-Scopes"), ",") {
			if scope = strings.TrimSpace(scope); scope == "repo" || scope == "public_repo" {
				userDestination.TokenPermission = "likely"
			}
		}
	} else {
		userDestination.URL = p.baseURL + "/repo/create"
	}
	capability.Destinations = append(capability.Destinations, userDestination)
	owners := make([]string, 0, len(orgs))
	for owner := range orgs {
		owners = append(owners, owner)
	}
	sort.Strings(owners)
	if len(owners) > 20 {
		owners = owners[:20]
		capability.Clipped = true
	}
	for _, owner := range owners {
		destination := agentsv1alpha1.ForgeDestination{Owner: owner, Kind: "organization", Policy: "unknown", TokenPermission: "unknown"}
		if p.provider.Type == "github" {
			destination.URL = p.baseURL + "/new?owner=" + url.QueryEscape(owner)
			var membership struct {
				Role  string `json:"role"`
				State string `json:"state"`
			}
			_, membershipErr := p.get(ctx, "/user/memberships/orgs/"+url.PathEscape(owner), &membership)
			var organization struct {
				CanCreate        *bool `json:"members_can_create_repositories"`
				CanCreatePrivate *bool `json:"members_can_create_private_repositories"`
				CanCreatePublic  *bool `json:"members_can_create_public_repositories"`
			}
			_, orgErr := p.get(ctx, "/orgs/"+url.PathEscape(owner), &organization)
			if membershipErr == nil && membership.State == "active" {
				destination.Role = membership.Role
				if membership.Role == "admin" || orgErr == nil && (isTrue(organization.CanCreate) || isTrue(organization.CanCreatePrivate) || isTrue(organization.CanCreatePublic)) {
					destination.Policy = "allowed"
				}
				if membership.Role != "admin" && orgErr == nil && isFalse(organization.CanCreate) && !isTrue(organization.CanCreatePrivate) && !isTrue(organization.CanCreatePublic) {
					destination.Policy = "restricted"
				}
			}
		} else {
			destination.URL = p.baseURL + "/repo/create"
			var permission struct {
				CanCreateRepository bool `json:"can_create_repository"`
				IsOwner             bool `json:"is_owner"`
			}
			if _, err := p.get(ctx, "/users/"+url.PathEscape(capability.Login)+"/orgs/"+url.PathEscape(owner)+"/permissions", &permission); err == nil {
				if permission.CanCreateRepository || permission.IsOwner {
					destination.Policy = "allowed"
				} else {
					destination.Policy = "restricted"
				}
			}
		}
		capability.Destinations = append(capability.Destinations, destination)
	}
	return nil
}

func isTrue(value *bool) bool  { return value != nil && *value }
func isFalse(value *bool) bool { return value != nil && !*value }
