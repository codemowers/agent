package main

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/ext"
)

type receiverProtocol struct {
	Metadata struct {
		Name string `json:"name"`
		UID  string `json:"uid"`
	} `json:"metadata"`
	Spec struct {
		Signature *struct {
			Algorithm      string `json:"algorithm"`
			KeyEncoding    string `json:"keyEncoding"`
			Header         string `json:"header"`
			Prefix         string `json:"prefix"`
			Encoding       string `json:"encoding"`
			SignedBytesCEL string `json:"signedBytesCEL"`
			Freshness      *struct {
				TimestampHeader string `json:"timestampHeader"`
				MaxSkew         string `json:"maxSkew"`
			} `json:"freshness"`
		} `json:"signature"`
		Event struct {
			RequiredHeaders    []string `json:"requiredHeaders"`
			AcceptCEL          string   `json:"acceptCEL"`
			EventsCEL          string   `json:"eventsCEL"`
			AllowedKeyPrefixes []string `json:"allowedKeyPrefixes"`
		} `json:"event"`
		Response struct {
			Status        int `json:"status"`
			DefaultStatus int `json:"defaultStatus"`
			Rules         []struct {
				WhenCEL     string `json:"whenCEL"`
				Status      int    `json:"status"`
				ContentType string `json:"contentType"`
				BodyCEL     string `json:"bodyCEL"`
			} `json:"rules"`
		} `json:"response"`
	} `json:"spec"`
}

// webhookReceiver is a WebhookReceiver or, with an empty namespace, a
// ClusterWebhookReceiver.
type webhookReceiver struct {
	Metadata struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
		UID       string `json:"uid"`
	} `json:"metadata"`
	Spec struct {
		ProtocolRef string            `json:"protocolRef"`
		Match       map[string]string `json:"match"`
		Senders     struct {
			SecretKeyRef *struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
				Key       string `json:"key"`
			} `json:"secretKeyRef"`
			GitHubMeta       string   `json:"githubMeta"`
			CIDRs            []string `json:"cidrs"`
			ServiceAccounts  []string `json:"serviceAccounts"`
			Audience         string   `json:"audience"`
			Ed25519PublicKey string   `json:"ed25519PublicKey"`
		} `json:"senders"`
	} `json:"spec"`
}

func (r webhookReceiver) kind() string {
	if r.Metadata.Namespace == "" {
		return "ClusterWebhookReceiver"
	}
	return "WebhookReceiver"
}

