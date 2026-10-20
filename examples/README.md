# Examples

The Agents implement the README's segmentation example in one namespace:

| Agent | Profile | Worker access in `rbac.yaml` |
| --- | --- | --- |
| `krtek` | Kube admin | Cluster-admin |
| `matmlazi` | Developer | Namespace workloads without Secrets; `get` on the Kaniko push Secret |
| `patpalouk` | Customer service | None; API token mount disabled |
| `laurivosandi` | Personal Gitea agent | None |

Sharing one namespace with the harness relies on the cluster's
`agent-worker-prefix` admission policy, with `krtek` exempt.

Apply with the context and namespace from `skaffold.env`. Files under
`*/optional/` are templates with placeholder IDs and URLs.

1. `crd/codemowers.cloud_*.yaml` with `kubectl apply --server-side`, then
   `rbac.yaml` and `agents/`.
2. `tasks/fallbacks.yaml`. Creating a Task starts a worker turn.
3. `webhooks/cluster-*.yaml`, then the `receiver-*.yaml` and `policy-*.yaml`
   files you use.
4. `mail/rbac.yaml`, then the Mailbox and MailSubscription.
5. `devices/blueos.yaml`.

Steps outside these files:

- Slack needs the app's signing secret as key `slack-codemowers` in
  `agent-operator-webhook-keys`. `mat-slack-token` holds API tokens and cannot
  verify events.
- Gitea assignment policies need a Gitea API token in
  `agent-operator-provider-tokens` and a matching `gitProviders` entry. The
  generated webhook signing key is not an API token.
- Configure senders and `device-agent` with the values the UI shows.
