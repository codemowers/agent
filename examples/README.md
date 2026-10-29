# Examples

These manifests run the Agents of the [segmentation
example](../docs/security-model.md#agent-segmentation-example). The Agents,
their Tasks, bindings, Mailboxes and the `hifiberry` Device live in namespace
`agents`, the harness in `codemowers-agent-system`, and `matmlazi` deploys
into `demo`.

| Agent | Profile | Worker access in `rbac.yaml` |
| --- | --- | --- |
| `krtek` | Kube admin | Cluster-admin |
| `matmlazi` | Developer | Namespace workloads without Secrets; `get` on the Kaniko push Secret |
| `patpalouk` | Customer service | None; API token mount disabled |
| `laurivosandi` | Personal Gitea agent | None |

No Agent sets `resourcePrefix`, so plain RBAC decides what each worker may
change. Set it to confine a worker's writes through the `agent-resource-prefix`
admission policy.

## Apply

Files under `*/optional/` are templates with placeholder IDs and URLs.

1. After installing the chart, which installs the CRDs on first install, `rbac.yaml`,
   `agents/`, including the worker PodMonitor, and the MCP servers they may
   use, `mcp/servers.yaml`.
2. `tasks/fallbacks.yaml`, and optionally `tasks/krtek-capacity-review.yaml`
   and `tasks/krtek-platform-watch.yaml` with its weekly wake,
   `tasks/krtek-platform-watch-schedule.yaml`, and its feeds,
   `feeds/platform-watch.yaml`, with the chart's `feeds.enabled`.
   Creating a Task starts a worker turn.
3. `webhooks/protocol-*.yaml`, the receivers you use, then their bindings and
   policies. Set each protocol's `allowedKeyPrefixes` to your hosts and point
   each forge hook at its receiver's path.
4. `mail/rbac.yaml`, then the Mailboxes and MailBinding.
5. `devices/hifiberry.yaml`, with the chart's `deviceBridge.enabled`.
6. Optionally `browser/browser-mcp.yaml` in `demo`; see
   [browser/README.md](browser/README.md).

Outside these files:

- Each Agent needs its `<name>-credentials` Secret, with the keys its
  manifest lists, and `agents` the `agent-operator-webhook-keys` Secret;
  `demo` needs the
  `zot-registry-cluster-push-kaniko` Secret for Kaniko builds.
- Slack needs the app's signing secret as key `slack-codemowers` in
  `agent-operator-webhook-keys`. A Slack API token cannot verify events.
- Gitea assignment policies need a Gitea API token as `GITEA_TOKEN` in the
  Agent's credential Secret, a matching `gitProviders` entry, and the Secret
  listed in the chart's `operator.credentialSecretNames`.
- Configure senders and `device-agent` with the values the dashboard shows.

`task-quota.yaml` caps the namespace at 1000 Tasks and alerts before that.
`task-maintain-agent.yaml` is a long-running maintenance Task for this
repository.

## Proposed API designs

[Shared Knowledge and Draft examples](knowledge/README.md) describe a proposed
API for reusable findings and evolving designs. Their CRDs and Agent selectors
are not implemented; these examples are not part of the Apply steps above.
