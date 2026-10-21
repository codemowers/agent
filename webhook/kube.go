package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// errNotFound means the API server answered that the object does not exist,
// as opposed to being unavailable.
var errNotFound = errors.New("not found")

const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

var kubernetesName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
var secretKeyName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

func validKubernetesName(name string) bool {
	return len(name) > 0 && len(name) <= 253 && kubernetesName.MatchString(name)
}
func validSecretKey(name string) bool {
	return len(name) > 0 && len(name) <= 253 && secretKeyName.MatchString(name)
}

type receiverResolver interface {
	Protocol(context.Context, string) (receiverProtocol, error)
	// Receiver reads a WebhookReceiver, or a ClusterWebhookReceiver when the
	// namespace is empty.
	Receiver(ctx context.Context, namespace, name string) (webhookReceiver, error)
	Bindings(context.Context, webhookReceiver) ([]receiverBinding, error)
	ReceiverKey(context.Context, webhookReceiver) ([]byte, error)
	// ReviewToken returns the authenticated username of a token issued for
	// audience, or "" when the API server rejects it.
	ReviewToken(ctx context.Context, token, audience string) (string, error)
}
type kubeHookResolver struct {
	client          *http.Client
	base, tokenFile string
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
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("Kubernetes API address unavailable")
	}
	return &kubeHookResolver{client: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}, base: "https://" + net.JoinHostPort(host, port), tokenFile: serviceAccountDir + "/token"}, nil
}
func (k *kubeHookResolver) get(ctx context.Context, path string, out any) error {
	return k.do(ctx, http.MethodGet, path, nil, http.StatusOK, out)
}
func (k *kubeHookResolver) do(ctx context.Context, method, path string, body any, want int, out any) error {
	token, err := os.ReadFile(k.tokenFile)
	if err != nil {
		return fmt.Errorf("read ServiceAccount token: %w", err)
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	if resp.StatusCode != want {
		return fmt.Errorf("Kubernetes %s %s returned %d", method, path, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}

// ReviewToken checks a sender's token with the API server. The audience
// keeps tokens issued for other services from being accepted here.
func (k *kubeHookResolver) ReviewToken(ctx context.Context, token, audience string) (string, error) {
	review := map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "TokenReview",
		"spec": map[string]any{"token": token, "audiences": []string{audience}}}
	var result struct {
		Status struct {
			Authenticated bool     `json:"authenticated"`
			Audiences     []string `json:"audiences"`
			User          struct {
				Username string `json:"username"`
			} `json:"user"`
		} `json:"status"`
	}
	if err := k.do(ctx, http.MethodPost, "/apis/authentication.k8s.io/v1/tokenreviews", review, http.StatusCreated, &result); err != nil {
		return "", err
	}
	if !result.Status.Authenticated || !slices.Contains(result.Status.Audiences, audience) {
		return "", nil
	}
	return result.Status.User.Username, nil
}
