# codemowers/agent: GitOps agent harness

This file is the consolidated implementation contract and repository guidance.
Keep it updated when behavior or architecture changes. README.md covers user
setup and operation.

The project goal is a GitOps-centric, Kubernetes-native agent harness that is
reasonably safe to use out of the box through confinement, scoped credentials,
authenticated inputs and human review. Kubernetes is the execution/deployment
target; Docker/Compose runtime deployments are outside scope. Skaffold builds
this repository's harness images in cluster Kaniko Pods. Workers build
application images in separate Kaniko Pods through Skaffold.

In the segmentation example in `docs/security-model.md`, customer support has the greatest external
prompt-injection exposure, developer has the greatest supply-chain and
privilege-escalation exposure, and kube admin has the greatest sensitive-data
disclosure risk due to cluster-wide access. Developer builds with Kaniko and
deploys development workloads only in namespaces its RBAC grants, alongside cluster-wide
operational reads. Platform admission must enforce user namespace isolation
(`hostUsers: false`) or a supported Kata runtime for those Pods and reject host
access/confinement bypasses. Kata and local models do not remove API privileges
or prevent abuse of credentials and data available to an agent.

## Working in this repository

- Backward compatibility is not a goal at this stage. Make breaking changes
  directly; do not add migration paths or existing-installation instructions.
- Use single quotes for JavaScript strings and template literals for interpolation.
  Quotes embedded in JSON, selectors and CLI configuration are data.
- Build this harness with Skaffold's cluster Kaniko builder and the configured
  Zot repository. Worker application builds use the separate sandbox workflow below.
- Keep tooling and dependency Docker stages independent of source/frontend stages.
  Pin dependencies and coding CLIs; production images receive pruned dependencies.
  Exclude credentials, local configuration, generated output and editor files from
  Git and image build context.
- Keep CSS organized by component, shared defaults and one block per responsive
  breakpoint. Remove superseded rules and unused selectors. Preserve desktop,
  mobile drawer, composer, dialog and read-only layouts.
- Bump the Go `WorkerPodVersion` with any change to the runner image, worker
  code or operator/worker contract, so active worker Pods are replaced on the
  next operator rollout instead of running the old image until idle shutdown.
- Run focused verification appropriate to the change. Do not add redundant tests
  or repeat broad checks after relevant checks pass without a new concern.
- Product drafts describe requirements, application design and acceptance criteria.
  Platform recipes and cluster findings belong in live MCP guidance and shared
  worker instructions. Preserve product-specific ownership, storage and recovery
  requirements when trimming drafts. Every product draft declares separate Test,
  Build and Deploy controls in agentControls front matter, with explicit off-state
  behavior; do not combine image builds and deployments into one control.
- Do not reintroduce obsolete namespace, secret, provider or UI compatibility
  paths. Preserve existing PVCs and their contents during cleanup.

### Infrastructure discovery and diagnostics

Use Codemowers MCP for global engineering guidance and the target cluster's
Driftmower MCP for capabilities, inventory, admission, schemas, access, logs and
metrics. Use the configured Codemowers advisory connection and the Driftmower
connection whose `get_cluster_identity` matches the configured target cluster. Deployment-specific
MCP URLs belong in the separately provisioned ConfigMap, not repository configuration.

The generic Driftmower alias can point to a different cluster. If native tools
are absent, use the authenticated MCP transport with its scoped ServiceAccount
flow. Read `structuredContent` and warnings; workload text can be only a summary.
Use scoped inventory and exact Pod/container names for bounded log queries.
Reuse relevant discovery within a turn rather than repeatedly scanning everything.

Before infrastructure recommendations or actions, including container smoke-test
advice, read relevant Codemowers advisory sections and query Driftmower cluster
identity, provisioning capabilities and namespace readiness. For platform manifests,
read applicable admission policies/bindings, CRD schemas and exact operation access.
Schema discovery does not establish write permission. Short-hostname conventions
must come from policy discovery, not guessed domain suffixes or existing Ingresses.

Use Driftmower `query_logs` before `kubectl logs`. Empty Loki results do not prove
success or absence of Kubernetes logs. Check scope/time/source; use a bounded
kubectl fallback only for a missing/unavailable capability or an evidenced missing
result, explaining the concrete reason. Apply manifests and wait for rollouts
through kubectl/Skaffold when MCP has no matching operation. Never exec into Pods
for diagnostics. Do not expand RBAC to silence inventory warnings.

## Current architecture and deployment

The Go operator runs the OIDC SPA, Task API, execution-bound Agent MCP,
RabbitMQ routing and worker orchestration. A Go webhook service authenticates
receivers, and a Node.js Mailbox Pod owns IMAP/SMTP. Claude and Codex run in
separate coding worker Pods; the operator does not run a coding CLI or delegate
routing authority to an LLM.

| Component | Responsibility |
| --- | --- |
| `agent-operator` | Go Task API, reconciliation, status, routing and Agent MCP |
| `agent-operator-webhook` | Signed and restricted internal event intake |
| `agent-mailbox` | Namespaced IMAP/SMTP Pod with protocol-scoped Secret projections and its own MCP |
| `agent-feeds` | Optional public feed crawler that wakes bound Tasks through the operator |
| `codemowers/agent` | Ubuntu coding runner, native CLIs, workspace/session adapter and project tools |
| Task PVC | Git checkout, edits, artifacts, caches, progress notes and native CLI session files |
| Existing platform | Namespace ordering, tenancy, quotas, isolation, workload admission, storage and registry |

