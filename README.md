# Codemowers Agent

Always thought that having an agent run commands on your personal laptop was nuts?
Give it a workspace in Kubernetes.

`codemowers/agent` runs Claude Code, Codex or OpenCode in Kubernetes worker Pods. Each
Task gets a persistent workspace for its Git checkout, build artifacts and CLI
session. Tasks keep working while your laptop is closed: forge events, mail and
alerts wake them, and you or a colleague can pick up the conversation from any
browser.

[![Watch the demo on YouTube](https://img.youtube.com/vi/MhEL9cvLxQE/hqdefault.jpg)](https://www.youtube.com/watch?v=MhEL9cvLxQE)

Workers are confined by the cluster, hold only the credentials their Agent
names, accept only authenticated events and push changes for human review. The
[security model](docs/security-model.md) covers who may change what, current
limits and an example of separating Agents by trust level.

To try it without your own cluster, [get a sandbox at
trial.codemowers.cloud](https://trial.codemowers.cloud).

## Features

- **Claude, Codex or OpenCode.** Each Agent picks a CLI and provider
  credential; OpenCode reaches an OpenAI-compatible server such as llama.cpp
  for local models. A Task can switch to another model from that provider and
  keep its workspace.
- **Persistent Tasks.** Follow progress and queue prompts in the browser. The
  worker stays up for the Agent's idle timeout after each reply. Suspend stops
  it and keeps the workspace, session and queued messages; Stop interrupts the
  current turn only. Suspending the Agent from the navbar stops all its Tasks.
  RBAC can let observers read conversations without prompting.
- **GitHub and Gitea.** Signed webhooks route issue and pull-request events to
  the Task that claims the issue URL. A TaskPolicy creates a Task when an issue
  is assigned to the Agent's forge account. Closing the issue completes the
  Task and stops its worker.
- **Other events.** Alertmanager, Slack and other senders post to authenticated
  receivers, and CEL turns each payload into a resource URI that selects the
  Task. Unmatched events go to a fallback Task.
- **One wake per activity.** The first event on a key wakes its Task. For the
  next three minutes by default, the same activity arriving another way, such
  as GitHub's mail after its webhook, is only recorded, and further events get
  one trailing wake. Set `coalesceSeconds` on an Agent, TaskPolicy or Task, up
  to a day for a busy issue reviewed daily.
- **Mail.** A Mailbox Pod holds one account's IMAP and SMTP credentials. Mail
  can wake Tasks, and workers send and read mail through its MCP server.
- **Remote devices.** Machines running `device-agent` connect over WebSocket;
  workers run commands and edit files on them through the Devices MCP server.
- **MCP status.** The Task header shows each MCP server's status, instructions
  and tools, and can disable one for that Task.

## Compared with OpenClaw, Hermes and Google AX

[OpenClaw](https://github.com/openclaw/openclaw) is a personal assistant across
messaging channels and devices, and
[Hermes](https://github.com/NousResearch/hermes-agent) focuses on memory,
learned skills and personal automation. `codemowers/agent` is instead built
for a fleet of coding agents on one cluster. Agents, Tasks, receivers and
policies are custom resources, so the whole fleet can be declared in Git.
Webhooks, mail and alerts wake the Task that owns an issue, thread or alert,
and work goes from that issue to tested code, a pull request and a deployment,
with humans merging. An idle Task holds no Pod, only a resource, a PVC and a
queue, so one cluster can carry hundreds of Agents and thousands of Tasks fed
by one forge webhook per organization.

[Google AX](https://github.com/google/ax) runs on [Agent
Substrate](https://github.com/agent-substrate/substrate/blob/main/docs/architecture.md),
with PostgreSQL, its own sandbox scheduler and memory and filesystem
checkpoints on gVisor or Kata. A coding Task's state is its checkout,
uncommitted changes, artifacts and CLI session, so `codemowers/agent` keeps it
on a PVC and runs workers as ordinary Pods confined by your cluster: Resume
starts a new Pod on the same files, with no extra database or scheduler.

## Install

Requirements:

- Kubernetes 1.36 or later, with a container runtime that supports image
  volumes and Pod user namespaces.
- The RabbitMQ Cluster and Messaging Topology operators, and Passmower or
  another provider of the `OIDCClient` resource.
- An Ingress controller, DNS and TLS for the dashboard and webhook hosts.
- Admission policies that confine worker Pods, as described in the [security
  model](docs/security-model.md#admission-policies).

The chart ships the `codemowers.ai` CRDs in [chart/crds/](chart/crds/). Helm
installs them with the first release but never upgrades or deletes them, so
apply them from the release tag before each upgrade:

```sh
kubectl apply --server-side -f chart/crds/
```

Install the chart:

```sh
helm install agent oci://ghcr.io/codemowers/charts/agent \
  --namespace codemowers-agent-system --create-namespace \
  --set operator.host=dashboard.example.com \
  --set operator.apiResource=https://dashboard.example.com/api \
  --set webhook.host=webhook.example.com \
  --set webhook.publicOrigin=https://webhook.example.com
```

The hosts default to the short names `dashboard` and `webhook`, for clusters
whose admission expands them. `operator.apiResource` must always be the full
dashboard URL plus `/api`, or sign-in fails with `invalid_target`. See
[chart/values.yaml](chart/values.yaml) for the other settings.

Next, create the [credential
Secrets](docs/security-model.md#credential-storage) and apply Agents, RBAC,
Tasks and receivers from [examples/](examples/README.md). The dashboard shows
copyable kubectl commands and the webhook and `device-agent` setup for each
Agent.

## Workspaces and builds

Workers run Ubuntu 26.04 with Node.js, Git, ripgrep, Python, build tools, gh,
tea, Helm, Skaffold and the Claude Code, Codex and OpenCode CLIs pinned in
[runner/package.json](runner/package.json). Downloaded tools are verified
against [runner/checksums.sha256](runner/checksums.sha256). kubectl and Go
are mounted from their upstream images. Install Python packages into a virtual
environment in the workspace.

A Task's workspace starts as a Git repository with its specification under
`specs/`. Its PVCs come from the Agent's `volumeClaimTemplates`; without them
Tasks are stateless. Workers build images with Kaniko through Skaffold, using
their own ServiceAccount's kubeconfig.

## Development

Copy [skaffold.env.example](skaffold.env.example) to `skaffold.env`, fill in
your cluster's values, apply the CRDs, which Skaffold does not render, and
run Skaffold:

```sh
kubectl apply --server-side -f chart/crds/
skaffold dev
```

## Roadmap

- **Per-Task broker access:** confine each worker to its own Task's queue.
  Today its token reaches every queue of its ServiceAccount.
- **Reopening and key claiming:** reopen a completed Task on a later event or
  user action, and let a worker add routing keys to its own Task.
- **Slack:** link threads to existing Tasks, including alerts Alertmanager
  posted to Slack, and copy the first reply there.
- **Gitea:** test assignment-based Task creation end to end, and discover where
  repositories can be created.
- **Mail:** search, and tie sent messages to their Task so replies return to it.
- **Worker placement:** run workers outside their Agent's namespace, so a hosted
  tenant's worker memory counts against its own quota.
- **Separate repositories:** these components could run without the operator:
  - `mailbox/`: an IMAP and SMTP MCP server, with a pluggable bearer-token
    check in place of TokenReview.
  - `webhook/`: a signed-webhook-to-AMQP gateway with CEL protocols loaded from
    files instead of custom resources.
  - `operator/devicebridge/`: a shell and file MCP server for `device-agent`.
  - `runner/` and `src/`: an image wrapping the Claude, Codex and OpenCode CLIs with
    session persistence.

## Contributing

Bug reports and pull requests are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md)
and [MAINTAINERS.md](MAINTAINERS.md). Report vulnerabilities as described in
[SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE).
