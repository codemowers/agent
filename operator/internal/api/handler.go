// Package api provides a minimal HTTP UI for creating Tasks and sending prompts.
package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	"github.com/codemowers/agent/operator/internal/rabbitmq"
)

// Publisher is the subset of rabbitmq.Manager used by the API handler.
type Publisher interface {
	DeclareQueue(name string) error
	Publish(ctx context.Context, queue string, msg rabbitmq.PromptMessage) error
}

// Handler serves the web UI and REST API for Task management.
type Handler struct {
	Client client.Client
	AMQP   Publisher // nil if RabbitMQ not configured
	Addr   string
}

func (h *Handler) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("api")
	mux := h.routes()
	srv := &http.Server{Addr: h.Addr, Handler: mux}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background()) //nolint:errcheck
	}()
	logger.Info("API server listening", "addr", h.Addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (h *Handler) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/agents", h.listAgents)
	mux.HandleFunc("GET /api/tasks", h.listTasks)
	mux.HandleFunc("POST /api/tasks", h.createTask)
	mux.HandleFunc("POST /api/tasks/{namespace}/{name}/prompts", h.sendPrompt)
	mux.HandleFunc("DELETE /api/tasks/{namespace}/{name}", h.deleteTask)
	mux.HandleFunc("/", h.serveUI)
	return mux
}

// --- API types ---

type agentItem struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Backend   string `json:"backend"`
}

type taskItem struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	AgentName string `json:"agentName"`
	Phase     string `json:"phase"`
	Reason    string `json:"reason"`
	Objective string `json:"objective"`
	WorkerPod string `json:"workerPod,omitempty"`
}

type createTaskReq struct {
	AgentName      string `json:"agentName"`
	AgentNamespace string `json:"agentNamespace"`
	Objective      string `json:"objective"`
}

type sendPromptReq struct {
	Body string `json:"body"`
}

// --- Handlers ---

func (h *Handler) listAgents(w http.ResponseWriter, r *http.Request) {
	var list agentsv1alpha1.AgentList
	if err := h.Client.List(r.Context(), &list); err != nil {
		jsonError(w, err, 500)
		return
	}
	out := make([]agentItem, 0, len(list.Items))
	for _, a := range list.Items {
		out = append(out, agentItem{
			Name:      a.Name,
			Namespace: a.Namespace,
			Backend:   a.Spec.Model.Backend,
		})
	}
	jsonOK(w, out)
}

func (h *Handler) listTasks(w http.ResponseWriter, r *http.Request) {
	var list agentsv1alpha1.TaskList
	if err := h.Client.List(r.Context(), &list); err != nil {
		jsonError(w, err, 500)
		return
	}
	out := make([]taskItem, 0, len(list.Items))
	for _, t := range list.Items {
		agentName := ""
		for _, ref := range t.OwnerReferences {
			if ref.Kind == "Agent" {
				agentName = ref.Name
				break
			}
		}
		out = append(out, taskItem{
			Name:      t.Name,
			Namespace: t.Namespace,
			AgentName: agentName,
			Phase:     string(t.Status.Phase),
			Reason:    t.Status.Reason,
			Objective: t.Spec.Objective,
			WorkerPod: t.Status.WorkerPodName,
		})
	}
	jsonOK(w, out)
}

