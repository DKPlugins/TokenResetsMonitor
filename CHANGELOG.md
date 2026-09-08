# Changelog

All notable changes are recorded here. This project follows [Semantic Versioning](https://semver.org/).

## Unreleased

## 1.0.0-rc.1

Initial release candidate; stable release acceptance is tracked in [docs/releasing.md](docs/releasing.md).

### Added

- Provider-specific, fully paginated TokenResets polling with persistent ETag caches.
- Provider, event-type, confidence, plan, product, and window filters.
- Transactional event history and independent durable webhook/Telegram delivery queues.
- Baseline suppression, retry scheduling, source-revision handling, and configuration reconciliation.
- YAML configuration, environment/CLI overrides, setup wizard, validation, and migration commands.
- Synthetic test notifications and network-free dry-run previews.
- Structured redacted logs, file rotation, status snapshots, and diagnostic ZIP export.
- Windows service integration, Linux systemd unit, versioned installers, Docker/Compose deployment.
- Versioned configuration, state, and webhook schemas with migration backups.
- CI, cross-platform release archives, checksums, GHCR multiarchitecture publishing, and Dependabot updates.