The operator has namespaced Agent and Task CRDs, RabbitMQ event
routing, and worker Pods derived from `Agent.spec.template` as a Kubernetes
`PodTemplateSpec` plus StatefulSet-shaped `volumeClaimTemplates`.
Namespace administrators, who are humans, define and update Agent specs. No
automation receives RBAC to create or mutate Agents. The operator reads Agents
and updates only `Agent.status`. Namespace administrators may also declare
Tasks with immutable `Task.spec.agentRef` naming a same-namespace Agent; the
operator adds their Agent ownerReference and routing label before creating a
worker or PVC. The operator creates event/UI Tasks and owns derived Task metadata
and status. Workers cannot write Agent or Task Kubernetes resources. Workers
report execution results through the configured transport, and the operator
records validated observations in `Task.status`. A namespace administrator's
ability to edit Agent references and therefore select credential keys is part
of this trust boundary. Worker Pods receive only the Secret keys that the
operator references in their Pod spec, never a whole provider Secret mount.
The operator projects every Git provider key named by the Agent into the worker
environment. One provider of a type receives `GH_TOKEN` or `GITEA_TOKEN`;
multiple providers of that type receive `GH_TOKEN_<HOST>` or
`GITEA_TOKEN_<HOST>`, with uppercase ASCII and punctuation replaced by
underscores. Ports and collision hashes disambiguate host names. A Task with
a matching external repository reference also receives its selected key as
`AGENT_FORGE_TOKEN` for checkout. Unbound Tasks still receive their Agent's
configured provider credentials.
Provider tokens live under separate keys in the namespace's
`agent-operator-provider-tokens` Secret. Model and Git provider
`credentialKeyRef` fields select a key. The operator has get-only RBAC for
the provider Secret. Neither the operator nor the webhook receiver has Secret
list/watch.
Webhook endpoints are receivers. A namespaced WebhookReceiver serves
`/hooks/<namespace>/<name>` to WebhookBindings of its namespace; a
cluster-scoped ClusterWebhookReceiver serves `/hooks/<name>` and fans each
event out to bindings of any namespace, so one GitHub or Gitea hook per org
can feed hundreds of Agents. A receiver names its WebhookProtocol, `match`
values for its CEL (such as `serverURL`, which separates github.com from GHE)
and its senders: an HMAC `secretKeyRef` (in the receiver's namespace, or a
named namespace for a cluster receiver), `serviceAccounts` with an `audience`
for in-cluster senders such as Alertmanager, or an `ed25519PublicKey`. One
must pass. `cidrs` and `githubMeta` (GitHub's hooks address ranges, cached
hourly and kept up to a day when GitHub is unreachable) additionally restrict
client addresses, taken from Traefik's `X-Forwarded-For` only when the peer is
in `webhook.trustedProxies`. Bindings hold no key: they name the receiver, the
Agent, routing and `keyPrefixes`, required for cluster receivers, which select
a hierarchy of events by canonical key such as `https://github.com/codemowers`
or `https://github.com/codemowers/agent`. Events without an issue carry the
repository URL as their key. The webhook service needs Secret `get` because
receivers name their Secret; cluster admission decides who may reference a
ClusterWebhookReceiver. TokenReview results are
cached by token hash for up to five minutes, never past token expiry, after
screening the unverified audience and expiry. Protocols, receivers, bindings
and signing keys are reused for ten seconds and, while the API server is unavailable, for
up to ten minutes; an object the API server reports missing is dropped at
once. An unavailable API server otherwise yields 503, which senders retry.
The receiver has no unsigned
or internal listener; in-cluster DNS and network position are not
authentication. Keep sender namespace, Pod selectors and Alertmanager routing
out of Helm values; the sender-side egress policy is an example manifest. Sender verification is separate from the
RabbitMQ publisher identity described below.
Agent and Task CRDs use `codemowers.ai/v1alpha1` directly, with resource names
`agents.codemowers.ai` and `tasks.codemowers.ai`.
This checkout deploys the Go operator as `agent-operator` and the webhook
receiver as `agent-operator-webhook` in the existing
`codemowers-agent-system` namespace. An Agent and its workers may live in the
harness namespace or another namespace. The operator learns the harness
namespace from its Pod's `metadata.namespace` fieldRef and points workers at the
harness `agent_operator` and `devices` Services; Mailboxes run in the Agent's
namespace. Harness NetworkPolicies admit worker Pods from any namespace, and
Agent namespaces get no NetworkPolicies for now. Where a worker may deploy is
RBAC alone: it can hold grants in several workload namespaces and discovers
them through the Driftmower MCP. Here all Agents share the `agents`
namespace and `matmlazi` deploys into `demo`. Placing workers in a namespace other
than their Agent's, so that a hosted tenant's worker memory counts against a
quota outside its sandbox, is not implemented yet. Their hosts are `dashboard` and `webhook`. The chart owns
the `agent-events` RabbitmqCluster (`rabbitmq.name`) and its intake topology;
the intake Queue uses `deletionPolicy: retain` so queued events survive its
removal. The operator retries an event in place, waiting out API server and
broker outages; after six other failures it rejects the event, which a Policy
dead-letters to the retained `agent.intake.dead` queue so the shared intake
queue keeps flowing. Controllers and the event consumer run only on the holder
of the `agent-operator.codemowers.ai` Lease; the API and Agent MCP serve on
every replica, though streamed turn text reaches only browsers connected to the
replica the worker calls. A Task records its queue in `Task.status.queue`
under the `codemowers.ai/task-queue` finalizer, which deletes the queue
with the Task even after its Agent is gone.
The Device bridge is a separate Go Deployment and image named `agent-devices`.
The chart enables the webhook receiver by default and the Device bridge only
with `deviceBridge.enabled=true`; `webhook.enabled=false` omits the receiver.
A disabled component has no workloads or related routing. When the bridge is
disabled, workers receive neither its MCP endpoint nor its projected token.
The existing operator Ingress routes `/devices/` to its Service, while workers
call its `/mcp` endpoint in the harness namespace. A namespaced Device CR
references one key in a Secret in its namespace. The bridge reads Devices and
only Secrets with names allowed by `deviceBridge.secretNames` in chart RBAC.
The example Hifiberry uses its own `agent-device-hifiberry` Secret. A worker sees each
Device on which it holds a verb, named bare in its own namespace and as
`namespace/name` elsewhere. The bridge owns device-agent
WebSockets and updates Device connection status. With the bridge enabled, the
chart advertises it as the `devices` ClusterMCPServer that every
ServiceAccount may use, so workers get a projected token for its Service's
audience, never a Device key; Device RBAC alone decides access.
Pat's separate unprivileged ServiceAccount has no RBAC and its default API
token mount is disabled, so its device calls are denied.
The bridge authenticates TokenReview and checks SubjectAccessReview on the exact
Device before each command, for the custom verb `exec` (`device_shell`), `read`
(`device_file_read`) or `write` (`device_file_write`); several worker
ServiceAccounts may hold those grants. Standard verbs such as `get` grant
nothing, so broad read-only roles never reach a Device. `write` can usually be
turned into code execution by overwriting scripts or service files, so grant it
only where `exec` would also be acceptable. Device deletion or key rotation closes the connection.
The operator sets the `worker` container image from the `agent-worker-image`
ConfigMap, overriding stale `image` values in Agent CRs. Signed forge events create
Tasks only after checking the configured account and current assignment.
Operator worker Pods are named `<prefix><Task prefix>-<incarnation ID>` and
Task PVCs `<prefix><volume>-<Task UID prefix>`, where the prefix is
`Agent.spec.resourcePrefix` or, when unset, `<Agent name>-`. The prefix is shortened as needed
to fit Kubernetes Pod name limits. Existing PVCs keep the names recorded in
`Task.status.workspacePVCs`.
`Agent.spec.idleSeconds` (default 3600, range 0–86400) is copied into
`Task.spec.idleSeconds` on creation, including directly created Tasks. Task
idle changes are sent to the resident worker over RabbitMQ. Worker container
requests and limits from `Agent.spec.resources` are also copied into new Tasks
as `Task.spec.resources`; Task edits replace the Pod, while Agent edits affect
new Tasks. A same-backend Task model ID change is sent over RabbitMQ and starts
a new native session epoch; backend or credential changes replace the Pod.
An optional `Agent.spec.instructions` describes the Agent's role, such as
cluster administrator, and leads every worker turn's prompt; changes reach the
next worker Pod. Task titles are short display names in `Task.spec.title`. The initial
worker prompt asks the worker to summarize the objective and set its own title through the
execution-bound Agent MCP; only the operator writes the Task spec. The Task
metadata name remains the stable ID, and the UI updates on Task watch events.
The Go `WorkerPodVersion` is stamped on each worker Pod and included
in its configuration hash. Bumping it replaces all active worker Pods as the
Task informer reconciles existing Tasks on operator startup. The replacement
inherits the last result time for its idle deadline. Pod start, stop and
terminal exit entries include actual Pod names in
bounded Task conversation history. A clean exit reads "exited" and adds no
line after an operator stop; a failed exit gives its reason, such as
OOMKilled, and exit code, and failed Pods remain for diagnosis. The worker starts the idle clock
after a successful CLI turn, continues reading its RabbitMQ queue until that
deadline, and exits when the queue stays empty. Zero means immediate exit.
The operator Task API exposes a bounded chat display from
`Task.status.conversation` and the latest worker response from
`Task.status.progress.summary`. The UI retrieves conversation through an
authorized Task detail endpoint and updates it on Kubernetes watch events.
The worker reports turn start and result through the operator's Agent MCP on
internal port 8083. The operator verifies a rotating audience-bound Pod token,
live Pod UID and current Task/Agent ownership. Result writes update only the
owning Task's status, including the backend, configured model ID, turn duration
and available input, output and cache token counts on its conversation bubble;
there is no live worker log polling. During a turn the worker sends the main
agent's text so far, bounded to its last 8000 bytes, through `report_turn_progress`;
the operator forwards it only to authorized open event streams as a live agent
bubble and writes no status. The same call carries the main agent's last 30
MCP calls: server, tool, running/done/failed and arguments as compact JSON of
at most 500 characters with secret-looking keys masked. The live bubble lists
them while the turn runs; call results are never streamed or stored. The turn result stores that full text. Claude turns run with background tasks disabled because
a headless turn's exit would kill background subagents and lose their results. A terminal Pod's
termination message remains a fallback summary. The Task view links structured
GitHub and Gitea references. Prometheus Task gauges are rebuilt from Task
status at scrape time with namespace, phase and activity labels. Each replica
also records `agent_operator_` histograms of the requests it serves: MCP check
round trips and Agent MCP tool calls. The chart's PodMonitor scrapes them, and
the operator NetworkPolicy admits Prometheus from
`operator.prometheusNamespace`. Workers serve `/metrics` and `/readyz` on port
9464: `agent_worker_` token counters and turn durations by backend, model and
outcome, MCP request durations through the token bridge by server, and idle
seconds computed at scrape time. A PodMonitor in each Agent namespace takes
`agent` and `task` from the operator's Pod labels, so a worker cannot claim
another Task's series. `/readyz` succeeds once the workspace and broker are
ready; the readiness probe then moves a `starting` Task to `idle` until the
worker reports a turn.
`Task.spec.routingKeys` holds exact canonical resource URIs, scoped by the
Task's namespace and Agent; webhook and mail events share them. Forge issues
and pull requests use `https://<forge>/<owner>/<repo>/issues/<number>` with
lowercase owner and repository; Slack threads use
`https://slack.com/archives/<channel>/p<root ts>`; Alertmanager uses its
external URL; mail threads use `mid:<root Message-ID>`. Canonicalization
lowercases scheme and host and drops a trailing slash; queries, fragments and
user info are rejected. Each WebhookProtocol lists `allowedKeyPrefixes`,
matched at a path boundary, that bound its CEL and binding `keyCEL` keys, so an
in-cluster sender cannot claim a forge key. A Task may claim several
keys. Duplicate exact claims within one Agent route to the fallback Task. `Task.spec.externalReferences` supplies starting source links
and forge checkout context; `Task.status.externalReferences` records links
collected later. Neither decides
event routing. The webhook
service reads receivers, protocols and bindings, verifies each sender,
evaluates bounded CEL once, and publishes one normalized key-bearing event per
matching binding
events to the `agent.intake` exchange with routing key
`webhook.<namespace>.<binding>` and the key in header `x-key`; the Mailbox
publishes `mail.<namespace>.<mailbox>`. CEL extracts the actor and an event
type such as `issues.assigned`. The operator accepts only events
whose broker-verified `user_id` is the webhook receiver's ServiceAccount and
ignores events whose actor is the Agent's own probed forge login on the key's
host when that snapshot reflects the current Agent generation. It then
resolves exact keys. It reads TaskPolicy only when no Task claims a key: the
policy of the Agent with the longest matching `keyPrefix` and allowed
`eventTypes` applies, and equally specific policies create nothing.
`verify: forgeAssignment` checks with the Agent's credential that the issue or
pull request is open and assigned to the Agent's account, reusing the current
forge snapshot's login; `verify: none` creates directly but never for mail.
Unknown keys go to the binding's fallback Task. The worker's prompt and the
conversation bubble for an event name its source, type, actor and link before
its text; for a forge issue or pull request they tell the worker to read it
there and post replies there, since its participants do not see the chat. The
event that created a Task through a TaskPolicy is its objective and does not
also wake it. Coalescing is upstream of
both transports and per routing key: the first event on a key wakes the Task
at once and opens a window. Within it, an event from another binding, such as
forge mail after its webhook, notifies of the same activity and is only
recorded; events from the same binding get one trailing wake, which opens the
next window, so a busy key wakes its Task at most once per window. The window
is `Task.spec.coalesceSeconds`, copied from the creating TaskPolicy, else
`Agent.spec.coalesceSeconds`, else 180 seconds; zero wakes on every event and
a day suits a chatty issue reviewed daily. The operator keeps intake
bookkeeping (delivery receipts, pending wakes and open windows) in
`Task.status.events`, and completion in `Task.status.completion`, never in
Task annotations. Workers hold no
broker password: they log in to RabbitMQ with a projected ServiceAccount token
for audience `agent-rabbitmq` and refresh it with AMQP `update-secret`. The
broker's OAuth2 backend maps that audience to read and configure on queues
named `<token sub>:*`, so Task queues are
`system:serviceaccount:<namespace>:<worker ServiceAccount>:<Task>`. The
separator is `:` because scope patterns split on `/` and Kubernetes names
cannot contain `:`. Workers cannot publish. The webhook receiver
and Mailbox IMAP containers log in with tokens for audience
`agent-rabbitmq-publisher`, which may only write to the intake exchange, and
stamp their ServiceAccount as AMQP `user_id`; RabbitMQ rejects a `user_id`
that differs from the token subject. That verified identity replaces a shared
event HMAC key. Only the operator uses the default broker user. The broker
fetches the ServiceAccount issuer's JWKS from `rabbitmq.jwksURI`: the API
server on k3s, which must allow anonymous discovery, or the public issuer on
EKS.
In-cluster callers wake a Task directly through the operator's wake endpoint,
`POST /internal/v1/namespaces/<namespace>/tasks/<task>/wake` on the
`agent-operator-wake` Service, which every replica serves and the Ingress
never routes. The caller presents a projected ServiceAccount token for
audience `agent-operator-wake.<harness namespace>`;
the operator checks it with TokenReview and requires a SubjectAccessReview for
the custom verb `wake` on that Task, which nobody holds by default. The JSON
body carries optional `text` (4 KiB, the conversation line and prompt) and
`url`. An `Idempotency-Key` header records repeats of the same caller and key
once. The arrival is attributed to the caller's ServiceAccount with source
`wake`, coalesces per caller like a binding, and is only recorded on a
completed Task. A CronJob calling it gives a Task a schedule.
Namespaced `Feed` resources name a public `https` RSS, Atom or JSON Feed and an
interval (default one hour). The optional `agent-feeds` crawler
(`feeds.enabled`), a single Recreate replica without leader election, polls
each Feed with conditional requests through a client that reaches only public
addresses, and keeps the validators and the 100 most recently seen items (ID,
title, link, published time and the time it first saw them) in `Feed.status`;
items outlive the feed's own window. A Feed's first recorded items are seeded
silently. Each later poll that finds new items wakes the Task named by every
same-namespace `FeedBinding` once, with the new items' titles, links and their
first-seen time, under an idempotency key derived from them. Status advances
only after every wake succeeded. The crawler's ServiceAccount holds `wake` on
Tasks cluster-wide, so namespace administrators decide through FeedBindings
which of their Tasks a Feed wakes. With the crawler enabled, the chart
advertises it as the `feeds` ClusterMCPServer that every ServiceAccount may
use, with a token for its Service's audience; `list_feeds` and
`list_feed_items` (by feed, `since` a first-seen time, limit) cover the Feeds
of the worker's own namespace, and the worker reads linked pages it finds
relevant.
MCP servers reach workers by advertisement and RBAC. A cluster-scoped
`ClusterMCPServer` or a namespaced `MCPServer` names a URL and an optional
token `audience`; its metadata name is the endpoint name. Every in-cluster
token audience the harness checks is its Service's `<service>.<namespace>`,
each such Service checking tokens on one port, so the wake endpoint has its
own Service. Without `audience`, a server's audience is derived the same way
from its in-cluster URL, and a server outside the cluster gets no token; an
explicit audience serves servers with a contract of their own, such as
Driftmower. Namespace administrators may set any audience: through the Agent
Pod template they can already mint worker tokens for any audience. When it creates a worker Pod, the operator attaches every server on
which a SubjectAccessReview grants the worker ServiceAccount the custom verb
`use`, a namespaced server overriding a cluster one of the same name. One
projected volume at `/var/run/agent-tokens` holds every worker token:
`agent_operator`, `rabbitmq`, `mcp/<name>` per server and
`mcp/mailbox-<name>` per Mailbox. Attachment stays out of the worker
configuration hash, like Mailboxes, so RBAC changes reach the next worker Pod.
Wildcard RBAC such as cluster-admin uses every server. The servers still
authorize each call: advertise only Kubernetes-native servers, which check
TokenReview and SubjectAccessReview per call, or authless servers with
nothing sensitive (see `docs/security-model.md`).
Alertmanager notifications posted to Slack by a bot and human replies in that
thread are separate arrivals. Ignore bot echoes, preserve a human reply's
Slack source and receipt, and never merge by similar text, labels, or channel.
Slack thread timestamps and Alertmanager fingerprints have no inherent common
identity. A verified link must add the Slack thread key as another routing key
on the appropriate Task; subsequent replies then reach that Task. If the link
does not exist yet, route the reply to the Slack fallback Task so it remains
visible. The first reply must be copied to the linked Task with its original
source, actor, and receipt before the fallback thread is considered handled.
Do not mark a shared fallback Task complete merely because one thread linked.
The operator's execution-bound Agent MCP provides bounded same-Agent Task
list/detail, create with an optional exact routing key for deduplication, and
audited completion. A worker can report progress and set a short title only on
its own Task. The operator performs MCP-triggered Task spec/status writes; the worker receives
no Task write RBAC. Key claiming on an existing Task and reopening remain to
be implemented.
Completion must stop the owning worker, retain history/PVC for review, and not
silently reopen on duplicate webhook delivery. The operator suspends a
completed Task, which deletes its worker Pod at once. A webhook
`issues.closed` or `pull_request.closed` event on a Task's first routing key
is recorded and completes the Task without prompting the worker. Later events
on a completed Task are recorded without waking it. A later authenticated event or
user action may reopen according to the Task's lifecycle policy. Slack-to-alert
linking is not implemented yet.
The operator UI uses the dark green harness palette and a keyboard-accessible
mobile task drawer containing the Task list and creation form.
Within the selected Agent, the Task list sorts by the latest user, external
(webhook or mail), or agent message, with creation as fallback. System Pod and
probe updates do not change list order. The row shows that message time, Task
name and phase; namespace and Agent are already selected outside the row.
Opening a running Task requests an MCP connectivity check over a short-lived
RabbitMQ control queue for that worker Pod. The worker checks it even during a
CLI turn, initializes each configured MCP and lists its tools,
then reports endpoint names and availability through the execution-bound Agent
MCP. Workers get `agent_operator`, the advertised servers they may use and
one `mailbox-<name>` per RBAC-authorized Mailbox. The operator accepts any sanitized report: names match
`^[A-Za-z0-9_-]{1,64}$` without duplicates, and detail, title, instructions
and tool names are bounded. The operator records the latest request and result in Task status and
streams the change to the UI. Requests are coalesced for 20 seconds. A Task
without a running worker cannot be probed by opening it. Probe messages target
one Pod UID so replacement workers discard stale requests. Probing neither
starts a CLI turn nor extends the worker idle deadline.
The Task header renders MCP endpoint results as green available and red
unavailable status dots. Buttons show the server's `serverInfo.title`, or the
capitalized endpoint name before the first probe. The multi-line tooltip shows
state and check time, the server's bounded `initialize` instructions and its
tool names.
The Device bridge's instructions list the caller's authorized Devices.
Server names are DNS labels that cannot start with `mailbox-`.
Apply the Agent CRD with `kubectl apply --server-side`; it is too large for
client-side apply. Selecting one toggles it in
`Task.spec.disabledMCPs`, which the operator sends to a running worker as
runtime configuration; the worker omits disabled endpoints from the CLI
configuration from its next turn. `agent_operator` cannot be disabled. It
keeps the last completed result in `Task.status.mcpProbe` with its `checkedAt`
time and shows it washed out while a check is pending, after a failed check or
when the worker is stopped; only a fresh result of the current worker is shown
at full strength. The Task title remains in
the header, while namespace, Agent, Task name and phase
stay in the Task list instead of a duplicate subtitle.
Transitional states animate: a working turn shows a typing bubble, a starting
or stopping worker shows a spinner line, and the Task list dot pulses. Pending
Send and header actions show a spinner. Non-JSON proxy errors, such as a 504,
display as readable text with their HTTP status.
The chat composer shows a wakeup instruction as its placeholder and submits
that instruction when Send is clicked with an empty input.
The Task header offers Suspend/Resume, which sets `Task.spec.suspend`; the
operator stops the worker and keeps the PVC, native session and queued
messages. A completed Task cannot be resumed from the UI. Stop is shown during
a working turn and sends a `stop` message on the worker Pod's control queue;
the worker interrupts only a turn started before the request, reports it as
stopped and stays resident. Each action adds an attributed system line.
On desktop, the Task header offers a collapsible list of copyable kubectl
commands for Task and Agent status, the full Task and worker Pods. It also
includes a `kubectl apply -f - <<'EOF'` Task manifest template using the
current Agent and namespace; the user fills in a Task name and objective.
The New Task form and Task detail expose a Gitea webhook setup template for
the selected Agent. Its named path uses the configured public webhook origin,
and the UI gives copyable WebhookReceiver, binding and signing-key retrieval
commands. These
are instructions and never expose Secret values through the API. The expected
Mittwald-generated signing key is distinct from a Gitea API token.
The same view gives a copyable command that prints the `device-agent
--endpoint` line for each Device in the Agent's namespace, reading the key
through its connectionSecretRef with the user's own kubectl. It follows with
commands that download the latest `device-agent` release for the device's
`uname -m` architecture and install a systemd unit that runs it with
`--foreground` at boot, keeping the endpoint in a root-only EnvironmentFile.
Task selection is encoded as `namespace` and `task` URL query parameters,
restored after OIDC callback and kept in browser history.
While the worker idles, a system line after the last reply counts down to idle
shutdown from `Task.spec.idleSeconds` and `Task.status.lastResultAt`. It updates
locally each second, labels the deadline as expected, and states that Task
deletion is manual; other worker states appear only in the timeline.
It records accepted UI prompts, routed webhook notices with validated source
URLs when provided, and worker `agent_result` lines from the owned Pod's bounded logs; full native CLI
transcripts remain private on the Task PVC. The `matmlazi-mail` Mailbox Pod
owns the live IMAP connection and SMTP credential projection.
External chat headers show the sender with a readable transport label, such as
"via Gitea webhook notification" or "via mail notification". When a verified
source URL exists, the transport label is the link. External and web chat
messages share the same incoming bubble styling.
The namespaced Mailbox CR has independent optional IMAP and SMTP sections.
When a Mailbox is created, the operator reconciles one Node.js Pod with IMAP
and/or SMTP containers. Each container receives only its protocol credential
keys and nonsecret settings as explicit environment variables. The IMAP
container uses IMAP IDLE, fetches headers only,
and publishes persistent events to the RabbitMQ intake exchange as the
`agent-mailbox` ServiceAccount; the operator accepts mail events only from
that ServiceAccount in the Mailbox's own namespace.
The operator advances Mailbox.status.imap.cursor after routing the event.
The MailBinding CR binds one
Mailbox to one Agent for metadata-only notification routing through declarative
header matchers. RE2 named captures and bounded built-in mail metadata expand
literal event templates, and `${name|lower}` lowercases a value; there is no
mail CEL evaluator or provider-specific
parser. The receiving mail server checks SPF, DKIM and DMARC during SMTP
delivery and stamps Authentication-Results. The IMAP service exposes bounded
RFC 5322 headers to matchers; it does not redo those checks or synthesize
authentication verdict fields. The GitHub example matches the local receiving
server's authserv-id and DMARC result in the stored header, and takes the
issue URL from `References`, which GitHub's DKIM signature covers; its
`Message-ID` and `X-GitHub-*` headers are unsigned. Ambiguous From or
duplicate matched headers fail closed, and a template that expands to an
invalid URI is a nonmatch. The key resolves through the same exact Task lookup
and TaskPolicy as webhook events. Mail and webhook arrivals are both recorded
with their own source and receipt; the coalescing window above keeps a
notification of the same activity from waking the Task twice. Mail body text is never routing
authority.
For an exact Task key match, the operator appends an IMAP thread external
reference using the root RFC Message-ID, Mailbox name and UID, and folder.
It also records the server thread ID when IMAP exposes OBJECTID or Gmail
threading. Namespace is implicit in the Task; the Mailbox UID prevents reuse
after a Mailbox is recreated. This source reference does not change worker
checkout configuration or event routing. Fallback intake Tasks do not claim
the thread reference.
Unmatched mail goes to a configured intake Task unless a forge-verified
TaskPolicy creates one; headers alone never create a Task. A MailBinding alone
decides which Agent a Mailbox wakes. Kubernetes RBAC decides mail access: when
creating a worker Pod, the operator attaches each same-namespace Mailbox on
which a SubjectAccessReview grants the worker ServiceAccount verb `send` or
`read`, so several Agents can share a Mailbox. An optional
`Mailbox.spec.agentRef` pins it to one Agent: other Agents' workers are not
attached even when RBAC, such as cluster-admin, would allow it, and the `mcp`
container accepts only that Agent's worker ServiceAccount. A missing Agent
leaves the pinned Mailbox accepting no worker. Attachment is excluded from the worker configuration hash,
so RBAC changes reach a worker with its next Pod. Each attached Mailbox becomes
the worker MCP endpoint `mailbox-<name>`, served by an `mcp` container in that Mailbox's Pod behind
the operator-owned Service `agent-mailbox-<name>`. Workers present a projected
token for that Service's audience. The `mcp` container authenticates it
with TokenReview, accepts only ServiceAccounts of the Mailbox's namespace and
checks a SubjectAccessReview per call for the custom verb `send` or `read` on
that Mailbox; grant them with ordinary Roles. Only the `mcp` container
receives the `agent-mailbox` ServiceAccount token, which needs only
TokenReview and SubjectAccessReview creation, and it holds no mail credential.
It forwards authorized calls over loopback to the IMAP listing and SMTP
containers, which listen only on 127.0.0.1. Mail search remains pending.
`send_email` succeeds only after SMTP acceptance;
transport errors can leave the outcome unknown and are never retried
automatically. Mailbox.status holds the IMAP UIDVALIDITY/UID cursor, advanced
after Task routing. Mail bodies, outbox entries and Task bindings do not belong
in CR status. No per-message or per-delivery CRDs exist. Optional Sent-copy
archiving preflights a unique selectable IMAP Sent folder, then appends the
exact SMTP message bytes with `\\Seen` after SMTP acceptance. SMTP success
and archive failure are reported separately so a retry cannot silently send a
duplicate.
Signed pings are acknowledged without routing; the operator records bounded
per-Task delivery receipts. Anyone holding a receiver's secret can forge its
deliveries, and GitHub and Gitea sign neither headers nor a timestamp, so keep
secrets with the receiver owner. The operator UI uses its native OIDCClient owner
Secret and bearer tokens. When an Agent sets `spec.access`, its `users` (OIDC
`sub` claims) and `groups` alone decide who may read, create, prompt and control
its Tasks, and an empty `access` admits nobody; otherwise SubjectAccessReview
for Agent `get` rights decides.
Its API streams Agent and Task cache invalidations to the browser over an
authenticated HTTP event stream. Task subscriptions are scoped to Agent names
and checked the same way; the browser fetches changed state from
the access-controlled API. RabbitMQ is not exposed to browsers.
The operator removes the RoleBindings it once owned for Agent access.
Helm has no organization source or group allowlist configuration.
The Gitea protocol copies the original issue or pull request title and body,
when present, into each bounded normalized event before appending comment text.
This lets workers see the original post from a signed comment delivery even
when a private repository cannot be fetched anonymously. A provider API token
is still needed for live repository reads and assignment checks.
The `laurivosandi` Gitea binding routes unmatched verified events to its
namespaced `laurivosandi-gitea-intake` Task. Gitea ping events produce no Task
arrival. Per-issue Task creation still requires a Gitea API token in the Agent
and a matching TaskPolicy.
PostgreSQL, memory checkpoint/restore,
Agent Substrate and a new sandbox management system remain unnecessary.

