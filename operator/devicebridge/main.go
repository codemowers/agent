package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	devicesv1 "github.com/codemowers/agent/operator/api/v1alpha1"
	"github.com/gorilla/websocket"
	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const secretName = "agent-device-keys"
const audience = "agent-devices-mcp"

var deviceKeyPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

type bridge struct {
	namespace string
	objects   client.Client
	kube      kubernetes.Interface
	mu        sync.Mutex
	connected map[string]*deviceConn
}

type deviceConn struct {
	ws      *websocket.Conn
	uid     string
	key     string
	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]chan commandResult
	next    uint64
}

type commandResult struct {
	ID       string `json:"id"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Content  string `json:"content,omitempty"`
}

func main() {
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		log.Fatal("POD_NAMESPACE is required")
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := devicesv1.AddToScheme(scheme); err != nil {
		log.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		log.Fatal(err)
	}
	objects, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		log.Fatal(err)
	}
	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}
	b := &bridge{namespace: namespace, objects: objects, kube: kube, connected: make(map[string]*deviceConn)}
	go b.reconcileLoop()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/devices/", b.connectDevice)
	mux.HandleFunc("/mcp", b.handleMCP)
	server := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(server.ListenAndServe())
}

func (b *bridge) devices(ctx context.Context) ([]devicesv1.Device, error) {
	var list devicesv1.DeviceList
	if err := b.objects.List(ctx, &list, client.InNamespace(b.namespace)); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (b *bridge) keyFor(ctx context.Context, d *devicesv1.Device) (string, error) {
	if d.Spec.ConnectionSecretRef.Name != secretName {
		return "", errors.New("invalid secret reference")
	}
	var secret corev1.Secret
	if err := b.objects.Get(ctx, client.ObjectKey{Namespace: b.namespace, Name: secretName}, &secret); err != nil {
		return "", err
	}
	key := string(secret.Data[d.Spec.ConnectionSecretRef.Key])
	if !deviceKeyPattern.MatchString(key) {
		return "", errors.New("invalid device key")
	}
	return key, nil
}

func (b *bridge) deviceForKey(ctx context.Context, key string) (*devicesv1.Device, error) {
	if !deviceKeyPattern.MatchString(key) {
		return nil, errors.New("invalid key")
	}
	devices, err := b.devices(ctx)
	if err != nil {
		return nil, err
	}
	var found *devicesv1.Device
	for i := range devices {
		candidate, err := b.keyFor(ctx, &devices[i])
		if err != nil {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(key)) == 1 {
			if found != nil {
				return nil, errors.New("duplicate device key")
			}
			found = &devices[i]
		}
	}
	if found == nil {
		return nil, errors.New("unknown device key")
	}
	return found, nil
}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }, ReadBufferSize: 4096, WriteBufferSize: 4096}

func (b *bridge) connectDevice(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/devices/")
	if !deviceKeyPattern.MatchString(key) || strings.Contains(key, "/") {
		http.NotFound(w, r)
		return
	}
	d, err := b.deviceForKey(r.Context(), key)
	if err != nil {
		http.Error(w, "device unavailable", http.StatusForbidden)
		return
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &deviceConn{ws: ws, uid: string(d.UID), key: key, pending: make(map[string]chan commandResult)}
	ws.SetReadLimit(2 << 20)
	b.mu.Lock()
	previous := b.connected[d.Name]
	b.connected[d.Name] = c
	b.mu.Unlock()
	if previous != nil {
		previous.ws.Close()
	}
	b.setStatus(d.Name, c, true, "", "")
	defer func() {
		ws.Close()
		b.mu.Lock()
		if b.connected[d.Name] == c {
			delete(b.connected, d.Name)
		}
		b.mu.Unlock()
		b.setStatus(d.Name, c, false, "", "")
	}()
	for {
		_, raw, err := ws.ReadMessage()
		if err != nil {
			return
		}
		var message struct {
			Type     string `json:"type"`
			Hostname string `json:"hostname"`
			Version  string `json:"version"`
			ID       string `json:"id"`
		}
		if json.Unmarshal(raw, &message) != nil {
			continue
		}
		if message.Type == "hello" {
			b.setStatus(d.Name, c, true, limit(message.Hostname, 100), limit(message.Version, 40))
			continue
		}
		if message.ID == "" {
			continue
		}
		var result commandResult
		if json.Unmarshal(raw, &result) != nil {
			continue
		}
		c.mu.Lock()
		response := c.pending[result.ID]
		delete(c.pending, result.ID)
		c.mu.Unlock()
		if response != nil {
			response <- result
		}
	}
}

func limit(value string, n int) string {
	if len(value) > n {
		return value[:n]
	}
	return value
}

func (b *bridge) setStatus(name string, c *deviceConn, connected bool, hostname, version string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var d devicesv1.Device
	if b.objects.Get(ctx, client.ObjectKey{Namespace: b.namespace, Name: name}, &d) != nil || string(d.UID) != c.uid {
		return
	}
	// An old connection must never overwrite its replacement's status.
	b.mu.Lock()
	current := b.connected[name]
	b.mu.Unlock()
	if connected && current != c || !connected && current != nil {
		return
	}
	now := metav1.Now()
	wasConnected := d.Status.Connected
	d.Status.Connected = connected
	if connected {
		if !wasConnected {
			d.Status.ConnectedAt = &now
		}
		d.Status.LastSeenAt = &now
		if hostname != "" {
			d.Status.Hostname = hostname
		}
		if version != "" {
			d.Status.Version = version
		}
	}
	if err := b.objects.Status().Update(ctx, &d); err != nil {
		log.Printf("device status %s: %v", name, err)
	}
}

func (b *bridge) reconcileLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		b.reconcile()
		<-ticker.C
	}
}

func (b *bridge) reconcile() {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	devices, err := b.devices(ctx)
	if err != nil {
		log.Printf("device reconcile: %v", err)
		return
	}
	known := make(map[string]devicesv1.Device, len(devices))
	for _, d := range devices {
		known[d.Name] = d
	}
	b.mu.Lock()
	current := make(map[string]*deviceConn, len(b.connected))
	for name, c := range b.connected {
		current[name] = c
	}
	b.mu.Unlock()
	for name, c := range current {
		d, ok := known[name]
		if !ok || string(d.UID) != c.uid {
			c.ws.Close()
			continue
		}
		key, err := b.keyFor(ctx, &d)
		if err != nil || subtle.ConstantTimeCompare([]byte(key), []byte(c.key)) != 1 {
			c.ws.Close()
		}
	}
	for _, d := range devices {
		if current[d.Name] == nil && d.Status.Connected {
			d.Status.Connected = false
			if err := b.objects.Status().Update(ctx, &d); err != nil {
				log.Printf("stale device status %s: %v", d.Name, err)
			}
		}
	}
}

func (b *bridge) authenticate(r *http.Request) (*authnv1.UserInfo, error) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return nil, errors.New("missing bearer token")
	}
	token := strings.TrimPrefix(header, "Bearer ")
	if token == "" {
		return nil, errors.New("missing bearer token")
	}
	review, err := b.kube.AuthenticationV1().TokenReviews().Create(r.Context(), &authnv1.TokenReview{Spec: authnv1.TokenReviewSpec{Token: token, Audiences: []string{audience}}}, metav1.CreateOptions{})
	if err != nil || !review.Status.Authenticated || review.Status.Error != "" {
		return nil, errors.New("token rejected")
	}
	user := review.Status.User
	if !strings.HasPrefix(user.Username, "system:serviceaccount:"+b.namespace+":") {
		return nil, errors.New("wrong service account namespace")
	}
	return &user, nil
}

// Device verbs are capabilities rather than standard read verbs, so a broad
// read-only grant never yields a shell.
var deviceVerbs = []string{"exec", "read", "write"}

// toolVerbs maps each tool to the verb it requires on the Device.
var toolVerbs = map[string]string{"device_shell": "exec", "device_file_read": "read", "device_file_write": "write"}

// allowedVerbs returns the Device verbs the caller holds, in deviceVerbs order.
func (b *bridge) allowedVerbs(ctx context.Context, user *authnv1.UserInfo, name string) []string {
	var verbs []string
	for _, verb := range deviceVerbs {
		if b.authorized(ctx, user, name, verb) {
			verbs = append(verbs, verb)
		}
	}
	return verbs
}

func (b *bridge) authorized(ctx context.Context, user *authnv1.UserInfo, name, verb string) bool {
	review, err := b.kube.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authzv1.SubjectAccessReview{Spec: authzv1.SubjectAccessReviewSpec{
		User: user.Username, UID: user.UID, Groups: user.Groups, Extra: convertExtra(user.Extra),
		ResourceAttributes: &authzv1.ResourceAttributes{Namespace: b.namespace, Verb: verb, Group: "codemowers.cloud", Resource: "devices", Name: name},
	}}, metav1.CreateOptions{})
	return err == nil && review.Status.Allowed && !review.Status.Denied && review.Status.EvaluationError == ""
}

func convertExtra(extra map[string]authnv1.ExtraValue) map[string]authzv1.ExtraValue {
	out := make(map[string]authzv1.ExtraValue, len(extra))
	for key, value := range extra {
		out[key] = authzv1.ExtraValue(value)
	}
	return out
}

func (b *bridge) command(ctx context.Context, name, command string, args any) (commandResult, error) {
	var d devicesv1.Device
	if err := b.objects.Get(ctx, client.ObjectKey{Namespace: b.namespace, Name: name}, &d); err != nil {
		return commandResult{}, errors.New("device unavailable")
	}
	key, err := b.keyFor(ctx, &d)
	if err != nil {
		return commandResult{}, errors.New("device unavailable")
	}
	b.mu.Lock()
	c := b.connected[name]
	b.mu.Unlock()
	if c == nil || c.uid != string(d.UID) || subtle.ConstantTimeCompare([]byte(key), []byte(c.key)) != 1 {
		return commandResult{}, errors.New("device offline")
	}
	c.mu.Lock()
	c.next++
	id := fmt.Sprintf("%d", c.next)
	response := make(chan commandResult, 1)
	c.pending[id] = response
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	payload, _ := json.Marshal(map[string]any{"id": id, "cmd": command, "args": args})
	c.writeMu.Lock()
	err = c.ws.WriteMessage(websocket.TextMessage, payload)
	c.writeMu.Unlock()
	if err != nil {
		return commandResult{}, errors.New("device disconnected")
	}
	select {
	case result := <-response:
		return result, nil
	case <-ctx.Done():
		return commandResult{}, ctx.Err()
	}
}
