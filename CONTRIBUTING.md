# Contributing

Bug reports and pull requests are welcome. Contributions are accepted under
the [Apache License 2.0](LICENSE).

- Open an issue first for larger changes so the design can be agreed on.
- [AGENTS.md](AGENTS.md) is the implementation contract. Update it when
  behavior or architecture changes, and follow its repository conventions.
- Target Kubernetes 1.36 and include the tests you ran: `go test ./...` in
  `operator/` and `webhook/`, and `npm test` at the root.
- Keep commits focused, one logical change each.

Please follow the [Code of Conduct](CODE_OF_CONDUCT.md), and report security
issues as described in [SECURITY.md](SECURITY.md).
