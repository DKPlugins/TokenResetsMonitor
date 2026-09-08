# Changelog

All notable changes are recorded here. This project follows [Semantic Versioning](https://semver.org/).

## Unreleased

## 1.0.0-rc.3

### Fixed

- Restore absent environment overrides correctly after Windows installer initialization in PowerShell 7.
- Keep optional Windows Event Log diagnostics from failing installer acceptance when no provider is registered.

### Maintenance

- Group dependency updates into one weekly pull request and delete branches after merging.
- Exercise published installers, tampered checksums, upgrades, backup restoration, and pending-delivery identity on Linux and Windows.

## 1.0.0-rc.2

### Fixed

- Make the Linux installer's systemd availability check pass the hosted runner's ShellCheck rules.

### Release status

- The first candidate tag remains available as source; its release publication was stopped by CI before artifacts were published.
- Candidate acceptance includes native Windows/systemd services and Docker startup on amd64 and arm64.

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