The supported baseline is Kubernetes 1.36+ with OCI image volumes, Pod user
namespaces, persistent storage, RabbitMQ and OIDC. The chart uses platform operators;
other clusters must provide compatible operators or adapt those resources to
existing services. Codemowers.cloud is an optional ready-configured environment.
Do not add legacy tool-copying fallbacks for unsupported runtimes.

Read the deployment context and namespace from the ignored local `skaffold.env`.
This checkout uses `dashboard` for the operator UI and `webhook` for its
receiver. Admission expands short hosts. Keep
unrelated workloads untouched. Browser TLS terminates at Ingress;
Ingress-to-service traffic uses HTTP for now. OIDCClient registers explicit
short source URLs; Passmower has no Ingress discovery. Read issuer, client ID,
internal URI, external origin, redirect URIs and allowed groups from the
provisioned owner Secret.

Use only the Pod `metadata.namespace` fieldRef for namespace context. No namespace UID field,
lookup, labels or environment variable, and no additional Namespace RBAC.
Pod UID remains necessary for execution identity and safe adoption.

`chart/values.yaml` exposes `operator.host` and requires an explicit public
`operator.apiResource` for OIDC `allowedResources`.
`skaffold.yaml` reads it from `AGENT_OPERATOR_API_RESOURCE` in local `skaffold.env`.
Do not use the short host for this resource: OIDC admission expands URI,
redirect and CORS fields but does not expand `allowedResources`.
The chart names its images `<imageRepository>/<name>:latest` by default, with
`imageRepository` defaulting to `ghcr.io/codemowers`. Skaffold sets it empty
and replaces the bare names with its builds, including the `image` key of the
`agent-worker-image` and `agent-mailbox-image` ConfigMaps the operator reads.
Release charts set `imageDigests` for all six harness images to the final
multi-architecture manifest digests, so the packaged references are immutable.
GitHub Actions builds images and the chart on main/master and publishes both
images and `oci://ghcr.io/<owner>/charts/agent` only for `v*` tags. There is
no separate cluster values file. Operator, worker and RabbitMQ defaults live
in the chart. Published images carry OCI source, revision and version labels.
Release packaging replaces the chart's empty `imageDigests` map with the six
final multi-architecture manifest digests and verifies every rendered image
reference before pushing the chart.

