package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type unsignedResolver struct {
	protocol receiverProtocol
	binding  receiverBinding
}

func (r unsignedResolver) Protocol(context.Context, string) (receiverProtocol, error) {
	return r.protocol, nil
}
func (r unsignedResolver) Binding(context.Context, string, string) (receiverBinding, error) {
	return r.binding, nil
}
func (r unsignedResolver) Bindings(context.Context, string) ([]receiverBinding, error) {
	return []receiverBinding{r.binding}, nil
}
func (r unsignedResolver) BindingKey(context.Context, receiverBinding) ([]byte, error) {
	panic("unsigned receiver must not read a Secret")
}

func TestUnsignedReceiverOnlyOnInternalNamedRoute(t *testing.T) {
	protocol, _ := sampleReceiver(t, "ClusterWebhookReceiver", "alertmanager")
	_, binding := sampleReceiver(t, "WebhookReceiver", "alerts-by-instance")
	binding.Metadata.UID = "receiver-uid"
	protocol.Spec.Event.EventsCEL = "[]"
	p := &publisher{resolver: unsignedResolver{protocol: protocol, binding: binding}}
	for _, tc := range []struct {
		name, path string
		internal   bool
		want       int
	}{
		{"public named", "/hooks/ns/alerts-by-instance", false, http.StatusForbidden},
		{"internal shared", "/hooks/alertmanager", true, http.StatusForbidden},
		{"internal named", "/hooks/ns/alerts-by-instance", true, http.StatusAccepted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{"alerts":[]}`))
			if strings.Count(tc.path, "/") == 3 {
				request.SetPathValue("namespace", "ns")
				request.SetPathValue("receiver", "alerts-by-instance")
			} else {
				request.SetPathValue("source", "alertmanager")
			}
			response := httptest.NewRecorder()
			p.handleReceiver(response, request, tc.internal)
			if response.Code != tc.want {
				t.Fatalf("got %d, want %d: %s", response.Code, tc.want, response.Body.String())
			}
		})
	}
}

func TestEndpointAssignsRoutingOrigin(t *testing.T) {
	for _, tc := range []struct {
		internal bool
		want     string
	}{{false, "external"}, {true, "internal"}} {
		key, err := endpointKey([]string{"alertmanager", "fingerprint"}, tc.internal)
		if err != nil || len(key) != 3 || key[0] != tc.want || key[1] != "alertmanager" {
			t.Fatalf("key=%v err=%v", key, err)
		}
	}
	key, err := endpointKey([]string{"internal", "forged"}, false)
	if err != nil || key[0] != "external" || key[1] != "internal" {
		t.Fatalf("sender controlled origin: %v err=%v", key, err)
	}
	key, err = endpointKey(nil, true)
	if err != nil || len(key) != 0 {
		t.Fatalf("empty control event key changed: %v err=%v", key, err)
	}
}
