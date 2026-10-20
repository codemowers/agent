package api

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authclient "k8s.io/client-go/kubernetes/typed/authorization/v1"
)

type caller struct {
	user     string
	groups   []string
	name     string
	username string
}

type tokenVerifier interface {
	Verify(context.Context, string) (caller, error)
}
type accessReviewer interface {
	Allowed(context.Context, caller, string, string) (bool, error)
}

type subjectAccessReviewer struct {
	client authclient.AuthorizationV1Interface
}

func (s subjectAccessReviewer) Allowed(ctx context.Context, user caller, namespace, name string) (bool, error) {
	review, err := s.client.SubjectAccessReviews().Create(ctx, &authv1.SubjectAccessReview{
		Spec: authv1.SubjectAccessReviewSpec{User: user.user, Groups: user.groups,
			ResourceAttributes: &authv1.ResourceAttributes{Group: "codemowers.cloud", Resource: "agents", Verb: "get", Namespace: namespace, Name: name}},
	}, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	if review.Status.EvaluationError != "" {
		return false, errors.New(review.Status.EvaluationError)
	}
	return review.Status.Allowed && !review.Status.Denied, nil
}

type oidcVerifier struct {
	issuer, audience string
	client           *http.Client
	mu               sync.Mutex
	keys             map[string]*rsa.PublicKey
	expires          time.Time
}

func newOIDCVerifier(issuer, origin string) (*oidcVerifier, error) {
	issuerURL, err := url.Parse(issuer)
	if err != nil || issuerURL.Scheme != "https" || issuerURL.Host == "" || issuerURL.User != nil || issuerURL.RawQuery != "" || issuerURL.Fragment != "" {
		return nil, errors.New("invalid OIDC issuer")
	}
	originURL, err := url.Parse(origin)
	if err != nil || originURL.Scheme != "https" || originURL.Host == "" || (originURL.Path != "" && originURL.Path != "/") || originURL.User != nil || originURL.RawQuery != "" || originURL.Fragment != "" {
		return nil, errors.New("invalid OIDC origin")
	}
	return &oidcVerifier{issuer: issuer, audience: strings.TrimRight(origin, "/") + "/api", client: &http.Client{Timeout: 10 * time.Second}}, nil
}

func NewOIDCVerifier(issuer, origin string) (tokenVerifier, error) {
	return newOIDCVerifier(issuer, origin)
}
func NewSubjectAccessReviewer(client authclient.AuthorizationV1Interface) accessReviewer {
	return subjectAccessReviewer{client: client}
}

func (v *oidcVerifier) refresh(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if time.Now().Before(v.expires) {
		return nil
	}
	discoveryURL, err := url.JoinPath(v.issuer, ".well-known/openid-configuration")
	if err != nil {
		return err
	}
	var discovery struct {
		Issuer string `json:"issuer"`
		JWKS   string `json:"jwks_uri"`
	}
	if err := v.fetchJSON(ctx, discoveryURL, &discovery); err != nil {
		return err
	}
	if discovery.Issuer != v.issuer {
		return errors.New("OIDC discovery issuer mismatch")
	}
	u, err := url.Parse(discovery.JWKS)
	issuerURL, _ := url.Parse(v.issuer)
	if err != nil || u.Scheme != "https" || u.Host != issuerURL.Host || u.User != nil || u.Fragment != "" {
		return errors.New("invalid JWKS URL")
	}
	var jwks struct {
		Keys []struct{ Kty, Use, Kid, N, E string } `json:"keys"`
	}
	if err := v.fetchJSON(ctx, discovery.JWKS, &jwks); err != nil {
		return err
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, key := range jwks.Keys {
		if key.Kty != "RSA" || (key.Use != "" && key.Use != "sig") || key.Kid == "" {
			continue
		}
		n, nerr := base64.RawURLEncoding.DecodeString(key.N)
		e, eerr := base64.RawURLEncoding.DecodeString(key.E)
		if nerr != nil || eerr != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		exponent := 0
		for _, b := range e {
			exponent = exponent<<8 | int(b)
		}
		if exponent < 3 {
			continue
		}
		keys[key.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exponent}
	}
	if len(keys) == 0 {
		return errors.New("OIDC JWKS has no signing keys")
	}
	v.keys, v.expires = keys, time.Now().Add(5*time.Minute)
	return nil
}

func (v *oidcVerifier) fetchJSON(ctx context.Context, endpoint string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("OIDC endpoint returned %d", resp.StatusCode)
	}
	return json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(target)
}

func (v *oidcVerifier) Verify(ctx context.Context, raw string) (caller, error) {
	if err := v.refresh(ctx); err != nil {
		return caller{}, err
	}
	claims := jwt.MapClaims{}
	parsed, err := jwt.NewParser(jwt.WithValidMethods([]string{"RS256", "PS256"}), jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience), jwt.WithExpirationRequired(), jwt.WithIssuedAt()).ParseWithClaims(raw, claims,
		func(t *jwt.Token) (any, error) {
			if t.Header["typ"] != "at+jwt" {
				return nil, errors.New("access token type required")
			}
			kid, _ := t.Header["kid"].(string)
			v.mu.Lock()
			key := v.keys[kid]
			v.mu.Unlock()
			if key == nil {
				return nil, errors.New("unknown signing key")
			}
			return key, nil
		})
	if err != nil || !parsed.Valid {
		return caller{}, errors.New("invalid or expired access token")
	}
	sub, _ := claims["sub"].(string)
	if sub == "" || claims["iat"] == nil {
		return caller{}, errors.New("access token identity missing")
	}
	groups := []string{"system:authenticated"}
	if rawGroups, ok := claims["groups"].([]any); ok {
		for _, item := range rawGroups {
			if group, ok := item.(string); ok && group != "" {
				groups = append(groups, group)
			}
		}
	}
	name, _ := claims["name"].(string)
	username, _ := claims["preferred_username"].(string)
	if username == "" {
		username = sub
	}
	return caller{user: v.issuer + "#" + sub, groups: groups, name: name, username: username}, nil
}
