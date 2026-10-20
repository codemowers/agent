package main

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
)

type receiverProtocol struct {
	Metadata struct {
		Name string `json:"name"`
		UID  string `json:"uid"`
	} `json:"metadata"`
	Spec struct {
		Verification struct {
			Algorithm   string `json:"algorithm"`
			KeyEncoding string `json:"keyEncoding"`
			Signature   struct {
				Header   string `json:"header"`
				Prefix   string `json:"prefix"`
				Encoding string `json:"encoding"`
			} `json:"signature"`
			SignedBytesCEL string `json:"signedBytesCEL"`
			Freshness      *struct {
				TimestampHeader string `json:"timestampHeader"`
				MaxSkew         string `json:"maxSkew"`
			} `json:"freshness"`
		} `json:"verification"`
		Event struct {
			RequiredHeaders []string `json:"requiredHeaders"`
			AcceptCEL       string   `json:"acceptCEL"`
			EventsCEL       string   `json:"eventsCEL"`
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
type receiverBinding struct {
	Metadata struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
		UID       string `json:"uid"`
	} `json:"metadata"`
	Spec struct {
		SourceRef struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"sourceRef"`
		AgentRef           string            `json:"agentRef"`
		Match              map[string]string `json:"match"`
		VerificationKeyRef *struct {
			Name string `json:"name"`
			Key  string `json:"key"`
		} `json:"verificationKeyRef"`
		Routing struct {
			KeyCEL       string `json:"keyCEL"`
			ExistingTask string `json:"existingTask"`
			Otherwise    struct {
				TaskRef string `json:"taskRef"`
			} `json:"otherwise"`
			CoalesceFor string `json:"coalesceFor"`
		} `json:"routing"`
	} `json:"spec"`
}
type receiverBindingList struct {
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
	Items []receiverBinding `json:"items"`
}
type normalizedEvent struct {
	Version      int      `json:"version"`
	Namespace    string   `json:"namespace"`
	Agent        string   `json:"agent"`
	Receiver     string   `json:"receiver"`
	ReceiverUID  string   `json:"receiverUID"`
	Protocol     string   `json:"protocol"`
	Source       string   `json:"source"`
	DeliveryID   string   `json:"deliveryID"`
	Type         string   `json:"type"`
	Action       string   `json:"action,omitempty"`
	Actor        string   `json:"actor"`
	Text         string   `json:"text"`
	URL          string   `json:"url,omitempty"`
	Key          []string `json:"key"`
	FallbackTask string   `json:"fallbackTask,omitempty"`
	CoalesceFor  string   `json:"coalesceFor,omitempty"`
}

func (k *kubeHookResolver) Protocol(ctx context.Context, name string) (receiverProtocol, error) {
	var v receiverProtocol
	if !validKubernetesName(name) {
		return v, fmt.Errorf("invalid protocol name")
	}
	err := k.get(ctx, "/apis/codemowers.cloud/v1alpha1/clusterwebhookreceivers/"+url.PathEscape(name), &v)
	return v, err
}
func (k *kubeHookResolver) Binding(ctx context.Context, namespace, name string) (receiverBinding, error) {
	var v receiverBinding
	if !validKubernetesName(namespace) || !validKubernetesName(name) {
		return v, fmt.Errorf("invalid receiver path")
	}
	err := k.get(ctx, "/apis/codemowers.cloud/v1alpha1/namespaces/"+url.PathEscape(namespace)+"/webhookreceivers/"+url.PathEscape(name), &v)
	return v, err
}
func (k *kubeHookResolver) Bindings(ctx context.Context, protocol string) ([]receiverBinding, error) {
	var out []receiverBinding
	path := "/apis/codemowers.cloud/v1alpha1/webhookreceivers?limit=100"
	for {
		var page receiverBindingList
		if err := k.get(ctx, path, &page); err != nil {
			return nil, err
		}
		for _, b := range page.Items {
			if b.Spec.SourceRef.Kind == "ClusterWebhookReceiver" && b.Spec.SourceRef.Name == protocol {
				out = append(out, b)
			}
		}
		if len(out) > 100 {
			return nil, fmt.Errorf("too many receiver bindings")
		}
		if page.Metadata.Continue == "" {
			break
		}
		path = "/apis/codemowers.cloud/v1alpha1/webhookreceivers?limit=100&continue=" + url.QueryEscape(page.Metadata.Continue)
	}
	return out, nil
}
func (k *kubeHookResolver) BindingKey(ctx context.Context, b receiverBinding) ([]byte, error) {
	if b.Spec.VerificationKeyRef == nil || b.Spec.VerificationKeyRef.Name != "agent-operator-webhook-keys" || !validSecretKey(b.Spec.VerificationKeyRef.Key) {
		return nil, fmt.Errorf("invalid verification key reference")
	}
	var secret struct {
		Data map[string][]byte `json:"data"`
	}
	path := "/api/v1/namespaces/" + url.PathEscape(b.Metadata.Namespace) + "/secrets/agent-operator-webhook-keys"
	if err := k.get(ctx, path, &secret); err != nil {
		return nil, err
	}
	key := secret.Data[b.Spec.VerificationKeyRef.Key]
	if len(key) == 0 {
		return nil, fmt.Errorf("verification key is missing")
	}
	return key, nil
}

func receiverEnv() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("rawBody", cel.BytesType), cel.Variable("body", cel.DynType), cel.Variable("headers", cel.MapType(cel.StringType, cel.StringType)), cel.Variable("binding", cel.DynType), cel.Variable("event", cel.DynType),
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
	for _, k := range []string{"x-github-event", "x-github-delivery", "x-github-hook-id", "x-gitea-event", "x-gitea-event-type", "x-gitea-delivery", "x-slack-signature", "x-slack-request-timestamp", "x-signature-ed25519", "x-signature-timestamp"} {
		if _, ok := m[k]; !ok {
			m[k] = ""
		}
	}
	return m
}
func checkReceiverSignature(r *http.Request, p receiverProtocol, key, raw []byte, vars map[string]any, env *cel.Env) (bool, error) {
	v := p.Spec.Verification
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
	switch v.Algorithm {
	case "none":
		return true, nil
	case "bearer":
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		return len(got) > 0 && subtle.ConstantTimeCompare(key, []byte(got)) == 1, nil
	case "hmacSha256", "ed25519":
		signature := r.Header.Get(v.Signature.Header)
		if !strings.HasPrefix(signature, v.Signature.Prefix) {
			return false, nil
		}
		signature = strings.TrimPrefix(signature, v.Signature.Prefix)
		var decoded []byte
		var err error
		if v.Signature.Encoding == "hex" {
			decoded, err = hex.DecodeString(signature)
		} else if v.Signature.Encoding == "base64" {
			decoded, err = base64.StdEncoding.DecodeString(signature)
		} else {
			return false, fmt.Errorf("unsupported signature encoding")
		}
		if err != nil {
			return false, nil
		}
		if v.Algorithm == "hmacSha256" {
			if v.KeyEncoding == "hex" {
				key, err = hex.DecodeString(strings.TrimSpace(string(key)))
				if err != nil {
					return false, fmt.Errorf("invalid HMAC key")
				}
			}
			mac := hmac.New(sha256.New, key)
			mac.Write(signed)
			return hmac.Equal(mac.Sum(nil), decoded), nil
		}
		if v.KeyEncoding == "hex" {
			key, err = hex.DecodeString(strings.TrimSpace(string(key)))
			if err != nil {
				return false, fmt.Errorf("invalid Ed25519 key")
			}
		}
		return len(key) == ed25519.PublicKeySize && ed25519.Verify(ed25519.PublicKey(key), signed, decoded), nil
	default:
		return false, fmt.Errorf("unsupported verification algorithm")
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
	p.handleReceiver(w, r, false)
}

func (p *publisher) internalReceiverHandler(w http.ResponseWriter, r *http.Request) {
	p.handleReceiver(w, r, true)
}

func (p *publisher) handleReceiver(w http.ResponseWriter, r *http.Request, internal bool) {
	resolver := p.resolver
	source := r.PathValue("source")
	namespace := r.PathValue("namespace")
	name := r.PathValue("receiver")
	var named *receiverBinding
	if name != "" {
		b, err := resolver.Binding(r.Context(), namespace, name)
		if err != nil {
			http.Error(w, "receiver unavailable", 503)
			return
		}
		if b.Spec.SourceRef.Kind != "ClusterWebhookReceiver" || !validKubernetesName(b.Spec.SourceRef.Name) {
			http.Error(w, "invalid receiver source", 422)
			return
		}
		named = &b
		source = b.Spec.SourceRef.Name
	}
	protocol, err := resolver.Protocol(r.Context(), source)
	if err != nil {
		http.Error(w, "protocol unavailable", 503)
		return
	}
	if protocol.Spec.Verification.Algorithm == "none" {
		if !internal || name == "" || protocol.Spec.Verification.Freshness != nil || protocol.Spec.Verification.SignedBytesCEL != "" {
			http.Error(w, "unsigned receiver requires an internal named route", 403)
			return
		}
	} else if protocol.Spec.Verification.Algorithm == "" {
		http.Error(w, "verification algorithm missing", 422)
		return
	}
	var bindings []receiverBinding
	if named != nil {
		bindings = []receiverBinding{*named}
	} else {
		bindings, err = resolver.Bindings(r.Context(), source)
		if err != nil {
			http.Error(w, "receiver bindings unavailable", 503)
			return
		}
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
	headers := receiverHeaders(r)
	for _, h := range protocol.Spec.Event.RequiredHeaders {
		if headers[strings.ToLower(h)] == "" {
			http.Error(w, "missing required header", 400)
			return
		}
	}
	env, err := receiverEnv()
	if err != nil {
		http.Error(w, "CEL unavailable", 503)
		return
	}
	// Authenticate each candidate before evaluating any payload-dependent CEL.
	var matchedVars map[string]any
	var pending []normalizedEvent
	for _, b := range bindings {
		if !validKubernetesName(b.Metadata.Namespace) || !validKubernetesName(b.Metadata.Name) ||
			!validKubernetesName(b.Spec.AgentRef) || b.Metadata.UID == "" ||
			b.Spec.SourceRef.Kind != "ClusterWebhookReceiver" || b.Spec.SourceRef.Name != source {
			continue
		}
		var key []byte
		if protocol.Spec.Verification.Algorithm == "none" {
			if b.Spec.VerificationKeyRef != nil {
				http.Error(w, "unsigned receiver must not reference a key", 422)
				return
			}
		} else {
			if b.Spec.VerificationKeyRef == nil {
				http.Error(w, "verification key missing", 422)
				return
			}
			key, err = resolver.BindingKey(r.Context(), b)
			if err != nil {
				http.Error(w, "verification key unavailable", 503)
				return
			}
		}
		vars := map[string]any{"rawBody": raw, "headers": headers, "body": map[string]any{}, "binding": map[string]any{"match": b.Spec.Match}, "event": map[string]any{}}
		valid, err := checkReceiverSignature(r, protocol, key, raw, vars, env)
		if err != nil {
			http.Error(w, "invalid verification configuration", 503)
			return
		}
		if !valid {
			continue
		}
		var body any
		if err := json.Unmarshal(raw, &body); err != nil {
			http.Error(w, "malformed JSON", 400)
			return
		}
		vars["body"] = body
		if expression := protocol.Spec.Event.AcceptCEL; expression != "" {
			accepted, err := evalReceiver(env, expression, vars)
			if err != nil {
				http.Error(w, "invalid acceptCEL", 422)
				return
			}
			if accepted != true {
				continue
			}
		}
		if matchedVars != nil {
			http.Error(w, "ambiguous receiver bindings", http.StatusConflict)
			return
		}
		matchedVars = vars
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
		for _, entry := range events {
			event, ok := entry.(map[string]any)
			if !ok {
				http.Error(w, "invalid normalized event", 422)
				return
			}
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
			normalized.Key, err = endpointKey(normalized.Key, internal)
			if err != nil {
				http.Error(w, err.Error(), 422)
				return
			}
			payload, _ := json.Marshal(normalized)
			if len(payload) > 16384 {
				http.Error(w, "normalized event too large", 422)
				return
			}
			pending = append(pending, normalized)
		}
	}
	if matchedVars == nil {
		http.Error(w, "unauthorized", 401)
		return
	}
	status := protocol.Spec.Response.Status
	if status == 0 {
		status = protocol.Spec.Response.DefaultStatus
	}
	if status == 0 {
		status = 202
	}
	var responseBody string
	var responseContentType string
	for _, rule := range protocol.Spec.Response.Rules {
		value, err := evalReceiver(env, rule.WhenCEL, matchedVars)
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
			body, err := evalReceiver(env, rule.BodyCEL, matchedVars)
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
		if err := p.publishEnvelope(event, receiverMessageID(event), payload); err != nil {
			http.Error(w, "event publish failed", 503)
			return
		}
	}
	if responseContentType != "" {
		w.Header().Set("Content-Type", responseContentType)
	}
	w.WriteHeader(status)
	if responseBody != "" {
		_, _ = io.WriteString(w, responseBody)
	}
}

// endpointKey assigns the transport origin after CEL has produced the source key.
// Request bodies and receiver expressions cannot choose the first segment.
func endpointKey(key []string, internal bool) ([]string, error) {
	if len(key) == 0 {
		return nil, nil
	}
	if len(key) >= 16 {
		return nil, fmt.Errorf("event key is too long")
	}
	origin := "external"
	if internal {
		origin = "internal"
	}
	return append([]string{origin}, key...), nil
}

func receiverMessageID(event normalizedEvent) string {
	key, _ := json.Marshal([]any{event.ReceiverUID, event.DeliveryID, event.Source, event.Type, event.Key})
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])
}
func normalizeReceiverEvent(b receiverBinding, p receiverProtocol, e map[string]any) (normalizedEvent, error) {
	n := normalizedEvent{Version: 1, Namespace: b.Metadata.Namespace, Agent: b.Spec.AgentRef, Receiver: b.Metadata.Name, ReceiverUID: b.Metadata.UID, Protocol: p.Metadata.Name, FallbackTask: b.Spec.Routing.Otherwise.TaskRef, CoalesceFor: b.Spec.Routing.CoalesceFor}
	if n.CoalesceFor != "" {
		d, err := time.ParseDuration(n.CoalesceFor)
		if err != nil || d < time.Second || d > time.Hour {
			return n, fmt.Errorf("coalesceFor must be a duration from 1s to 1h")
		}
	}
	for _, field := range []struct {
		key string
		dst *string
	}{{"source", &n.Source}, {"deliveryID", &n.DeliveryID}, {"type", &n.Type}, {"action", &n.Action}, {"actor", &n.Actor}, {"text", &n.Text}, {"url", &n.URL}} {
		if v, ok := e[field.key].(string); ok {
			*field.dst = v
		}
	}
	if n.Source == "" || n.DeliveryID == "" || len(n.DeliveryID) > 160 || n.Type == "" {
		return n, fmt.Errorf("event identity is incomplete")
	}
	if n.Actor == "" {
		n.Actor = n.Source
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
	if value, exists := e["key"]; exists {
		raw, ok := value.([]any)
		if !ok {
			return n, fmt.Errorf("event key must be a list")
		}
		if len(raw) > 16 {
			return n, fmt.Errorf("event key is too long")
		}
		for _, part := range raw {
			s, ok := part.(string)
			if !ok || s == "" || len(s) > 256 {
				return n, fmt.Errorf("invalid key segment")
			}
			n.Key = append(n.Key, s)
		}
	}
	return n, nil
}
