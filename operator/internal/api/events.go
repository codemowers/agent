package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"time"

	"k8s.io/client-go/tools/cache"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
)

// Events forwards Kubernetes cache changes as invalidation hints. API reads
// remain authoritative, and no CR content is sent through this stream. The
// one exception is a working turn's streamed text, which exists only here.
type Events struct {
	Cache ctrlcache.Cache
	mu    sync.Mutex
	next  uint64
	subs  map[uint64]*eventSubscriber
}

type eventSubscriber struct {
	agents map[string]bool
	ch     chan string
	turns  chan turnText
}

// turnText carries a working turn's whole text so far, so a dropped frame
// costs nothing once a later one arrives.
type turnText struct {
	Namespace string `json:"namespace"`
	Task      string `json:"task"`
	Turn      string `json:"turn"`
	Content   string `json:"content"`
}

func NewEvents(cache ctrlcache.Cache) *Events {
	return &Events{Cache: cache, subs: make(map[uint64]*eventSubscriber)}
}

func agentTopic(namespace, name string) string { return namespace + "/" + name }

func (e *Events) subscribe(agents map[string]bool) (<-chan string, <-chan turnText, func()) {
	e.mu.Lock()
	e.next++
	id := e.next
	sub := &eventSubscriber{agents: agents, ch: make(chan string, 2), turns: make(chan turnText, 8)}
	e.subs[id] = sub
	e.mu.Unlock()
	return sub.ch, sub.turns, func() {
		e.mu.Lock()
		delete(e.subs, id)
		e.mu.Unlock()
	}
}

func (e *Events) publish(kind, topic string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, sub := range e.subs {
		if kind == "tasks" && !sub.agents[topic] {
			continue
		}
		select {
		case sub.ch <- kind:
		default: // A pending invalidation already requires a fresh API read.
		}
	}
}

func (e *Events) publishTurn(topic string, text turnText) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, sub := range e.subs {
		if !sub.agents[topic] {
			continue
		}
		select {
		case sub.turns <- text:
		default: // A slow browser catches up with the next frame.
		}
	}
}

// NeedLeaderElection keeps the stream serving browsers on every replica.
func (e *Events) NeedLeaderElection() bool { return false }

func (e *Events) Start(ctx context.Context) error {
	agents, err := e.Cache.GetInformer(ctx, &agentsv1alpha1.Agent{})
	if err != nil {
		return fmt.Errorf("watch Agents: %w", err)
	}
	tasks, err := e.Cache.GetInformer(ctx, &agentsv1alpha1.Task{})
	if err != nil {
		return fmt.Errorf("watch Tasks: %w", err)
	}
	_, err = agents.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(any) { e.publish("agents", "") },
		UpdateFunc: func(old, current any) {
			before, okBefore := old.(*agentsv1alpha1.Agent)
			after, okAfter := current.(*agentsv1alpha1.Agent)
			if okBefore && okAfter && before.Spec.Model.Backend == after.Spec.Model.Backend &&
				before.Spec.Suspended == after.Spec.Suspended {
				return
			}
			e.publish("agents", "")
		},
		DeleteFunc: func(any) { e.publish("agents", "") },
	})
	if err != nil {
		return fmt.Errorf("handle Agent events: %w", err)
	}
	_, err = tasks.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) { e.publishTask(obj) },
		UpdateFunc: func(old, current any) {
			before, okBefore := old.(*agentsv1alpha1.Task)
			after, okAfter := current.(*agentsv1alpha1.Task)
			if okBefore && okAfter && reflect.DeepEqual(taskDisplay(before), taskDisplay(after)) &&
				reflect.DeepEqual(before.Spec.ExternalReferences, after.Spec.ExternalReferences) &&
				reflect.DeepEqual(before.Status.ExternalReferences, after.Status.ExternalReferences) {
				return
			}
			if okBefore && okAfter && taskAgentTopic(before) != taskAgentTopic(after) {
				e.publishTask(before)
			}
			e.publishTask(current)
		},
		DeleteFunc: func(obj any) { e.publishTask(obj) },
	})
	if err != nil {
		return fmt.Errorf("handle Task events: %w", err)
	}
	<-ctx.Done()
	return nil
}

func displayIdleSeconds(task *agentsv1alpha1.Task) int32 {
	if task.Spec.IdleSeconds != nil {
		return *task.Spec.IdleSeconds
	}
	return agentsv1alpha1.DefaultIdleSeconds
}