## Task identity, state and execution

Task IDs are opaque, immutable and independent of repository, issue, PR, chat
thread or native session. A Task can start from a prompt without a repository.
External references are aliases, not primary keys. Each Task owns one scratch PVC
and has at most one active execution; different Tasks run as sandbox capacity
permits. Never share a mutable checkout or implicit last CLI session between Tasks.

Every accepted prompt/action records server-assigned identity, timestamp, actor,
source and causation. User actors come from verified OIDC issuer/subject;
controller prompts and recovery actions are visibly attributed to Controller;
external actors retain connector identity; worker messages are execution-bound.
Clients/workers cannot forge user or controller attribution. Preserve creator
identity separately from later prompt authors and preserve original source events.

### Workspace and native sessions

Always initialize Git: clone the selected authorized repository or `git init`
for a scratch Task. Configure global Git user.name/email from agent identity and
`pull.rebase=true`. Work on topic branches; do not force-push or push directly
to main/master. Creating a local checkout does not create a remote; ask before
selecting/creating one unless the Task already authorizes it.
If direct push to `codemowers/agent` is denied, advise a fork under the configured
Git account and a pull request from that fork; ask before creating the fork or
remote. Do not change repository visibility as a permission workaround.

Seed `specs/<draft-or-task>-<task-prefix>.md`, workspace `AGENTS.md` with relevant
specification/MCP references, and safe `skaffold.env.example`. Populate actual
`skaffold.env` from the same CLI defaults, refresh only owned variables each turn,
preserve unrelated settings and add it to workspace `.gitignore`. Preserve user
edits and avoid duplicate Task references. Attached draft/reference text is an
immutable execution ConfigMap file, not an oversized CLI prompt argument.

