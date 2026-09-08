# Release acceptance record

Recorded on 2026-09-08. Application, configuration, state, and webhook versions are independent; configuration, state, and webhook schemas remain `1` throughout this release line.

## Stable release

[`v1.0.0`](https://github.com/DKPlugins/TokenResetsMonitor/releases/tag/v1.0.0), source commit `2e43e5c17ab29c09e0d9b97d0d1a4b7164206ad5`, passed its [release workflow](https://github.com/DKPlugins/TokenResetsMonitor/actions/runs/34212987851) and [final published-artifact acceptance](https://github.com/DKPlugins/TokenResetsMonitor/actions/runs/34213326058).

Both Windows and Linux installed the published stable release, rejected tampered archives, upgraded a source-built rc.4 binary, restored a working prior service from installer backups, and preserved pending delivery identity across upgrade and rollback. Both published container architectures were pulled anonymously and ran as UID `10001` with a read-only root and networking disabled for the runtime smoke check.

All six stable assets were downloaded and verified against GitHub's sizes and SHA-256 digests; all five `checksums.txt` payload entries matched. The Windows executable reported version `1.0.0`, the source commit above, Go `1.26.0`, bbolt `1.5.0`, `x/sys 0.45.0`, and a clean source tree. The release is stable and immutable. No acceptance gates remain open.

## Accepted candidate

`v1.0.0-rc.4`, source commit `5436864615070c299d9348e1ebc7f778b63d096e`, passed the [release workflow](https://github.com/DKPlugins/TokenResetsMonitor/actions/runs/34211959021) and [published-artifact acceptance](https://github.com/DKPlugins/TokenResetsMonitor/actions/runs/34212291430).

All six published assets were downloaded and matched GitHub's SHA-256 metadata; all five payload entries matched `checksums.txt`. The extracted Windows executable reported the expected candidate version, source commit, and UTC build date.

| Area | Verified behavior |
| --- | --- |
| Linux and Windows | Race tests, `go vet`, formatting, builds, and native service lifecycles passed. |
| Source API | Local HTTP fixtures cover provider-specific pagination, page caches and `304`, late events, malformed responses, provider mismatches, and API retry deadlines. |
| Filtering and history | Confidence/scope filters, `any-paid`, unknown applicability, full baselines, revisions, changed filters/recipients/credentials, and no historical replay are covered. |
| Delivery | Independent durable channels, retries, failures, webhook templates/headers/methods, Telegram response validation, and stable notification identity are covered. |
| Test commands | Synthetic TEST markers, independent channel outcomes, exit codes, dry-run without network, and unchanged monitoring state are covered. |
| Logs and diagnostics | Redaction, rotation, retention, bounded exports, private Windows archive ACLs, and readers surviving Windows rotation/status replacement are covered. |
| Migrations | Version-zero fixtures preserve records and create backups; unsupported future configuration/state schemas are refused without modification. |
| Published Linux installer | Rejects a deliberately corrupted archive, installs a systemd service, upgrades a previous binary, preserves configuration/state hashes, and restores a working service from its real backup. |
| Published Windows installer | The same installation/upgrade/backup checks pass under PowerShell 7, including LocalService operation and uninstall preserving data. |
| Pending deliveries | A previous binary persists a failed notification; the new binary resumes it; restoring the old backup permits the old binary to resend with the same `Idempotency-Key` and detection time. Baseline history is never sent. |
| Published containers | Anonymous GHCR pulls and execution succeed on amd64 and arm64, with a read-only root, no network during runtime smoke checks, and UID `10001`. CI also checks persistent-volume ownership and reuse. |

Installer acceptance builds the previous application from the exact `v1.0.0-rc.2` source tag and downloads the candidate and its installers from the published release. The job records source commits and binary hashes. This is a real cross-version binary test; the previous executable in that job is explicitly source-built.

ARM64 container execution uses QEMU on a GitHub-hosted Linux runner. Native service acceptance runs on hosted Windows and Linux machines. Tests use local HTTP fixtures and synthetic credentials, without real webhook recipients or Telegram tokens. A separate live TokenResets sanity check established history with channels disabled on Windows and Docker.

## Dependency updates for the stable release

[PR #6](https://github.com/DKPlugins/TokenResetsMonitor/pull/6) was reviewed, rebased onto the accepted candidate, and merged as `b62f106d80f0621991a7ca61e0b14d43e14ee100`. Its final reviewed head `4e90025f01241b71e225b8af5ad6bd8d68a36dc7` passed [Linux/Windows race tests and Docker CI](https://github.com/DKPlugins/TokenResetsMonitor/actions/runs/34212427479).

A separate binary test used the checksum-verified published rc.2 Windows executable (bbolt `1.4.3`) and a local build of that reviewed head (bbolt `1.5.0`, `x/sys 0.45.0`, planned application version `1.0.0`). Upgrade and backup rollback preserved the pending notification ID and detection time; the pending count followed `1 -> 0 -> restored 1 -> 0`, with no baseline replay. The old binary used Go `1.26.0`; the local comparison build used Go `1.27.1`. The release workflow uses the version specified in `go.mod`.

## Earlier candidates

- `v1.0.0-rc.1`: source tag retained; publication stopped by a Linux ShellCheck failure.
- `v1.0.0-rc.2`: Linux acceptance passed; Windows installer acceptance exposed PowerShell 7 environment restoration behavior. Release notes identify the affected installer.
- `v1.0.0-rc.3`: both installers and published containers passed acceptance. The next candidate added private Windows diagnostic archives and file-sharing regressions.

No published version tag or release asset was replaced. GitHub immutable releases are enabled; all distributions use explicit version tags.