type receiverBinding struct {
	Metadata struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
		UID       string `json:"uid"`
	} `json:"metadata"`
	Spec struct {
		ReceiverRef struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"receiverRef"`
		AgentRef    string   `json:"agentRef"`
		KeyPrefixes []string `json:"keyPrefixes"`
		Routing     struct {
			KeyCEL    string `json:"keyCEL"`
			Otherwise struct {
				TaskRef string `json:"taskRef"`
			} `json:"otherwise"`
			CoalesceFor string `json:"coalesceFor"`
		} `json:"routing"`
	} `json:"spec"`
}

// subscribes reports whether the binding references the receiver: a
// WebhookReceiver only from its own namespace.
func (b receiverBinding) subscribes(r webhookReceiver) bool {
	return b.Spec.ReceiverRef.Kind == r.kind() && b.Spec.ReceiverRef.Name == r.Metadata.Name &&
		(r.Metadata.Namespace == "" || b.Metadata.Namespace == r.Metadata.Namespace)
}

type receiverBindingList struct {
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
	Items []receiverBinding `json:"items"`
}
type normalizedEvent struct {
	Version      int    `json:"version"`
	Namespace    string `json:"namespace"`
	Agent        string `json:"agent"`
	Binding      string `json:"binding"`
	BindingUID   string `json:"bindingUID"`
	Protocol     string `json:"protocol"`
	DeliveryID   string `json:"deliveryID"`
	Type         string `json:"type"`
	Actor        string `json:"actor"`
	Text         string `json:"text"`
	URL          string `json:"url,omitempty"`
	Key          string `json:"key,omitempty"`
	FallbackTask string `json:"fallbackTask,omitempty"`
	CoalesceFor  string `json:"coalesceFor,omitempty"`
}

func (k *kubeHookResolver) Protocol(ctx context.Context, name string) (receiverProtocol, error) {
	var v receiverProtocol
	if !validKubernetesName(name) {
		return v, fmt.Errorf("invalid protocol name")
	}
	err := k.get(ctx, "/apis/codemowers.cloud/v1alpha1/webhookprotocols/"+url.PathEscape(name), &v)
	return v, err
}

// Receiver reads a WebhookReceiver, or a ClusterWebhookReceiver when
// namespace is empty.
func (k *kubeHookResolver) Receiver(ctx context.Context, namespace, name string) (webhookReceiver, error) {
	var v webhookReceiver
	if !validKubernetesName(name) || (namespace != "" && !validKubernetesName(namespace)) {
		return v, fmt.Errorf("invalid receiver path")
	}
	path := "/apis/codemowers.cloud/v1alpha1/clusterwebhookreceivers/" + url.PathEscape(name)
	if namespace != "" {
		path = "/apis/codemowers.cloud/v1alpha1/namespaces/" + url.PathEscape(namespace) + "/webhookreceivers/" + url.PathEscape(name)
	}
	err := k.get(ctx, path, &v)
	return v, err
}

// ReceiverKey reads the signing key a receiver's secretKeyRef selects: in the
// receiver's namespace, or in the named namespace for a cluster receiver.
func (k *kubeHookResolver) ReceiverKey(ctx context.Context, r webhookReceiver) ([]byte, error) {
	ref := r.Spec.Senders.SecretKeyRef
	if ref == nil {
		return nil, fmt.Errorf("receiver has no secretKeyRef")
	}
	namespace := ref.Namespace
	if r.Metadata.Namespace != "" {
		namespace = r.Metadata.Namespace
	}
	if !validKubernetesName(namespace) || !validKubernetesName(ref.Name) || !validSecretKey(ref.Key) {
		return nil, fmt.Errorf("invalid secretKeyRef")
	}
	var secret struct {
		Data map[string][]byte `json:"data"`
	}
	if err := k.get(ctx, "/api/v1/namespaces/"+url.PathEscape(namespace)+"/secrets/"+url.PathEscape(ref.Name), &secret); err != nil {
		return nil, err
	}
	key := secret.Data[ref.Key]
	if len(key) == 0 {
		return nil, fmt.Errorf("signing key is missing: %w", errNotFound)
	}
	return key, nil
}

// Bindings lists the WebhookBindings subscribed to a receiver: of every
// namespace for a ClusterWebhookReceiver, of its own for a WebhookReceiver.
func (k *kubeHookResolver) Bindings(ctx context.Context, r webhookReceiver) ([]receiverBinding, error) {
	base := "/apis/codemowers.cloud/v1alpha1/webhookbindings"
	if r.Metadata.Namespace != "" {
		base = "/apis/codemowers.cloud/v1alpha1/namespaces/" + url.PathEscape(r.Metadata.Namespace) + "/webhookbindings"
	}
	var out []receiverBinding
	path := base + "?limit=500"
	for {
		var page receiverBindingList
		if err := k.get(ctx, path, &page); err != nil {
			return nil, err
		}
		for _, b := range page.Items {
			if b.subscribes(r) {
				out = append(out, b)
			}
		}
		if page.Metadata.Continue == "" {
			return out, nil
		}
		path = base + "?limit=500&continue=" + url.QueryEscape(page.Metadata.Continue)
	}
}

func receiverEnv() (*cel.Env, error) {
	return cel.NewEnv(ext.Strings(),
		cel.Variable("rawBody", cel.BytesType), cel.Variable("body", cel.DynType), cel.Variable("headers", cel.MapType(cel.StringType, cel.StringType)), cel.Variable("receiver", cel.DynType), cel.Variable("event", cel.DynType),
		cel.Function("urlHost", cel.Overload("url_host_string", []*cel.Type{cel.StringType}, cel.StringType, cel.UnaryBinding(func(v ref.Val) ref.Val {
			u, e := url.Parse(v.Value().(string))
			if e != nil {
				return types.String("")
			}
			return types.String(strings.ToLower(u.Hostname()))
		}))),
		cel.Function("sha256Hex", cel.Overload("sha256_hex_bytes", []*cel.Type{cel.BytesType}, cel.StringType, cel.UnaryBinding(func(v ref.Val) ref.Val {
			sum := sha256.Sum256(v.Value().([]byte))
			return types.String(hex.EncodeToString(sum[:]))
		}))),
	)
}
func evalReceiver(env *cel.Env, expression string, vars map[string]any) (any, error) {
	if len(expression) > 16384 {
		return nil, fmt.Errorf("CEL expression is too long")
	}
	ast, issues := env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}
	program, err := env.Program(ast, cel.CostLimit(100000))
	if err != nil {
		return nil, err
	}
	result, _, err := program.Eval(vars)
	if err != nil {
		return nil, err
	}
	native, err := result.ConvertToNative(reflect.TypeOf((*any)(nil)).Elem())
	if err != nil {
		return nil, err
	}
	return plainCELValue(native), nil
}

func plainCELValue(value any) any {
	switch v := value.(type) {
	case map[any]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			name, ok := key.(string)
			if !ok {
				return nil
			}
			out[name] = plainCELValue(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = plainCELValue(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = plainCELValue(item)
		}
		return out
	case ref.Val:
		return plainCELValue(v.Value())
	default:
		return value
	}
}
func receiverHeaders(r *http.Request) map[string]string {
	m := map[string]string{}
	for k, v := range r.Header {
		if len(v) > 0 {
			m[strings.ToLower(k)] = v[0]
		}
	}
	for _, k := range []string{"x-github-event", "x-github-delivery", "x-gitea-event", "x-gitea-event-type", "x-gitea-delivery", "x-signature-ed25519", "x-signature-timestamp"} {
		if _, ok := m[k]; !ok {
			m[k] = ""
		}
	}
	return m
}

// checkReceiverSignature verifies the protocol's signature with the
// receiver's key: the shared secret for hmacSha256, the public key for ed25519.
func checkReceiverSignature(r *http.Request, p receiverProtocol, key []byte, raw []byte, vars map[string]any, env *cel.Env) (bool, error) {
	v := p.Spec.Signature
	if v == nil {
		return false, fmt.Errorf("protocol defines no signature")
	}
	if v.Algorithm == "hmacSha256" && v.KeyEncoding == "hex" {
		decoded, err := hex.DecodeString(strings.TrimSpace(string(key)))
		if err != nil {
			return false, fmt.Errorf("invalid signing key")
		}
		key = decoded
	}
	if v.Freshness != nil {
		duration, err := time.ParseDuration(v.Freshness.MaxSkew)
		if err != nil || duration <= 0 || duration > time.Hour {
			return false, fmt.Errorf("invalid freshness")
		}
		stamp, err := parseReceiverTime(r.Header.Get(v.Freshness.TimestampHeader))
		if err != nil || time.Since(stamp) > duration || time.Until(stamp) > duration {
			return false, nil
		}
	}
	signed := raw
	if v.SignedBytesCEL != "" {
		value, err := evalReceiver(env, v.SignedBytesCEL, vars)
		if err != nil {
			return false, err
		}
		bytes, ok := value.([]byte)
		if !ok {
			return false, fmt.Errorf("signedBytesCEL must return bytes")
		}
		signed = bytes
	}
	signature, ok := strings.CutPrefix(r.Header.Get(v.Header), v.Prefix)
	if !ok || signature == "" {
		return false, nil
	}
	var decoded []byte
	var err error
	switch v.Encoding {
	case "hex":
		decoded, err = hex.DecodeString(signature)
	case "base64":
		decoded, err = base64.StdEncoding.DecodeString(signature)
	default:
		return false, fmt.Errorf("unsupported signature encoding")
	}
	if err != nil {
		return false, nil
	}
	switch v.Algorithm {
	case "hmacSha256":
		mac := hmac.New(sha256.New, key)
		mac.Write(signed)
		return hmac.Equal(mac.Sum(nil), decoded), nil
	case "ed25519":
		return len(key) == ed25519.PublicKeySize && ed25519.Verify(ed25519.PublicKey(key), signed, decoded), nil
	default:
		return false, fmt.Errorf("unsupported signature algorithm")
	}
}

func parseReceiverTime(s string) (time.Time, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err == nil {
		return time.Unix(n, 0), nil
	}
	return time.Parse(time.RFC3339, s)
}

func (p *publisher) receiverHandler(w http.ResponseWriter, r *http.Request) {
	receiver, err := p.resolver.Receiver(r.Context(), r.PathValue("namespace"), r.PathValue("receiver"))
	if errors.Is(err, errNotFound) {
		http.Error(w, "unknown receiver", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "receiver unavailable", 503)
		return
	}
	protocol, err := p.resolver.Protocol(r.Context(), receiver.Spec.ProtocolRef)
	if err != nil {
		http.Error(w, "protocol unavailable", 503)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			http.Error(w, "payload too large", 413)
		} else {
			http.Error(w, "read error", 400)
		}
		return
	}
	env, err := receiverEnv()
	if err != nil {
		http.Error(w, "CEL unavailable", 503)
		return
	}
	headers := receiverHeaders(r)
	vars := map[string]any{"rawBody": raw, "headers": headers, "body": map[string]any{}, "receiver": map[string]any{"match": receiver.Spec.Match}, "event": map[string]any{}}
	// Authenticate the sender before evaluating any payload-dependent CEL.
	status, err := p.authenticateSender(r, receiver, protocol, raw, vars, env)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	for _, h := range protocol.Spec.Event.RequiredHeaders {
		if headers[strings.ToLower(h)] == "" {
			http.Error(w, "missing required header", 400)
			return
		}
	}
	var body any
	if err := json.Unmarshal(raw, &body); err != nil {
		http.Error(w, "malformed JSON", 400)
		return
	}
	vars["body"] = body
	accepted := true
	if expression := protocol.Spec.Event.AcceptCEL; expression != "" {
		value, err := evalReceiver(env, expression, vars)
		if err != nil {
			http.Error(w, "invalid acceptCEL", 422)
			return
		}
		accepted = value == true
	}
	var pending []normalizedEvent
	if accepted {
		value, err := evalReceiver(env, protocol.Spec.Event.EventsCEL, vars)
		if err != nil {
			http.Error(w, "invalid eventsCEL", 422)
			return
		}
		events, ok := value.([]any)
		if !ok || len(events) > 100 {
			http.Error(w, "eventsCEL must return a bounded list", 422)
			return
		}
		bindings, err := p.resolver.Bindings(r.Context(), receiver)
		if err != nil {
			http.Error(w, "webhook bindings unavailable", 503)
			return
		}
		// Each event reaches every subscribed binding whose key prefixes it matches.
		for _, b := range bindings {
			if !validKubernetesName(b.Metadata.Namespace) || !validKubernetesName(b.Metadata.Name) ||
				!validKubernetesName(b.Spec.AgentRef) || b.Metadata.UID == "" || !b.subscribes(receiver) ||
				(receiver.kind() == "ClusterWebhookReceiver" && len(b.Spec.KeyPrefixes) == 0) {
				continue
			}
			for _, entry := range events {
				event, ok := entry.(map[string]any)
				if !ok {
					http.Error(w, "invalid normalized event", 422)
					return
				}
				event = maps.Clone(event)
				if b.Spec.Routing.KeyCEL != "" {
					vars["event"] = event
					key, err := evalReceiver(env, b.Spec.Routing.KeyCEL, vars)
					if err != nil {
						http.Error(w, "invalid keyCEL", 422)
						return
					}
					event["key"] = key
				}
				normalized, err := normalizeReceiverEvent(b, protocol, event)
				if err != nil {
					http.Error(w, err.Error(), 422)
					return
				}
				if len(b.Spec.KeyPrefixes) > 0 && !slices.ContainsFunc(b.Spec.KeyPrefixes, func(prefix string) bool {
					return normalized.Key != "" && keyHasPrefix(normalized.Key, prefix)
				}) {
					continue
				}
				payload, _ := json.Marshal(normalized)
				if len(payload) > 16384 {
					http.Error(w, "normalized event too large", 422)
					return
				}
				pending = append(pending, normalized)
			}
		}
	}
	status = protocol.Spec.Response.Status
	if status == 0 {
		status = protocol.Spec.Response.DefaultStatus
	}
	if status == 0 {
		status = 202
	}
	var responseBody string
	var responseContentType string
	for _, rule := range protocol.Spec.Response.Rules {
		value, err := evalReceiver(env, rule.WhenCEL, vars)
		if err != nil {
			http.Error(w, "invalid response rule", 422)
			return
		}
		if value != true {
			continue
		}
		if rule.Status != 0 {
			status = rule.Status
		}
		responseContentType = rule.ContentType
		if rule.BodyCEL != "" {
			body, err := evalReceiver(env, rule.BodyCEL, vars)
			if err != nil {
				http.Error(w, "invalid response body", 422)
				return
			}
			var ok bool
			responseBody, ok = body.(string)
			if !ok || len(responseBody) > 4096 {
				http.Error(w, "invalid response body", 422)
				return
			}
		}
		break
	}
	for _, event := range pending {
		payload, err := json.Marshal(event)
		if err != nil {
			http.Error(w, "encode event", 500)
			return
		}
		publish := p.publishEnvelope
		if p.publish != nil {
			publish = p.publish
		}
		if err := publish(event, receiverMessageID(event), payload); err != nil {
			http.Error(w, "event publish failed", 503)
			return
		}
	}
	if recorder, ok := w.(*hookRecorder); ok {
		recorder.published = len(pending)
	}
	if responseContentType != "" {
		w.Header().Set("Content-Type", responseContentType)
	}
	w.WriteHeader(status)
	if responseBody != "" {
		_, _ = io.WriteString(w, responseBody)
	}
}

func receiverMessageID(event normalizedEvent) string {
	key, _ := json.Marshal([]any{event.BindingUID, event.DeliveryID, event.Type, event.Key})
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])
}
func normalizeReceiverEvent(b receiverBinding, p receiverProtocol, e map[string]any) (normalizedEvent, error) {
	n := normalizedEvent{Version: 2, Namespace: b.Metadata.Namespace, Agent: b.Spec.AgentRef, Binding: b.Metadata.Name, BindingUID: b.Metadata.UID, Protocol: p.Metadata.Name, FallbackTask: b.Spec.Routing.Otherwise.TaskRef, CoalesceFor: b.Spec.Routing.CoalesceFor}
	if n.CoalesceFor != "" {
		d, err := time.ParseDuration(n.CoalesceFor)
		if err != nil || d < time.Second || d > time.Hour {
			return n, fmt.Errorf("coalesceFor must be a duration from 1s to 1h")
		}
	}
	for _, field := range []struct {
		key string
		dst *string
	}{{"deliveryID", &n.DeliveryID}, {"type", &n.Type}, {"actor", &n.Actor}, {"text", &n.Text}, {"url", &n.URL}} {
		if v, ok := e[field.key].(string); ok {
			*field.dst = v
		}
	}
	if n.DeliveryID == "" || len(n.DeliveryID) > 160 || n.Type == "" || len(n.Type) > 128 {
		return n, fmt.Errorf("event identity is incomplete")
	}
	if n.Actor == "" {
		n.Actor = n.Protocol
	}
	if len(n.Text) > 4096 {
		n.Text = n.Text[:4096]
	}
	if n.URL != "" {
		parsed, err := url.Parse(n.URL)
		if err != nil || len(n.URL) > 2048 || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil {
			n.URL = ""
		}
	}
	// Keys are canonical resource URIs within the protocol's prefixes. An
	// empty key routes to the binding's fallback Task.
	if value, exists := e["key"]; exists && value != "" {
		raw, ok := value.(string)
		if !ok {
			return n, fmt.Errorf("event key must be a URI string")
		}
		key, err := canonicalKey(raw)
		if err != nil {
			return n, fmt.Errorf("event key: %w", err)
		}
		if !slices.ContainsFunc(p.Spec.Event.AllowedKeyPrefixes, func(prefix string) bool { return keyHasPrefix(key, prefix) }) {
			return n, fmt.Errorf("event key is outside the protocol's allowed prefixes")
		}
		n.Key = key
	}
	return n, nil
}
