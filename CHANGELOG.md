# Changelog

All notable changes are recorded here. This project follows [Semantic Versioning](https://semver.org/).

## 1.2.0

Stable release including the monitoring, setup, and observability features introduced in the 1.1 release candidates.

### Added

- Event history and per-destination explanations for baseline suppression, filter rejection, pending work, failure, cancellation, and acknowledged delivery.
- Filter previews against retained real events without contacting notification destinations or modifying delivery queues.
- Delivery history with attempt outcomes, cancellation reasons, retry deadlines, and live retry of a selected failed delivery.
- Correction and withdrawal notifications with a comparison to the announcement previously acknowledged by that destination. Telegram and Slack enable change messages by default; webhooks explicitly opt in.
- Configurable history-detail retention, keeping baseline and delivery identity markers to prevent replay; unlimited retention is the default.
- Binary upgrade/backup rollback gates from both stable 1.0.0 and 1.1.0-rc.2 before publication, and published installer acceptance for both paths.

### Fixed

- Record acknowledgements against the exact payload sent when a source revision arrives during an in-flight request, preserving the next correction and its comparison.
- Preserve complete provider-scan discoveries when a separate retraction-detail lookup fails.
- Let a new correction or withdrawal supersede an obsolete failed change delivery without inheriting its terminal failure; retain the earlier attempt history.
- Recognize Windows connection-refused errors when a crashed daemon leaves a stale local-control descriptor, allowing safe stopped access while retaining authentication-failure protection.
- Refuse systemd smoke tests when retained data, logs, or a unit already exists, including dangling links, before registering cleanup or changing the installation.

### Compatibility

- Configuration schema 3 accepts existing schema 1 and 2 files without rewriting them. Saving a migration remains explicit.
- State schema 3 migrates older supported databases with a consistent backup. Preserve the matching configuration/state backup when rolling back to 1.0 or a 1.1 candidate.
- Standard webhook schema remains 1 with additive change-notification fields; existing ordinary notification IDs and pending work are preserved.
- Old databases cannot reconstruct delivery attempts or historical filter decisions that earlier versions never recorded; history marks those gaps instead of inventing outcomes.

## 1.1.0-rc.2

### Fixed

- Return success from the Windows installer regression script only after all assertions and cleanup pass. Expected configuration rejection no longer leaves a failing native exit code for GitHub Actions.
- Replace the blocked RC1 publication with a new immutable candidate tag; application features and schema compatibility remain unchanged.

## 1.1.0-rc.1

Release candidate for the 1.1 monitoring and notification features. Keep a pre-migration state backup when testing an upgrade from 1.0.

### Added

- Prometheus metrics, local/private-network health endpoints, and container liveness probes.
- Guided Telegram pairing and Slack incoming webhook setup with hidden secret prompts, private backups, and optional TEST delivery.
- Operational configuration reload with last-valid fallback and visible generation/reload status.
- Read-only project update checks, explicit release compatibility manifests, and online/offline doctor diagnostics.
- Expanded public onboarding, destination setup, configuration, observability, and upgrade guides.

### Fixed

- Reject missing/null pagination completion fields; bound complete history scans and preserve the newest event revision.
- Persist destination-wide rate-limit cooldowns, including across restart, in state schema 2.
- Apply private Windows ACLs to configuration files and backups; preserve secrets during setup and validation.
- Validate webhook template execution and keep operational diagnostics useful when channel secrets are unavailable.
- Validate installer candidates structurally with deferred service environment references; grant LocalService read access to private configuration and include all linked documentation and compatibility metadata in release archives.
- Use consistent release candidate versions in native/Docker builds and preserve atomic configuration edits through directory mounts.

### Compatibility

- New configuration files use schema 2; version 1 remains readable without rewriting.
- State migrates to schema 2 with a backup. Rollback to 1.0 requires a matching old-state backup.
- Standard webhook payload remains schema 1. Background release metadata checks default to every 24 hours and can be disabled; no automatic installation occurs.


## 1.0.0 - 2026-09-08

First stable release. Includes the monitoring, notification, diagnostics, service, and update features introduced in the release candidates below. Configuration, state, and webhook schema versions remain `1`.

- Accepted Windows/systemd installation, checksum rejection, upgrades, backup rollback, durable notifications, and public Docker amd64/arm64 execution.
- Updated bbolt to `1.5.0`, `golang.org/x/sys` to `0.45.0`, Alpine to `3.24`, and pinned checkout/QEMU actions after CI and cross-version database verification.
- Recorded reproducible checks and their limits in [the acceptance record](docs/acceptance.md).

## 1.0.0-rc.4

### Fixed

- Create Windows diagnostic archives with a protected access-control list, including when exporting into a shared directory.
- Allow Windows log rotation and atomic status replacement while diagnostics readers hold an earlier file snapshot.

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
