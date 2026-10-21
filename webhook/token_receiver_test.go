package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type tokenResolver struct {
	protocol receiverProtocol
	binding  receiverBinding
	tokens   map[string]string
}

func (r tokenResolver) Protocol(context.Context, string) (receiverProtocol, error) {
	return r.protocol, nil
}
func (r tokenResolver) Binding(context.Context, string, string) (receiverBinding, error) {
	return r.binding, nil
}
func (r tokenResolver) Bindings(context.Context, string) ([]receiverBinding, error) {
	return []receiverBinding{r.binding}, nil
}
func (r tokenResolver) BindingKey(context.Context, receiverBinding) ([]byte, error) {
	panic("token binding must not read a Secret")
}
func (r tokenResolver) ReviewToken(_ context.Context, token, audience string) (string, error) {
	if audience != r.protocol.Spec.Verification.Audience {
		return "", nil
	}
	return r.tokens[token], nil
}

func TestServiceAccountTokenReceiverAcceptsOnlyBindingSenders(t *testing.T) {
	protocol, _ := sampleReceiver(t, "WebhookProtocol", "alertmanager")
	_, binding := sampleReceiver(t, "WebhookBinding", "alertmanager")
	binding.Metadata.UID = "binding-uid"
	protocol.Spec.Event.EventsCEL = "[]"
	p := &publisher{resolver: tokenResolver{protocol: protocol, binding: binding, tokens: map[string]string{
		"alertmanager": "system:serviceaccount:monitoring:alertmanager",
		"other":        "system:serviceaccount:monitoring:prometheus",
	}}}
	for _, tc := range []struct {
		name, path, token string
		want              int
	}{
		{"named", "/hooks/agents/alertmanager", "alertmanager", http.StatusAccepted},
		{"shared", "/hooks/alertmanager", "alertmanager", http.StatusAccepted},
		{"missing token", "/hooks/agents/alertmanager", "", http.StatusUnauthorized},
		{"rejected token", "/hooks/agents/alertmanager", "forged", http.StatusUnauthorized},
		{"other sender", "/hooks/agents/alertmanager", "other", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{"alerts":[]}`))
			if tc.token != "" {
				request.Header.Set("Authorization", "Bearer "+tc.token)
			}
			if strings.Count(tc.path, "/") == 3 {
				request.SetPathValue("namespace", "agents")
				request.SetPathValue("binding", "alertmanager")
			} else {
				request.SetPathValue("source", "alertmanager")
			}
			response := httptest.NewRecorder()
			p.receiverHandler(response, request)
			if response.Code != tc.want {
				t.Fatalf("got %d, want %d: %s", response.Code, tc.want, response.Body.String())
			}
		})
	}
}