Retain native Claude/Codex session files on the PVC at stable paths and record the
session ID early. Task status alone cannot restore a CLI session.
Missing/incompatible state starts a fresh session from objective, checkout and
prior outcomes, increments the epoch and records why. Changing CLI/model requires
a paused Task with no active worker and a new conversation epoch; retain workspace
and history. Full process-memory restore and filesystem snapshots are not needed.

## Runner, models and project builds

The runner uses Ubuntu 26.04 with Node 24.22.0, Git, SSH, Bash, coreutils, find,
tar, unzip, ripgrep, jq, curl, gh 2.102.0, tea 0.16.0, Helm 3.22.1, Python with
venv/pip, make and C++ build tools. The forge CLIs and Helm are verified upstream
static binaries copied into the runner image; their versions do not come from
Helm values or Agent specs. Do not mount Alpine/musl tool images onto PATH. Preinstall
both npm CLIs pinned in `runner/package.json`: Claude Code 2.1.306 and Codex 0.163.0. Install Skaffold
v2.25.0 from its binary release. Workers do not apt-install tools at startup.

Kubectl and Go come from OCI image volumes. Kubectl uses upstream
`registry.k8s.io/kubectl:v<server-version>`. The operator reads the API server
version at startup; an Agent image volume whose reference ends in `/kubectl`
without a tag gets that version's release tag, such as `v1.36.5` for
`v1.36.5+k3s1`, while tagged and other tool images stay as written.
Mount its whole `bin` directory and prepend `/opt/tools/kubectl/bin` to PATH in
both Pod types; image subpaths must be directories. Neither image embeds kubectl.
Workers also mount the official Go image's `/usr/local/go` tree at `/opt/tools/go`;
set GOROOT and PATH for that tree, keep Go caches under the Task PVC and disable
automatic toolchain downloads. Python project dependencies belong in workspace
virtual environments, not the read-only runner root filesystem.

