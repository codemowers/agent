# Webhooks

- Event keys are canonical resource URIs, such as
  `https://github.com/owner/repo/issues/1`; pull requests use the issue URL.
  A WebhookProtocol's `allowedKeyPrefixes` bounds the keys its events and
  binding `keyCEL` may produce, at a path boundary. Webhook and mail events
  for the same URI reach the same Task.
- CEL sees `rawBody`, `body`, lowercase `headers` and `binding.match`, plus
  `urlHost(string)`, `sha256Hex(bytes)` and the cel-go string extensions such
  as `lowerAscii()`. It cannot choose a namespace, Agent, Task or credential.
- `verification.algorithm: serviceAccountToken` authenticates in-cluster
  senders such as Alertmanager by a projected token for the protocol's
  `audience`, checked with a TokenReview against the binding's
  `serviceAccounts`. The sender needs egress to the webhook Service's port
  8080: Cilium `ipBlock` rules such as `::/0` do not match in-cluster Pods.
- Give each GitHub hook its own signing key; the hook ID header is not signed.
- A StringSecret can add a key to `agent-operator-webhook-keys` but cannot
  adopt the existing Secret. Keep its other keys.
- Slack bot posts, including Alertmanager notifications, are dropped. A human
  reply routes by its thread URL and reaches the Slack fallback Task until
  another Task claims that key.
