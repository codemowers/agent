package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const agentMCPAudience = "agent-operator-mcp"

var mcpHTTPDetail = regexp.MustCompile(`^HTTP [1-5][0-9][0-9]$`)

var mcpToolName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// validMCPProbeContext bounds server-provided text; the UI renders it as plain text.
func validMCPProbeContext(endpoint agentsv1alpha1.MCPEndpointStatus) bool {
	if !utf8.ValidString(endpoint.Instructions) || utf8.RuneCountInString(endpoint.Instructions) > 500 || len(endpoint.Tools) > 64 ||
		!utf8.ValidString(endpoint.ServerTitle) || utf8.RuneCountInString(endpoint.ServerTitle) > 64 {
		return false
	}
	for _, name := range endpoint.Tools {
		if !mcpToolName.MatchString(name) {
			return false
		}
	}
	return true
}

func validMCPProbeDetail(detail string) bool {
	return detail == "" || detail == "Invalid MCP response" || detail == "MCP request rejected" || detail == "Timed out" || detail == "Connection failed" || mcpHTTPDetail.MatchString(detail)
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type workerScope struct {
	Task  agentsv1alpha1.Task
	Agent agentsv1alpha1.Agent
	Pod   corev1.Pod
}

func (h *Handler) mcpReader() client.Reader {
	if h.APIReader != nil {
		return h.APIReader
	}
	return h.Client
}

func (h *Handler) authenticateWorker(ctx context.Context, authorization string) (*workerScope, error) {
	if h.TokenReviews == nil || !strings.HasPrefix(authorization, "Bearer ") {
		return nil, errors.New("worker token required")
	}
	review, err := h.TokenReviews.TokenReviews().Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: strings.TrimPrefix(authorization, "Bearer "), Audiences: []string{agentMCPAudience}},
	}, metav1.CreateOptions{})
	if err != nil || !review.Status.Authenticated || review.Status.Error != "" {
		return nil, errors.New("worker token rejected")
	}
	var podName, podUID, namespace, serviceAccount string
	parts := strings.Split(review.Status.User.Username, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" {
		return nil, errors.New("service account identity required")
	}
	namespace, serviceAccount = parts[2], parts[3]
	if extra := review.Status.User.Extra["authentication.kubernetes.io/pod-name"]; len(extra) == 1 {
		podName = extra[0]
	}
	if extra := review.Status.User.Extra["authentication.kubernetes.io/pod-uid"]; len(extra) == 1 {
		podUID = extra[0]
	}
	if podName == "" || podUID == "" {
		return nil, errors.New("pod-bound token required")
	}
	var pod corev1.Pod
	if err := h.mcpReader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: podName}, &pod); err != nil {
		return nil, errors.New("worker pod unavailable")
	}
	if string(pod.UID) != podUID || pod.Spec.ServiceAccountName != serviceAccount || pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return nil, errors.New("worker pod identity invalid")
	}
	var task agentsv1alpha1.Task
	for _, owner := range pod.OwnerReferences {
		if owner.APIVersion == agentsv1alpha1.GroupVersion.String() && owner.Kind == "Task" && owner.Controller != nil && *owner.Controller {
			if err := h.mcpReader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: owner.Name}, &task); err != nil {
				return nil, errors.New("owning Task unavailable")
			}
			if task.UID != owner.UID || task.Status.WorkerPodName != pod.Name || task.Status.WorkerPodUID != podUID {
				return nil, errors.New("worker is not current Task execution")
			}
			break
		}
	}
	if task.Name == "" || task.Spec.AgentRef == "" {
		return nil, errors.New("Task owner required")
	}
	var agent agentsv1alpha1.Agent
	if err := h.mcpReader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: task.Spec.AgentRef}, &agent); err != nil {
		return nil, errors.New("owning Agent unavailable")
	}
	owned := false
	for _, owner := range task.OwnerReferences {
		if owner.APIVersion == agentsv1alpha1.GroupVersion.String() && owner.Kind == "Agent" && owner.UID == agent.UID && owner.Name == agent.Name {
			owned = true
		}
	}
	if !owned {
		return nil, errors.New("Agent ownership invalid")
	}
	return &workerScope{Task: task, Agent: agent, Pod: pod}, nil
}