Tasks select hosted Claude or Codex in the UI. Freeze choices per execution.
Use Anthropic credentials for Claude and OpenAI credentials for Codex; provider
and CLI selection must agree.
Selected provider Secret references are optional for Pod creation. A worker
without a usable API key for its selected CLI records a failure and exits
nonzero before workspace setup; it never switches to the other provider.
Local-model execution is a roadmap item only;
there is no discovery, API adapter or compatibility proxy in this release.

### Namespace build contract

Workers reach the Kubernetes API with their own ServiceAccount. When its token
is mounted, the worker writes an in-cluster kubeconfig (context `in-cluster`,
rotating `tokenFile`) and sets `SKAFFOLD_KUBE_CONTEXT`. It does not set
`SKAFFOLD_NAMESPACE`: RBAC decides which workload namespaces it may deploy into,
the Driftmower MCP lists them, and the Agent's Pod template environment sets it.
Process environment overrides a project's `skaffold.env`, whose context names a
workstation's kubeconfig. If it can read `zot-registry-cluster-push-kaniko` in its own
namespace, it sets `SKAFFOLD_DEFAULT_REPO` to that Secret's single registry
host followed by the Agent name, without exposing the credential; otherwise
the Agent's Pod template sets it. Values from the Agent's Pod template
environment take precedence. Do not duplicate kubeconfig or
context in Skaffold YAML, rewrite endpoints or perform human OIDC bootstrap in a
worker. Push only with `zot-registry-cluster-push-kaniko`, never the reflected
pull Secret, and do not generate registry users or mutate credentials.

