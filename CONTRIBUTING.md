# Contributing

Use issues for reproducible bugs and concrete feature proposals. Include the application version, operating system/deployment mode, expected behavior, actual behavior, and sanitized diagnostic output. Do not publish bot tokens, webhook credentials, configuration files, or state databases.

## Development

Install Go 1.26 or newer. Work on a feature branch from `main`, then run:

```sh
gofmt -w cmd internal
go test -race ./...
go vet ./...
go build ./cmd/tokenresetsmonitor
```

The race detector requires a supported C compiler, including on Windows. Normal release binaries are built with `CGO_ENABLED=0`.

Use `httptest` and synthetic fixtures for API/webhook/Telegram tests. CI must not need real accounts or contact user destinations. Tests should protect behavior: complete pagination, baseline suppression, independent retries, migration integrity, and secret redaction. Add tests for a reproduced defect or a new public behavior, not snapshots of implementation details.

Keep adapters separated from scheduling/state. A sender renders and performs one attempt; the monitor owns durable retries. Use structured fields and sanitized error messages. Never log HTTP bodies, credentials, source objects, or arbitrary configuration values.

## Compatibility and review

Document changes to CLI flags, configuration, default behavior, webhook JSON, or persistent state. Existing defaults must keep their meaning in compatible releases. Provide a migration and backup/rollback story for any schema transition. Update the example configuration, README, relevant docs, and changelog alongside public changes.

PR descriptions should explain the triggering problem, resulting behavior, and verification performed. Note platform-specific checks you could not run. Native service smoke scripts require a disposable administrator/root CI runner and intentionally refuse an existing installation.

Maintainers follow [the release process](docs/releasing.md). Published versions are immutable; fixes require another version.