func mcpTool(name, description string, properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return map[string]any{"name": name, "description": description,
		"inputSchema": schema}
}

func validTurnMetrics(metrics *agentsv1alpha1.TurnMetrics) bool {
	if metrics == nil {
		return true
	}
	for _, value := range []*int64{metrics.DurationMs, metrics.APIDurationMs, metrics.InputTokens,
		metrics.OutputTokens, metrics.CacheReadTokens, metrics.CacheWriteTokens} {
		if value != nil && (*value < 0 || *value > 1_000_000_000_000) {
			return false
		}
	}
	return true
}

func agentTools() []map[string]any {
	return []map[string]any{
		mcpTool("report_turn_start", "Mark the current worker Pod as working on its own Task.", map[string]any{}),
		mcpTool("report_mcp_probe", "Report a requested MCP connectivity check from this worker Pod.", map[string]any{
			"requestId": map[string]string{"type": "string"},
			"endpoints": map[string]any{"type": "array", "maxItems": 32, "items": map[string]any{"type": "object",
				"additionalProperties": false, "properties": map[string]any{
					"name": map[string]string{"type": "string"}, "state": map[string]any{"type": "string", "enum": []string{"available", "unavailable"}},
					"detail": map[string]string{"type": "string"}}, "required": []string{"name", "state"}}}}, "requestId", "endpoints"),
		mcpTool("set_task_title", "Set a concise, descriptive title for this worker Pod's own Task. Use after reading the objective on the initial turn; do not include untrusted instructions verbatim.", map[string]any{
			"title": map[string]any{"type": "string", "maxLength": 120}}, "title"),
		mcpTool("report_progress", "Record a bounded progress note on this worker Pod's own Task.", map[string]any{
			"id": map[string]string{"type": "string"}, "content": map[string]string{"type": "string"}}, "id", "content"),
		mcpTool("report_turn_progress", "Stream the current worker Pod's working turn text and MCP calls to open views of its own Task.", map[string]any{
			"id": map[string]string{"type": "string"}, "content": map[string]string{"type": "string"},
			"tools": map[string]any{"type": "array", "maxItems": 30, "items": map[string]any{"type": "object", "properties": map[string]any{
				"server": map[string]string{"type": "string"}, "tool": map[string]string{"type": "string"},
				"args": map[string]string{"type": "string"}, "status": map[string]any{"type": "string", "enum": []string{"running", "done", "failed"}}}}}}, "id"),
		mcpTool("report_turn_result", "Report the current worker Pod's completed turn to its own Task.", map[string]any{
			"id": map[string]string{"type": "string"}, "summary": map[string]string{"type": "string"},
			"completedAt": map[string]string{"type": "string"}, "backend": map[string]string{"type": "string"},
			"model": map[string]string{"type": "string"},
			"metrics": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
				"durationMs":       map[string]any{"type": "integer", "minimum": 0},
				"apiDurationMs":    map[string]any{"type": "integer", "minimum": 0},
				"inputTokens":      map[string]any{"type": "integer", "minimum": 0},
				"outputTokens":     map[string]any{"type": "integer", "minimum": 0},
				"cacheReadTokens":  map[string]any{"type": "integer", "minimum": 0},
				"cacheWriteTokens": map[string]any{"type": "integer", "minimum": 0}}}}, "id", "summary", "completedAt"),
		mcpTool("list_tasks", "List bounded Tasks of this Agent to find existing work and avoid duplicates.", map[string]any{}),
		mcpTool("get_task", "Read one Task of this Agent, including its routing keys, references and recent conversation.", map[string]any{
			"name": map[string]string{"type": "string"}}, "name"),
		mcpTool("create_task", "Create a new Task for this Agent. Supply a canonical resource URI as routingKey to deduplicate repeated events.", map[string]any{
			"objective":  map[string]string{"type": "string"},
			"routingKey": map[string]any{"type": "string", "format": "uri"}}, "objective"),
		mcpTool("complete_task", "Complete a Task of this Agent and stop its worker while retaining history and workspace.", map[string]any{
			"name": map[string]string{"type": "string"}, "reason": map[string]string{"type": "string"}}, "name", "reason"),
	}
}

