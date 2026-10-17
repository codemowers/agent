package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

var kubernetesName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

type agentHook struct {
	Namespace    string
	Agent        string
	HookID       string
	ServerURL    string
	Secret       []byte
	GitProviders []struct {
		Type      string `json:"type"`
		ServerURL string `json:"serverURL"`
	} `json:"gitProviders"`
}

func (hook agentHook) allowsRepository(provider string, body []byte) bool {
	var payload struct {
		Repository struct {
			FullName string `json:"full_name"`
			HTMLURL  string `json:"html_url"`
		} `json:"repository"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.Repository.FullName == "" {
		return false
	}
	repositoryHost := serverHost(payload.Repository.HTMLURL)
	if repositoryHost == "" || repositoryHost != serverHost(hook.ServerURL) {
		return false
	}
	for _, configured := range hook.GitProviders {
		if configured.Type == provider && repositoryHost == serverHost(configured.ServerURL) {
			return true
		}
	}
	return false
}

type hookResolver interface {
	Resolve(context.Context, string) ([]agentHook, error)
}

type hookAgentList struct {
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
	Items []struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			GitHubWebhooks []struct {
				ServerURL string `json:"serverURL"`
				HookID    string `json:"hookID"`
				SecretRef string `json:"secretRef"`
			} `json:"githubWebhooks"`
			GitProviders []struct {
				Type      string `json:"type"`
				ServerURL string `json:"serverURL"`
			} `json:"gitProviders"`
		} `json:"spec"`
	} `json:"items"`
}

type kubeHookResolver struct {
	client    *http.Client
	base      string
	tokenFile string
}

func newKubeHookResolver() (*kubeHookResolver, error) {
	ca, err := os.ReadFile(serviceAccountDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("invalid Kubernetes service CA")
	}
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("Kubernetes service address is unavailable")
	}
	return &kubeHookResolver{
		client:    &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}},
		base:      "https://" + net.JoinHostPort(host, port),
		tokenFile: serviceAccountDir + "/token",
	}, nil
}

func validKubernetesName(name string) bool {
	return len(name) > 0 && len(name) <= 253 && kubernetesName.MatchString(name)
}

func (resolver *kubeHookResolver) get(ctx context.Context, path string, out any) error {
	token, err := os.ReadFile(resolver.tokenFile)
	if err != nil {
		return fmt.Errorf("read service account token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, resolver.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	resp, err := resolver.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Kubernetes GET %s returned %d", path, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}

func (resolver *kubeHookResolver) Resolve(ctx context.Context, hookID string) ([]agentHook, error) {
	if !validHookID(hookID) {
		return nil, fmt.Errorf("invalid GitHub hook ID")
	}
	var agents hookAgentList
	path := "/apis/agents.codemowers.cloud/v1alpha1/agents?limit=50"
	for {
		var page hookAgentList
		if err := resolver.get(ctx, path, &page); err != nil {
			return nil, err
		}
		agents.Items = append(agents.Items, page.Items...)
		if page.Metadata.Continue == "" {
			break
		}
		path = "/apis/agents.codemowers.cloud/v1alpha1/agents?limit=50&continue=" + url.QueryEscape(page.Metadata.Continue)
	}
	var hooks []agentHook
	var unavailable error
	for _, agent := range agents.Items {
		for _, subscription := range agent.Spec.GitHubWebhooks {
			if subscription.HookID != hookID {
				continue
			}
			if !validKubernetesName(agent.Metadata.Namespace) || !validKubernetesName(agent.Metadata.Name) || !validKubernetesName(subscription.SecretRef) || serverHost(subscription.ServerURL) == "" {
				continue
			}
			var secret struct {
				Data map[string][]byte `json:"data"`
			}
			path := "/api/v1/namespaces/" + agent.Metadata.Namespace + "/secrets/" + subscription.SecretRef
			if err := resolver.get(ctx, path, &secret); err != nil {
				// One namespace's missing Secret must not block other subscribers.
				unavailable = err
				slog.Warn("webhook subscriber Secret unavailable", "namespace", agent.Metadata.Namespace, "agent", agent.Metadata.Name, "err", err)
				continue
			}
			if len(secret.Data["token"]) == 0 {
				unavailable = fmt.Errorf("webhook subscriber Secret has no token")
				slog.Warn("webhook subscriber Secret has no token", "namespace", agent.Metadata.Namespace, "agent", agent.Metadata.Name)
				continue
			}
			hooks = append(hooks, agentHook{Namespace: agent.Metadata.Namespace, Agent: agent.Metadata.Name, HookID: hookID,
				ServerURL: subscription.ServerURL, Secret: secret.Data["token"], GitProviders: agent.Spec.GitProviders})
		}
	}
	if len(hooks) == 0 && unavailable != nil {
		return nil, unavailable
	}
	return hooks, nil
}

func validHookID(id string) bool {
	if id == "" || len(id) > 20 {
		return false
	}
	for _, char := range id {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func serverHost(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}
