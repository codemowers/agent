package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOIDCVerifierChecksAudienceTypeAndSignature(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	issuer := "https://issuer.example/"
	origin := "https://operator.example"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]string{"issuer": issuer, "jwks_uri": issuer + "jwks"})
		case "/jwks":
			e := big.NewInt(int64(key.PublicKey.E)).Bytes()
			json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{
				"kty": "RSA", "use": "sig", "kid": "key-1", "n": base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(e),
			}}})
		default:
			http.NotFound(w, r)
		}
	})
	v, err := newOIDCVerifier(issuer, origin)
	if err != nil {
		t.Fatal(err)
	}
	v.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		return recorder.Result(), nil
	})}
	sign := func(audience, typ string, signingKey *rsa.PrivateKey) string {
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iss": issuer, "aud": audience, "sub": "alice", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix(),
			"groups": []string{"engineers"},
		})
		token.Header["kid"], token.Header["typ"] = "key-1", typ
		encoded, err := token.SignedString(signingKey)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	identity, err := v.Verify(context.Background(), sign(origin+"/api", "at+jwt", key))
	if err != nil || identity.user != issuer+"#alice" || len(identity.groups) != 2 {
		t.Fatalf("valid token: %+v, %v", identity, err)
	}
	for _, raw := range []string{
		sign("https://other.example/api", "at+jwt", key),
		sign(origin+"/api", "JWT", key),
		sign(origin+"/api", "at+jwt", mustRSAKey(t)),
	} {
		if _, err := v.Verify(context.Background(), raw); err == nil {
			t.Fatal("invalid token was accepted")
		}
	}
}

func mustRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