func (h *Handler) agentMCP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	scope, err := h.authenticateWorker(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		http.Error(w, "worker authentication failed", http.StatusUnauthorized)
		return
	}
	var request mcpRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request); err != nil || request.JSONRPC != "2.0" {
		http.Error(w, "invalid JSON-RPC request", http.StatusBadRequest)
		return
	}
	if len(request.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch request.Method {
	case "initialize":
		result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]string{"name": "agent-operator", "title": "Agent", "version": "1"},
			"instructions": "Reports this worker's Task title, progress and turn results, and lists, creates and completes Tasks of the same Agent. " +
				"Task, Agent and namespace come from the worker's token, not tool arguments."}
	case "ping":
		result = map[string]any{}
	case "tools/list":
		result = map[string]any{"tools": agentTools()}
	case "tools/call":
		var call struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(request.Params, &call) != nil {
			err = errors.New("invalid tool call")
			break
		}
		result, err = h.callAgentTool(r.Context(), scope, call.Name, call.Arguments)
	default:
		err = errors.New("unknown method")
	}
	response := map[string]any{"jsonrpc": "2.0", "id": request.ID}
	if err != nil {
		response["error"] = map[string]any{"code": -32602, "message": err.Error()}
	} else {
		response["result"] = result
	}
	_ = json.NewEncoder(w).Encode(response)
}

func mcpText(value any) (any, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return map[string]any{"content": []map[string]string{{"type": "text", "text": string(data)}}}, nil
}

func decodeToolArgs(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("trailing tool arguments")
	}
	return nil
}

func (h *Handler) scopedTask(ctx context.Context, scope *workerScope, name string) (*agentsv1alpha1.Task, error) {
	if name == "" {
		return nil, errors.New("Task name required")
	}
	var task agentsv1alpha1.Task
	if err := h.mcpReader().Get(ctx, client.ObjectKey{Namespace: scope.Task.Namespace, Name: name}, &task); err != nil {
		return nil, errors.New("Task unavailable")
	}
	if task.Spec.AgentRef != scope.Agent.Name {
		return nil, errors.New("Task outside Agent scope")
	}
	for _, owner := range task.OwnerReferences {
		if owner.Kind == "Agent" && owner.APIVersion == agentsv1alpha1.GroupVersion.String() && owner.UID == scope.Agent.UID {
			return &task, nil
		}
	}
	return nil, errors.New("Task outside Agent scope")
}

