package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

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
	Binding(context.Context, string, string) (receiverBinding, error)
	Bindings(context.Context, string) ([]receiverBinding, error)
	BindingKey(context.Context, receiverBinding) ([]byte, error)
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
	token, err := os.ReadFile(k.tokenFile)
	if err != nil {
		return fmt.Errorf("read ServiceAccount token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	resp, err := k.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Kubernetes GET %s returned %d", path, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}