func (h *Handler) createTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, err, 400)
		return
	}
	if req.Objective == "" || req.AgentName == "" || req.AgentNamespace == "" {
		jsonError(w, fmt.Errorf("agentName, agentNamespace and objective are required"), 400)
		return
	}

	var agent agentsv1alpha1.Agent
	if err := h.Client.Get(r.Context(), client.ObjectKey{Namespace: req.AgentNamespace, Name: req.AgentName}, &agent); err != nil {
		jsonError(w, err, 404)
		return
	}

	taskName := fmt.Sprintf("%s-%s", slugify(req.Objective), randHex(4))
	task := &agentsv1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskName,
			Namespace: agent.Namespace,
			Labels: map[string]string{
				"codemowers.cloud/agent": agent.Name,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         agentsv1alpha1.GroupVersion.String(),
				Kind:               "Agent",
				Name:               agent.Name,
				UID:                agent.UID,
				BlockOwnerDeletion: boolPtr(true),
				Controller:         boolPtr(true),
			}},
		},
		Spec: agentsv1alpha1.TaskSpec{
			Objective: req.Objective,
		},
	}
	if err := h.Client.Create(r.Context(), task); err != nil {
		jsonError(w, err, 500)
		return
	}
	w.WriteHeader(http.StatusCreated)
	jsonOK(w, map[string]string{"name": task.Name, "namespace": task.Namespace})
}

func (h *Handler) sendPrompt(w http.ResponseWriter, r *http.Request) {
	if h.AMQP == nil {
		jsonError(w, fmt.Errorf("RabbitMQ not configured"), 503)
		return
	}
	var req sendPromptReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, err, 400)
		return
	}
	if req.Body == "" {
		jsonError(w, fmt.Errorf("body is required"), 400)
		return
	}
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	queue := ns + "/" + name

	if err := h.AMQP.DeclareQueue(queue); err != nil {
		jsonError(w, fmt.Errorf("declare queue: %w", err), 500)
		return
	}
	msg := rabbitmq.PromptMessage{Type: "prompt", Body: req.Body}
	if err := h.AMQP.Publish(r.Context(), queue, msg); err != nil {
		jsonError(w, fmt.Errorf("publish: %w", err), 500)
		return
	}
	// Annotate task so the TaskReconciler ensures a worker pod is running.
	var task agentsv1alpha1.Task
	if err := h.Client.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &task); err == nil {
		patch := client.MergeFrom(task.DeepCopy())
		if task.Annotations == nil {
			task.Annotations = make(map[string]string)
		}
		task.Annotations["codemowers.cloud/prompt-sent-at"] = fmt.Sprintf("%d", metav1.Now().UnixMilli())
		h.Client.Patch(r.Context(), &task, patch) //nolint:errcheck
	}
	jsonOK(w, map[string]string{"status": "published"})
}

func (h *Handler) deleteTask(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	var task agentsv1alpha1.Task
	if err := h.Client.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &task); err != nil {
		jsonError(w, err, 404)
		return
	}
	if err := h.Client.Delete(r.Context(), &task); err != nil {
		jsonError(w, err, 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) serveUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, uiHTML)
}

// --- Helpers ---

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.ToLower(s)
	s = nonAlnum.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	words := strings.SplitN(s, "-", 6)
	if len(words) > 5 {
		words = words[:5]
	}
	slug := strings.Join(words, "-")
	if len(slug) > 48 {
		slug = slug[:48]
	}
	return strings.Trim(slug, "-")
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b) //nolint:errcheck
	return fmt.Sprintf("%x", b)
}

func boolPtr(b bool) *bool { return &b }

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func jsonError(w http.ResponseWriter, err error, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()}) //nolint:errcheck
}

const uiHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Agent Tasks</title>
<style>
* { box-sizing: border-box; margin: 0; padding: 0; }
body { font: 14px/1.5 system-ui, sans-serif; background: #0d1117; color: #c9d1d9; display: flex; flex-direction: column; height: 100vh; }
header { background: #161b22; border-bottom: 1px solid #30363d; padding: 12px 20px; font-size: 15px; font-weight: 600; color: #58a6ff; }
main { display: grid; grid-template-columns: 360px 1fr; flex: 1; overflow: hidden; }
aside { border-right: 1px solid #30363d; overflow-y: auto; display: flex; flex-direction: column; }
section { flex: 1; overflow-y: auto; padding: 20px; }

/* New task form */
.new-task { padding: 16px; border-bottom: 1px solid #30363d; background: #161b22; }
.new-task h3 { font-size: 12px; text-transform: uppercase; letter-spacing: .05em; color: #8b949e; margin-bottom: 10px; }
select, textarea, input[type=text] { width: 100%; background: #0d1117; border: 1px solid #30363d; border-radius: 6px; color: #c9d1d9; padding: 7px 10px; font: inherit; }
select { margin-bottom: 8px; }
textarea { resize: vertical; min-height: 80px; margin-bottom: 8px; }
button { border: none; border-radius: 6px; padding: 7px 14px; font: inherit; cursor: pointer; }
.btn-primary { background: #238636; color: #fff; }
.btn-primary:hover { background: #2ea043; }
.btn-danger { background: transparent; border: 1px solid #f85149; color: #f85149; padding: 4px 10px; font-size: 12px; }
.btn-danger:hover { background: #f85149; color: #fff; }
.btn-send { background: #1f6feb; color: #fff; }
.btn-send:hover { background: #388bfd; }

/* Task list */
.task-list { padding: 8px; }
.task-item { border-radius: 6px; padding: 10px 12px; cursor: pointer; border: 1px solid transparent; margin-bottom: 4px; }
.task-item:hover { background: #161b22; border-color: #30363d; }
.task-item.active { background: #161b22; border-color: #1f6feb; }
.task-name { font-weight: 600; font-size: 13px; }
.task-meta { font-size: 11px; color: #8b949e; margin-top: 2px; }
.dot { display: inline-block; width: 8px; height: 8px; border-radius: 50%; margin-right: 6px; }
.dot-running { background: #3fb950; }
.dot-pending { background: #e3b341; }
.dot-waiting { background: #58a6ff; }
.dot-paused  { background: #8b949e; }
.dot-failed  { background: #f85149; }
.dot-completed { background: #238636; }
.dot- { background: #30363d; }

/* Task detail */
.detail-header { display: flex; justify-content: space-between; align-items: flex-start; margin-bottom: 20px; }
.detail-header h2 { font-size: 16px; word-break: break-all; }
.detail-section { margin-bottom: 20px; }
.detail-section label { font-size: 11px; text-transform: uppercase; letter-spacing: .05em; color: #8b949e; display: block; margin-bottom: 6px; }
.detail-section pre { background: #161b22; border: 1px solid #30363d; border-radius: 6px; padding: 12px; font-size: 13px; white-space: pre-wrap; word-break: break-word; }
.prompt-row { display: flex; gap: 8px; }
.prompt-row input { flex: 1; }
.empty { color: #8b949e; font-style: italic; margin-top: 40px; text-align: center; }
</style>
</head>
<body>
<header>Agent Tasks</header>
<main>
  <aside>
    <div class="new-task">
      <h3>New Task</h3>
      <select id="agentSelect"><option value="">Loading agents…</option></select>
      <textarea id="objective" placeholder="Objective…"></textarea>
      <button class="btn-primary" onclick="createTask()">Create Task</button>
    </div>
    <div class="task-list" id="taskList"></div>
  </aside>
  <section id="detail"><div class="empty">Select a task or create one.</div></section>
</main>
<script>
let agents = [], tasks = [], selectedTask = null;

async function load() {
  [agents, tasks] = await Promise.all([
    fetch('/api/agents').then(r => r.json()),
    fetch('/api/tasks').then(r => r.json()),
  ]);
  renderAgentSelect();
  renderTaskList();
  if (selectedTask) {
    const t = tasks.find(t => t.namespace === selectedTask.namespace && t.name === selectedTask.name);
    if (t) renderDetail(t);
  }
}

function renderAgentSelect() {
  const sel = document.getElementById('agentSelect');
  sel.innerHTML = agents.map(a =>
    '<option value="'+a.name+'|'+a.namespace+'">'+a.namespace+'/'+a.name+' ('+a.backend+')</option>'
  ).join('') || '<option value="">No agents</option>';
}

function renderTaskList() {
  const el = document.getElementById('taskList');
  if (!tasks.length) { el.innerHTML = '<div style="padding:12px;color:#8b949e;font-size:12px">No tasks yet</div>'; return; }
  el.innerHTML = tasks.map(t => {
    const active = selectedTask && selectedTask.name === t.name && selectedTask.namespace === t.namespace ? ' active' : '';
    const phase = (t.phase||'').toLowerCase();
    return '<div class="task-item'+active+'" onclick="select(\''+t.namespace+'\',\''+t.name+'\')">'+
      '<div class="task-name"><span class="dot dot-'+phase+'"></span>'+t.name+'</div>'+
      '<div class="task-meta">'+t.namespace+' · '+(t.agentName||'?')+' · '+(t.phase||'—')+'</div>'+
      '</div>';
  }).join('');
}

function select(ns, name) {
  selectedTask = {namespace: ns, name: name};
  const t = tasks.find(t => t.namespace === ns && t.name === name);
  if (t) renderDetail(t);
  renderTaskList();
}

function renderDetail(t) {
  document.getElementById('detail').innerHTML =
    '<div class="detail-header">'+
      '<h2>'+t.name+'</h2>'+
      '<button class="btn-danger" onclick="deleteTask(\''+t.namespace+'\',\''+t.name+'\')">Delete</button>'+
    '</div>'+
    '<div class="detail-section"><label>Namespace / Agent</label><pre>'+t.namespace+' / '+(t.agentName||'—')+'</pre></div>'+
    '<div class="detail-section"><label>Phase</label><pre>'+(t.phase||'—')+' '+(t.reason ? '('+t.reason+')' : '')+'</pre></div>'+
    (t.workerPod ? '<div class="detail-section"><label>Worker Pod</label><pre>'+t.workerPod+'</pre></div>' : '')+
    '<div class="detail-section"><label>Objective</label><pre>'+esc(t.objective)+'</pre></div>'+
    '<div class="detail-section"><label>Send Prompt</label>'+
      '<div class="prompt-row">'+
        '<input type="text" id="promptInput" placeholder="Follow-up prompt…" onkeydown="if(event.key===\'Enter\')sendPrompt(\''+t.namespace+'\',\''+t.name+'\')">'+
        '<button class="btn-send" onclick="sendPrompt(\''+t.namespace+'\',\''+t.name+'\')">Send</button>'+
      '</div>'+
    '</div>';
}

async function createTask() {
  const sel = document.getElementById('agentSelect').value;
  const obj = document.getElementById('objective').value.trim();
  if (!sel || !obj) return alert('Select an agent and enter an objective.');
  const [agentName, agentNamespace] = sel.split('|');
  const res = await fetch('/api/tasks', {method:'POST', headers:{'Content-Type':'application/json'},
    body: JSON.stringify({agentName, agentNamespace, objective: obj})});
  if (!res.ok) { const e = await res.json(); return alert(e.error); }
  document.getElementById('objective').value = '';
  await load();
  const t = await res.json();
  select(t.namespace, t.name);
}

async function sendPrompt(ns, name) {
  const body = document.getElementById('promptInput').value.trim();
  if (!body) return;
  const res = await fetch('/api/tasks/'+ns+'/'+name+'/prompts', {method:'POST',
    headers:{'Content-Type':'application/json'}, body: JSON.stringify({body})});
  if (!res.ok) { const e = await res.json(); return alert(e.error); }
  document.getElementById('promptInput').value = '';
}

async function deleteTask(ns, name) {
  if (!confirm('Delete task '+name+'?')) return;
  await fetch('/api/tasks/'+ns+'/'+name, {method:'DELETE'});
  selectedTask = null;
  document.getElementById('detail').innerHTML = '<div class="empty">Task deleted.</div>';
  await load();
}

function esc(s) { return (s||'').replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;'); }

load();
setInterval(load, 5000);
</script>
</body>
</html>`
