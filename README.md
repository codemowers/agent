# Codemowers Agent Operator

This repository builds the Go Agent/Task operator, its GitHub webhook receiver,
and the coding worker image. Kubernetes is the execution target. The older Node
controller lives in [codemowers/agent](https://github.com/codemowers/agent) and continues to serve its
own Redis Tasks at `controller`; this operator has separate Agent and Task CRs.

Agent and Task use `codemowers.cloud/v1alpha1` and are namespaced. An Agent
defines a worker Pod template, model, credentials, volume claim templates and
GitHub hook subscriptions. A Task belongs to one Agent and has its own worker
Pod and persistent workspace claims. The operator supplies the Skaffold-built
worker image, so an old image in an Agent CR cannot select a different runner.
Workers have `hostUsers: false` and `restartPolicy: Never`. Their Pod names use
`agent-<Agent name>-<Task prefix>-<incarnation ID>`.

## Deployment

The Skaffold configuration deploys Helm release `agent-operator` in
`codemowers-agent-system`, alongside the old release. It builds with cluster
Kaniko and pushes to the configured Zot repository. This instance expects the
`zot-registry-cluster-push-kaniko` Secret in that namespace and reuses the
existing `agent-events` RabbitMQ cluster. Keep its queues and PVCs.

| Component | Kubernetes name | Public short host |
| --- | --- | --- |
| Go operator and UI | `agent-operator` | `sloperator` |
| GitHub receiver | `agent-operator-webhook` | `webhook` |
| Default worker ServiceAccount | `agent-operator-worker` | None |

The older Node deployment keeps `controller`. This release does not deploy
Redis, the old controller, or a public RabbitMQ endpoint.

Use Kubernetes 1.36 or newer with OCI image volumes, Pod user namespaces,
persistent storage, OIDC and the platform operators required by the chart.
Configure your local, ignored `skaffold.env` with the target context, namespace,
Zot repository and the operator's full public API resource:

```dotenv
SKAFFOLD_KUBE_CONTEXT=your-kubeconfig-context
SKAFFOLD_NAMESPACE=codemowers-agent-system
SKAFFOLD_DEFAULT_REPO=zot.registry.example.com/your-repository
AGENT_OPERATOR_API_RESOURCE=https://your-public-sloperator-host/api
SKAFFOLD_CLEANUP=false
```

The short `operator.host` is `sloperator`; OIDC admission expands the Ingress,
redirect and CORS URLs. It does not expand `allowedResources`, so
`AGENT_OPERATOR_API_RESOURCE` must be the full public origin plus `/api`.
The OIDCClient owner Secret supplies the issuer, client ID, origin and redirect
URI to the operator. The webhook short host is `webhook`.

After provisioning the required platform resources and provider Secrets, run:

```sh
skaffold dev
```

The chart grants the operator CR, Pod, PVC and SubjectAccessReview permissions;
the webhook reads Agent subscriptions and their named signing Secrets; the
default worker Role is namespaced. Review the exact
[operator](chart/templates/operator-deployment.yaml),
[webhook](chart/templates/webhook-deployment.yaml) and
[worker](chart/templates/worker-rbac.yaml) rules before granting broader access.
The example [Agent RBAC](examples/rbac.yaml) includes separate developer,
read-only and cluster-admin worker identities. Grant browser users Kubernetes
`get` access to an Agent; `spec.access` is retained in the CRD but ignored.

## Create an Agent and Task

See [Agent examples](examples/agents.yaml) for Pod templates, model and
credential Secret references, persistent volumes and GitHub subscriptions.
Create the provider Secrets referenced by those examples with a `token` key:

```sh
kubectl -n codemowers-agent-system create secret generic agent-worker-github-token \
  --from-literal=token='<github-token>'
kubectl -n codemowers-agent-system create secret generic agent-worker-anthropic-api-token \
  --from-literal=token='<anthropic-api-key>'
kubectl -n codemowers-agent-system create secret generic agent-worker-openai-api-token \
  --from-literal=token='<openai-api-key>'
```

Supply actual values locally; do not commit credentials. Apply the
[Agent and Task CRDs](crd/) before creating resources.
The operator UI at `sloperator` can create Tasks and send follow-up prompts.
Its API requires an OIDC bearer token and checks Agent `get` rights through
Kubernetes SubjectAccessReview.

The live UI uses an authenticated HTTP event stream fed by Kubernetes Agent and
Task watches. A browser subscribes to named Agents only after its access is
checked. Events contain refresh hints; the UI reads changed state through the
access-controlled API. RabbitMQ remains private for worker prompts and webhook
delivery. The stream reconnects to recheck credentials and access.

## GitHub webhooks

GitHub and GitHub Enterprise use one receiver path:

```text
https://webhook.codemowers-agent-system.ee-lte-1.codemowers.io/hooks/github
```

Configure each Agent's `spec.githubWebhooks` with its GitHub server URL,
`X-GitHub-Hook-ID` value and a signing Secret in the Agent's namespace. The
Secret uses a `token` key. Use a distinct signing key for each GitHub hook
because the hook ID header is not signed. For the example hook, select issues,
issue comments, pull requests, pull request reviews and review comments with
`application/json` payloads.

The receiver validates each subscriber's signature and signed repository URL,
then publishes an event to RabbitMQ. The operator checks the subscription and
key again before routing it. A new Task is created only when the configured
account currently has the open issue or PR assigned. Updates to an existing
Task continue after unassignment. Signed pings are acknowledged without
creating Tasks, and Task status retains bounded delivery receipts.

The webhook Signing Secret can be created using
[the example manifests](examples/webhook-secrets.yaml). Keep signing keys and
provider credentials out of Git, Helm values and image build contexts.

## Current scope

The operator manages CRD Tasks independently of the old controller's Redis
Tasks. There is no cutover or data migration. Mail, the old controller UI and
its MCP configuration remain in [codemowers/agent](https://github.com/codemowers/agent). Gitea webhook
ingestion is deferred until it has a stable hook identifier equivalent. Local
model execution is not implemented.

See [AGENTS.md](AGENTS.md) for implementation rules and the current architecture.