Prefer Kaniko through `skaffold build`/`skaffold run`. Check missing
configuration against `skaffold schema get` and validate it offline with
`skaffold diagnose --yaml-only` first. For v4beta11 use `manifests.rawYaml`, not
invented deploy.kubectl manifest fields. Validation executes no commands/hooks
and does not establish RBAC or admission success.

Check exact get access to the push Secret through Driftmower. Skaffold Kaniko Pod
templates carry `skaffold-kaniko` and name the executor container `kaniko`;
platform admission supplies the executor image, OCI format and builder isolation/
capabilities. Direct Kaniko requires an explicitly requested verified workflow.
Failed or unclear infrastructure is a blocker: report it and stop. Do not switch
builders, registries, namespaces, privileges or mounts as a workaround. Do not
recommend Docker/Compose inside workers; they have no Docker daemon/socket.
Never change repository/package visibility or publish to personal/ephemeral
registries to make deployment work. New remotes default private unless explicitly
requested public. Forward credentials only to their verified service.

## MCP and outcome ownership

MCP scopes are layered: Codemowers is global advisory, Driftmower is cluster
capabilities/readiness, Agent is this execution's namespace facts and reporting.
Remote machine access uses projected ServiceAccount tokens verified through
TokenReview. Driftmower's stable machine audience is
`http://driftmower-mcp.driftmower.svc.cluster.local:3000/mcp`, independent of its
public browser OAuth resource. Controller probes use controller
identity; worker bridges reread rotating tokens per request and preserve sessions.
Never copy personal OAuth credentials or tokens into CLI configuration.

Harness-owned HTTP requests identify as `codemowers-agent (+https://github.com/codemowers/agent)`;
native Claude/Codex and Kubernetes API requests retain their own user agents.

| Operator Agent MCP tool (`agent_operator`) | Purpose |
| --- | --- |
| `set_task_title` | Short display title for the worker's own Task |
| `report_progress` | Bounded semantic progress/question, not completion |
| `list_tasks`, `get_task`, `create_task`, `complete_task` | Same-Agent Task discovery, deduplicated creation and audited completion |
| `report_turn_start`, `report_turn_progress`, `report_turn_result`, `report_mcp_probe` | Worker runtime reporting; not for model use |

The `feeds` MCP provides `list_feeds` and `list_feed_items` for the Feeds of
the worker's own namespace when the crawler is enabled.

Each `mailbox-<name>` MCP provides `send_email` (verb `send`) and
`list_mail_folders`, `list_mailbox_messages` and `read_mail_message`
(verb `read`), as configured
by the Mailbox's SMTP and IMAP sections.

The `devices` MCP provides `list_devices`, `device_shell`,
`device_file_read` and `device_file_write`. Access is granted per Device through
Kubernetes RBAC on the worker ServiceAccount and checked on every call.

Workers authenticate to the operator with audience-bound Pod tokens.
Reject stale/cross-Task reporting; namespace and Task identity come from the
binding, not model arguments. The controller verifies actual worker termination,
results and external evidence. MCP progress or a model completion claim does not
own lifecycle. Missing final reports must remain recoverable from worker output
and observed exit state; successful exit does not establish acceptance criteria.
A replacement worker reports a turn its predecessor finished but did not report
from the PVC receipt, so the operator accepts result times back to the Task's
creation rather than the Pod's.

Agents may also share a namespace with the harness. The developer role has no
Secret access apart from `get` on the Kaniko push Secret, which Skaffold checks
before mounting it. When an Agent sets `spec.resourcePrefix`, the cluster's
`agent-resource-prefix` admission policy, which takes the Agents of the
request's namespace as parameters, confines its worker ServiceAccount within
the Agent's own namespace to resources with that prefix: create, update,
delete and exec, plus Pod and Pod-template ServiceAccounts,
Secret references and PVC mounts. Skaffold `kaniko-*` builder Pods labelled
`skaffold-kaniko` and the shared `zot-registry-cluster-push-kaniko` Secret are
excepted. The operator passes the prefix to the worker as
`AGENT_RESOURCE_PREFIX`. Writes into other namespaces, and every write of an
Agent without `resourcePrefix`, are decided by plain, namespace-scoped RBAC,
so an admin Agent such as `krtek` needs only its cluster-admin binding.

`codemowers.io/created-by` creator annotations are immutable after creation.
The workspace instructions tell confined workers to use their prefix.
Supporting worker RBAC covers approved namespaced workloads/platform offerings
(OIDC, S3, CNPG, Dragonfly, RabbitMQ, Redpanda, OpenSearch, Elasticsearch, EMQX,
MongoDB), certificates, generated Secrets, monitoring and snapshots. There is no
cluster-wide CRD/Namespace access or general Secret permission for build discovery.
Discover schemas through Driftmower and separately check intended operations.
Do not add RBAC without explicit user direction. Existing workload exec/attach
permission is not permission to introduce Pod-exec diagnostic endpoints.

## Event routing and communication

Authenticate receivers, bind them to installation/namespace identity and persist
accepted events before acknowledgement. Payloads cannot choose tenant, credentials
or arbitrary repositories. Exact references route deterministically to canonical
Tasks; do not run a routing LLM in the current controller.