func (h *Handler) callAgentTool(ctx context.Context, scope *workerScope, name string, raw json.RawMessage) (any, error) {
	if scope.Task.Spec.Suspend && name != "report_turn_result" {
		return nil, errors.New("Task is suspended")
	}
	switch name {
	case "report_mcp_probe":
		var arg struct {
			RequestID string                             `json:"requestId"`
			Endpoints []agentsv1alpha1.MCPEndpointStatus `json:"endpoints"`
		}
		if decodeToolArgs(raw, &arg) != nil || len(arg.RequestID) != 32 || len(arg.Endpoints) == 0 || len(arg.Endpoints) > 32 {
			return nil, errors.New("invalid MCP probe report")
		}
		// The worker reports whatever it was configured with: Agent endpoints
		// plus operator-provided ones such as RBAC-attached Mailboxes. Accept any
		// bounded, well-formed result; the UI renders it as plain text.
		seen := make(map[string]bool)
		for _, endpoint := range arg.Endpoints {
			if !mcpNamePattern.MatchString(endpoint.Name) || seen[endpoint.Name] ||
				(endpoint.State != "available" && endpoint.State != "unavailable") || !validMCPProbeDetail(endpoint.Detail) || !validMCPProbeContext(endpoint) {
				return nil, errors.New("invalid MCP endpoint result")
			}
			seen[endpoint.Name] = true
		}
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var current agentsv1alpha1.Task
			if err := h.Client.Get(ctx, client.ObjectKeyFromObject(&scope.Task), &current); err != nil {
				return err
			}
			probe := current.Status.MCPProbe
			if current.Status.WorkerPodUID != string(scope.Pod.UID) || probe == nil || probe.RequestID != arg.RequestID || probe.PodUID != string(scope.Pod.UID) {
				return errors.New("MCP probe request is no longer current")
			}
			if probe.State == "complete" {
				return nil
			}
			now := metav1.Now()
			probe.State = "complete"
			probe.Endpoints = arg.Endpoints
			probe.UpdatedAt = now
			probe.CheckedAt = &now
			return h.Client.Status().Update(ctx, &current)
		})
		if err != nil {
			return nil, err
		}
		return mcpText(map[string]any{"recorded": true})
	case "set_task_title":
		var arg struct {
			Title string `json:"title"`
		}
		if err := decodeToolArgs(raw, &arg); err != nil {
			return nil, errors.New("invalid Task title")
		}
		title := strings.TrimSpace(arg.Title)
		if title == "" || len(title) > 120 || strings.ContainsAny(title, "\r\n") || strings.ContainsFunc(title, func(r rune) bool { return r < 32 || r == 127 }) {
			return nil, errors.New("Task title must be one line of up to 120 bytes")
		}
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var current agentsv1alpha1.Task
			if err := h.Client.Get(ctx, client.ObjectKeyFromObject(&scope.Task), &current); err != nil {
				return err
			}
			if current.UID != scope.Task.UID || current.Status.WorkerPodUID != string(scope.Pod.UID) || current.Spec.Suspend {
				return errors.New("worker is no longer current")
			}
			if current.Spec.Title == title {
				return nil
			}
			current.Spec.Title = title
			return h.Client.Update(ctx, &current)
		})
		if err != nil {
			return nil, err
		}
		return mcpText(map[string]any{"title": title})
	case "report_turn_start":
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var current agentsv1alpha1.Task
			if err := h.Client.Get(ctx, client.ObjectKeyFromObject(&scope.Task), &current); err != nil {
				return err
			}
			if current.Status.WorkerPodUID != string(scope.Pod.UID) || current.Spec.Suspend {
				return errors.New("worker is no longer current")
			}
			if current.Status.Activity == "working" {
				return nil
			}
			current.Status.Activity = "working"
			return h.Client.Status().Update(ctx, &current)
		})
		if err != nil {
			return nil, err
		}
		return mcpText(map[string]any{"working": true})
	case "report_progress":
		var arg struct{ ID, Content string }
		if decodeToolArgs(raw, &arg) != nil || arg.ID == "" || len(arg.ID) > 160 || strings.TrimSpace(arg.Content) == "" || len(arg.Content) > 8000 {
			return nil, errors.New("invalid progress note")
		}
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var current agentsv1alpha1.Task
			if err := h.Client.Get(ctx, client.ObjectKeyFromObject(&scope.Task), &current); err != nil {
				return err
			}
			if current.Status.WorkerPodUID != string(scope.Pod.UID) || current.Spec.Suspend {
				return errors.New("worker is no longer current")
			}
			now := metav1.Now()
			if !current.Status.AppendMessage(agentsv1alpha1.TaskMessage{ID: arg.ID, Role: "agent", Source: "worker",
				Content: arg.Content, CreatedAt: now}) {
				return nil
			}
			current.Status.Progress = &agentsv1alpha1.TaskProgress{Summary: arg.Content, UpdatedAt: now}
			return h.Client.Status().Update(ctx, &current)
		})
		if err != nil {
			return nil, err
		}
		return mcpText(map[string]any{"recorded": true})
	case "report_turn_progress":
		// Streamed text goes only to open browsers; the conversation stores the
		// turn's full text once its result arrives.
		var arg struct {
			ID, Content string
			Tools       []turnTool
		}
		if decodeToolArgs(raw, &arg) != nil || arg.ID == "" || len(arg.ID) > 160 || len(arg.Content) > 8000 ||
			strings.TrimSpace(arg.Content) == "" && len(arg.Tools) == 0 || !validTurnTools(arg.Tools) {
			return nil, errors.New("invalid turn progress")
		}
		if scope.Task.Status.Activity == "working" && h.Events != nil {
			if topic := taskAgentTopic(&scope.Task); topic != "" {
				h.Events.publishTurn(topic, turnText{Namespace: scope.Task.Namespace, Task: scope.Task.Name, Turn: arg.ID,
					Content: arg.Content, Tools: arg.Tools})
			}
		}
		return mcpText(map[string]any{"streamed": true})
	case "report_turn_result":
		var arg struct {
			ID, Summary, CompletedAt, Backend, Model string
			Metrics                                  *agentsv1alpha1.TurnMetrics
		}
		if err := decodeToolArgs(raw, &arg); err != nil || arg.ID == "" || arg.Summary == "" || len(arg.Summary) > 8000 || len(arg.ID) > 160 ||
			(arg.Backend != "" && arg.Backend != "claude" && arg.Backend != "codex") || len(arg.Model) > 200 || !validTurnMetrics(arg.Metrics) {
			return nil, errors.New("invalid turn result")
		}
		at, err := time.Parse(time.RFC3339Nano, arg.CompletedAt)
		if err != nil || at.After(time.Now().Add(5*time.Minute)) || at.Before(scope.Pod.CreationTimestamp.Time.Add(-time.Minute)) {
			return nil, errors.New("invalid completion time")
		}
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var current agentsv1alpha1.Task
			if err := h.Client.Get(ctx, client.ObjectKeyFromObject(&scope.Task), &current); err != nil {
				return err
			}
			if current.Status.WorkerPodUID != string(scope.Pod.UID) {
				return errors.New("worker is no longer current")
			}
			if !current.Status.AppendMessage(agentsv1alpha1.TaskMessage{ID: arg.ID, Role: "agent", Source: "worker", Backend: arg.Backend, Model: arg.Model, Metrics: arg.Metrics, Content: arg.Summary, CreatedAt: metav1.NewTime(at)}) {
				return nil
			}
			current.Status.Progress = &agentsv1alpha1.TaskProgress{Summary: arg.Summary, UpdatedAt: metav1.NewTime(at)}
			current.Status.Activity = "idle"
			stamp := metav1.NewTime(at)
			current.Status.LastResultAt = &stamp
			return h.Client.Status().Update(ctx, &current)
		})
		if err != nil {
			return nil, err
		}
		if err := h.finishRequestedCompletion(ctx, scope.Task.Namespace, scope.Task.Name); err != nil {
			return nil, err
		}
		return mcpText(map[string]any{"recorded": true})
	case "list_tasks":
		var list agentsv1alpha1.TaskList
		if err := h.mcpReader().List(ctx, &list, client.InNamespace(scope.Task.Namespace), client.MatchingLabels{"codemowers.cloud/agent": scope.Agent.Name}); err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, min(len(list.Items), 100))
		for i := range list.Items {
			if len(out) >= 100 {
				break
			}
			task, err := h.scopedTask(ctx, scope, list.Items[i].Name)
			if err != nil {
				continue
			}
			out = append(out, map[string]any{"name": task.Name, "title": task.Spec.Title, "objective": task.Spec.Objective, "phase": task.Status.Phase,
				"routingKeys": task.Spec.RoutingKeys, "externalReferences": taskExternalReferences(task)})
		}
		return mcpText(out)
	case "get_task":
		var arg struct {
			Name string `json:"name"`
		}
		if decodeToolArgs(raw, &arg) != nil {
			return nil, errors.New("invalid Task name")
		}
		task, err := h.scopedTask(ctx, scope, arg.Name)
		if err != nil {
			return nil, err
		}
		return mcpText(map[string]any{"name": task.Name, "title": task.Spec.Title, "objective": task.Spec.Objective, "phase": task.Status.Phase,
			"routingKeys": task.Spec.RoutingKeys, "externalReferences": taskExternalReferences(task),
			"conversation": task.Status.Conversation, "summary": task.Status.Progress})
	case "create_task":
		var arg struct {
			Objective  string `json:"objective"`
			RoutingKey string `json:"routingKey"`
		}
		if decodeToolArgs(raw, &arg) != nil || strings.TrimSpace(arg.Objective) == "" || len(arg.Objective) > 16000 {
			return nil, errors.New("objective required (up to 16000 bytes)")
		}
		name := "task-" + randHex(12)
		if arg.RoutingKey != "" {
			key, err := agentsv1alpha1.CanonicalKey(arg.RoutingKey)
			if err != nil {
				return nil, errors.New("routingKey must be a canonical resource URI")
			}
			arg.RoutingKey = key
			name = agentsv1alpha1.TaskNameForKey(scope.Agent.UID, key)
			var tasks agentsv1alpha1.TaskList
			if err := h.mcpReader().List(ctx, &tasks, client.InNamespace(scope.Task.Namespace), client.MatchingLabels{"codemowers.cloud/agent": scope.Agent.Name}); err != nil {
				return nil, err
			}
			for i := range tasks.Items {
				if slices.Contains(tasks.Items[i].Spec.RoutingKeys, arg.RoutingKey) {
					if existing, err := h.scopedTask(ctx, scope, tasks.Items[i].Name); err == nil {
						return mcpText(map[string]any{"name": existing.Name, "namespace": existing.Namespace, "existing": true})
					}
				}
			}
		}
		controller := true
		task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: scope.Task.Namespace,
			Labels: map[string]string{"codemowers.cloud/agent": scope.Agent.Name},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: agentsv1alpha1.GroupVersion.String(), Kind: "Agent",
				Name: scope.Agent.Name, UID: scope.Agent.UID, Controller: &controller, BlockOwnerDeletion: &controller}}},
			Spec: agentsv1alpha1.TaskSpec{AgentRef: scope.Agent.Name, Objective: arg.Objective,
				RoutingKeys: func() []string {
					if arg.RoutingKey == "" {
						return nil
					}
					return []string{arg.RoutingKey}
				}(),
				Suspend: scope.Agent.Spec.Suspended, IdleSeconds: int32Ptr(agentsv1alpha1.AgentIdleSeconds(&scope.Agent)),
				Resources: agentsv1alpha1.AgentWorkerResources(&scope.Agent)}}
		if err := h.Client.Create(ctx, task); apierrors.IsAlreadyExists(err) && arg.RoutingKey != "" {
			existing, lookupErr := h.scopedTask(ctx, scope, name)
			if lookupErr != nil || len(existing.Spec.RoutingKeys) != 1 || existing.Spec.RoutingKeys[0] != arg.RoutingKey {
				return nil, errors.New("routing key collision")
			}
			return mcpText(map[string]any{"name": existing.Name, "namespace": existing.Namespace, "existing": true})
		} else if err != nil {
			return nil, err
		}
		return mcpText(map[string]any{"name": task.Name, "namespace": task.Namespace, "existing": false})
	case "complete_task":
		var arg struct{ Name, Reason string }
		if decodeToolArgs(raw, &arg) != nil || strings.TrimSpace(arg.Reason) == "" || len(arg.Reason) > 2000 {
			return nil, errors.New("Task name and reason required")
		}
		if _, err := h.scopedTask(ctx, scope, arg.Name); err != nil {
			return nil, err
		}
		// The worker's own Task completes after its turn result is recorded;
		// another Task completes at once. Completion suspends the Task.
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var task agentsv1alpha1.Task
			if err := h.Client.Get(ctx, client.ObjectKey{Namespace: scope.Task.Namespace, Name: arg.Name}, &task); err != nil {
				return err
			}
			if task.Spec.AgentRef != scope.Agent.Name {
				return errors.New("Task outside Agent scope")
			}
			now := metav1.Now()
			completion := &agentsv1alpha1.TaskCompletion{Reason: arg.Reason, Actor: scope.Agent.Name}
			if task.Name == scope.Task.Name {
				completion.Requested = true
			} else {
				completion.CompletedAt = &now
			}
			if !task.Status.Completed() {
				task.Status.Completion = completion
			}
			task.Status.AppendMessage(agentsv1alpha1.TaskMessage{ID: "completion:" + randHex(12), Role: "system", Source: "agent-mcp",
				Actor: scope.Agent.Name, Content: "Agent requested completion: " + arg.Reason, CreatedAt: now})
			return h.Client.Status().Update(ctx, &task)
		}); err != nil {
			return nil, err
		}
		return mcpText(map[string]any{"completionRequested": true})
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}

func (h *Handler) finishRequestedCompletion(ctx context.Context, namespace, name string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var task agentsv1alpha1.Task
		if err := h.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &task); err != nil {
			return err
		}
		if task.Status.Completion == nil || !task.Status.Completion.Requested {
			return nil
		}
		now := metav1.Now()
		task.Status.Completion.Requested = false
		task.Status.Completion.CompletedAt = &now
		return h.Client.Status().Update(ctx, &task)
	})
}