func taskDisplay(task *agentsv1alpha1.Task) taskItem {
	item := taskItem{Name: task.Name, Title: task.Spec.Title, Namespace: task.Namespace,
		Phase: string(task.Status.Phase), Reason: task.Status.Reason,
		Objective: task.Spec.Objective, IdleSeconds: displayIdleSeconds(task), Activity: task.Status.Activity,
		CreatedAt: task.CreationTimestamp.Time.Format(time.RFC3339), WorkerPod: task.Status.WorkerPodName,
		WorkerPodUID: task.Status.WorkerPodUID, Suspended: task.Spec.Suspend,
		Completed:    task.Annotations["codemowers.cloud/completed"] == "true",
		DisabledMCPs: task.Spec.DisabledMCPs}
	if task.Status.MCPProbe != nil {
		item.MCPProbeAt = task.Status.MCPProbe.UpdatedAt.Time.Format(time.RFC3339Nano)
	}
	if task.Status.LastResultAt != nil {
		item.LastResultAt = task.Status.LastResultAt.Time.Format(time.RFC3339Nano)
	}
	if task.Annotations["codemowers.cloud/initial-delivery-id"] != "" {
		item.ObjectiveSource = "webhook"
	}
	for _, owner := range task.OwnerReferences {
		if owner.APIVersion == agentsv1alpha1.GroupVersion.String() && owner.Kind == "Agent" {
			item.AgentName = owner.Name + "/" + string(owner.UID)
			break
		}
	}
	if task.Status.Progress != nil {
		item.Summary = task.Status.Progress.Summary
		if !task.Status.Progress.UpdatedAt.IsZero() {
			item.SummaryAt = task.Status.Progress.UpdatedAt.Time.Format(time.RFC3339)
		}
	}
	if n := len(task.Status.Conversation); n > 0 {
		item.LastMessageID = task.Status.Conversation[n-1].ID
	}
	if at := latestActualMessageAt(task); !at.IsZero() {
		item.LastMessageAt = at.Format(time.RFC3339Nano)
	}
	return item
}

func latestActualMessageAt(task *agentsv1alpha1.Task) time.Time {
	latest := task.CreationTimestamp.Time
	if task.Status.LastMessageAt != nil && task.Status.LastMessageAt.Time.After(latest) {
		latest = task.Status.LastMessageAt.Time
	}
	for _, message := range task.Status.Conversation {
		if message.Role != "user" && message.Role != "external" && message.Role != "agent" {
			continue
		}
		if message.CreatedAt.Time.After(latest) {
			latest = message.CreatedAt.Time
		}
	}
	return latest
}

func (e *Events) publishTask(obj any) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	task, ok := obj.(*agentsv1alpha1.Task)
	if !ok {
		return
	}
	if topic := taskAgentTopic(task); topic != "" {
		e.publish("tasks", topic)
	}
}

func taskAgentTopic(task *agentsv1alpha1.Task) string {
	for _, owner := range task.OwnerReferences {
		if owner.APIVersion == agentsv1alpha1.GroupVersion.String() && owner.Kind == "Agent" {
			return agentTopic(task.Namespace, owner.Name)
		}
	}
	return ""
}

func (h *Handler) streamEvents(w http.ResponseWriter, r *http.Request) {
	identity, ok := h.authenticatedCaller(w, r)
	if !ok {
		return
	}
	if h.Events == nil {
		jsonError(w, fmt.Errorf("event stream unavailable"), http.StatusServiceUnavailable)
		return
	}
	var request struct {
		Agents []struct {
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
		} `json:"agents"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request); err != nil || len(request.Agents) > 1000 {
		jsonError(w, fmt.Errorf("invalid event subscription"), http.StatusBadRequest)
		return
	}
	topics := make(map[string]bool, len(request.Agents))
	for _, requested := range request.Agents {
		if requested.Namespace == "" || requested.Name == "" {
			jsonError(w, fmt.Errorf("invalid Agent name"), http.StatusBadRequest)
			return
		}
		var agent agentsv1alpha1.Agent
		if err := h.Client.Get(r.Context(), client.ObjectKey{Namespace: requested.Namespace, Name: requested.Name}, &agent); err != nil {
			jsonError(w, fmt.Errorf("Agent unavailable"), http.StatusForbidden)
			return
		}
		allowed, err := h.canAccessAgent(r, identity, &agent)
		if err != nil {
			jsonError(w, err, http.StatusServiceUnavailable)
			return
		}
		if !allowed {
			jsonError(w, fmt.Errorf("Agent access denied"), http.StatusForbidden)
			return
		}
		topics[agentTopic(agent.Namespace, agent.Name)] = true
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, fmt.Errorf("streaming unavailable"), http.StatusServiceUnavailable)
		return
	}
	updates, turns, unsubscribe := h.Events.subscribe(topics)
	defer unsubscribe()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n") //nolint:errcheck
	flusher.Flush()
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	// Reauthenticate periodically, including after token or RBAC changes.
	deadline := time.NewTimer(4 * time.Minute)
	defer deadline.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": heartbeat\n\n") //nolint:errcheck
		case kind := <-updates:
			fmt.Fprintf(w, "event: %s\ndata: {}\n\n", kind) //nolint:errcheck
		case text := <-turns:
			data, _ := json.Marshal(text)
			fmt.Fprintf(w, "event: turn\ndata: %s\n\n", data) //nolint:errcheck
		}
		flusher.Flush()
	}
}