### Communication

Assume collaborators are senior engineers. Keep communication brief and direct,
focused on outcomes, blockers and decisions requiring input. Give detailed plans,
explanations and logs when requested. Progress updates cover meaningful changes
or blockers; skip routine narration, acknowledgements and repeated summaries.
Final reports briefly state the result, relevant validation and material limitations.

For issue-backed Tasks, the original Git issue is the primary communication
anchor; for standalone PR Tasks, that PR is. Mail and chat are secondary, even
when they trigger the next turn. Workers post meaningful results, blockers and
confirmation questions there, read existing comments to avoid duplicates, and
summarize relevant mail or chat decisions without copying private messages or
credentials. `report_progress` records UI state and publishes no comment.

Log every `/hooks` hit before parsing and its completed HTTP/auth/routing outcome,
including rejected/malformed calls. Allowlist bounded delivery/event/action,
verified repository/issue/actor IDs and Task links; alerts expose count/status only.
Never log raw payloads, submitted body text, signatures/auth headers, query strings,
URLs, alert labels/annotations or credentials.

## Web UI, authorization and sharing

Use a public-client OIDC Authorization Code PKCE S256 SPA with no browser client
secret or BFF. Use the provisioned issuer/client/origin/redirect configuration,
exact redirects and API-scoped access tokens. Validate signature, issuer, audience,
expiry and `at+jwt` type on API requests; ID tokens are not API credentials.
Identify users by verified issuer/subject and apply authorization on each request.
Persist the OIDC client's user record, including its refresh token, in browser
localStorage so a signed-in session survives reloads. Keep redirect transaction
state in sessionStorage, clear the stored user on sign-out, and renew API-scoped
access tokens with the configured resource. Never put tokens in URLs, history or
workers. The API still validates every bearer token and its current authorization.

This instance permits `github.com:codemowers:admins` in OIDCClient and API. Explicit
subject/group allowlists override namespace membership; namespace membership is
a fallback only when no explicit allowlist exists. UI gets no Kubernetes or bot
credentials. Anonymous public draft/shared views are separately scoped below.

### Task screens and provenance

- A combobox selects the active Agent and filters the sidebar Task list. It is
  in the desktop header and below Tasks in the mobile burger drawer.
  The sidebar New button opens an editable Task objective form in the main pane.
  Show the selected Agent's effective CPU/memory requests and limits and idle
  timeout there. Agent status holds bounded, non-secret GitHub/Gitea capability
  snapshots for repository and creation links; refresh on Agent and Task-count
  changes. Show the check time and stale-data caveat in the UI. Treat uncertain
  token or organization permission as unknown.
  Selecting a bundled product draft copies its text into that ordinary objective;
  there is no separate draft or reference field in the Task. The selected Agent
  determines the CLI and a provider-discovered model can override its default.
- List at most ten accessible repositories per configured GitHub or Gitea forge
  with counts, external links and creation destinations. Distinguish organization
  policy/token uncertainty; do not probe by creating repositories. Creation
  links open the corresponding forge.
- Task URLs select the exact Task. List labels are `org/repo #123 Title`, ellipsized
  with a full tooltip. Desktop sidebar is at least 30em and resizable by pointer/
  keyboard with locally retained width; below 900px it is a burger drawer.
- Use compact title/status/actions and optional repository link. Pod/draft/model
  details belong in settings/tooltips, not duplicate heading/status rows or UUIDs.
  Show worker dots, actual Pod names in lifecycle/countdown, clear busy/queued/running
  prompts and idle time. Use Awaiting input after a completed response.
- Human bubbles are left, agent bubbles right. Display configured agent identity
  and author/time once, without IdP URLs. GitHub names link in a new tab; GitHub
  avatars fall back to stable defaults. These display identities do not
  establish authorization.
- Merge consecutive progress/result chunks for one execution, with sanitized
  Markdown. Links/bare URLs/URL-only inline code open new tabs with noopener/noreferrer;
  fenced code stays code. Never show raw tool results or credential commands in
  the timeline.
- Render operator lifecycle and control events as compact system lines.
- Keep Suspend/Resume mutually exclusive; actions share a row. Enter submits,
  Shift-Enter is multiline and IME composition does not submit.
- Report turn duration and input/output/cache tokens on agent bubbles. Show
  proxy errors such as a 504 as readable text with their HTTP status.
- Use restrained Matrix greens, red errors and amber pending states. On desktop a
  long tagline ends in an ellipsis with the full text in a tooltip; on mobile the
  tagline and the signed-in name are hidden and the avatar remains the sign-out
  button. Keep automatic header height and a bottom composer with a scrolling
  timeline filling the remaining viewport. Link the brand to codemowers/agent on GitHub.

### Drafts

Product drafts live in the embedded UI `operator/internal/api/public/drafts.json`.
Paint, File drop and Pad retain product-specific requirements. Selecting one
copies editable text into the ordinary Task objective. Task creation requires
OIDC authentication and Agent `get` access.

## Declarative examples

Use the current [developer](examples/agents/matmlazi.yaml),
[customer support](examples/agents/patpalouk.yaml) and
[admin](examples/agents/krtek.yaml) Agent examples and the
[`codemowers.ai/v1alpha1` CRD schemas](crd/) as the executable contract.
Namespace administrators supply the two fixed Secrets described above and
select individual keys in Agent specs. The operator creates Tasks and owns
Task specs; worker observations are validated and written to Task status by
the operator.

## Deferred directions

Executable image mounting for Claude/Codex, language-base selection and a static Go
worker adapter remain exploratory. Image volumes expose read-only files, not merged
root filesystems; image entrypoints/env do not run. Verify executable paths, absolute
symlinks, loaders/libraries and native session/MCP behavior before changing the
production npm installations. Mount compatible directories and combine PATH.
An Alpine/musl utility image cannot be assumed executable in Ubuntu by PATH alone.
A static adapter would still need the coding CLI, Git/toolchain, credentials, PVC
state and execution-envelope/progress/result/resume/interruption contract.

AX schemas and runner supervision/workspace hooks are possible design references.
Any reuse must preserve this harness's tenant authority, execution identity, CLI
configuration and progress/result contract; AX/Agent Substrate is not a runtime
dependency. Memory restore, filesystem branching, cross-namespace delegation,
billing and a full sandbox portal remain outside the initial scope.

Future reporting may add waiting/artifact/follow-up proposals; they do not transfer
lifecycle authority to the model. Persist timers and pending questions, reconcile
them after restarts and clean or retain Task-created workloads by explicit policy;
stopping a worker alone does not delete its deployments. CI/review/chat connectors
need duplicate/out-of-order delivery, loop suppression, pause and stale-report
verification. Shared control/queue behavior must remain identical across sources.

<!-- codemowers/agent task 9e36be24-a85c-4053-907d-2efe513a1346 -->
