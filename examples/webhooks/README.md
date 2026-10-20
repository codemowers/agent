# Webhook receivers

- The endpoint prepends `external` (public, signed) or `internal` (port 8083)
  to every key after CEL runs. Include it in `routingKeys` and
  TaskCreationPolicy prefixes, never in CEL.
- CEL sees `rawBody`, `body`, lowercase `headers` and `binding.match`, plus
  `urlHost(string)` and `sha256Hex(bytes)`. It cannot choose a namespace,
  Agent, Task or credential.
- `verification.algorithm: none` works only on the internal listener's named
  binding paths. Keep that Service off Ingresses and restrict it with a
  NetworkPolicy; cluster DNS is not authentication. The sender also needs
  egress to port 8083: Cilium `ipBlock` rules such as `::/0` do not match
  in-cluster Pods.
- Give each GitHub hook its own signing key; the hook ID header is not signed.
- A StringSecret can add a key to `agent-operator-webhook-keys` but cannot
  adopt the existing Secret. Keep its other keys.
- Slack bot posts, including Alertmanager notifications, are dropped. A human
  reply routes by its Slack thread key and reaches the Slack fallback Task
  until another Task claims that key.
