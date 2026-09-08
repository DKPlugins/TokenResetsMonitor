# Release process

The release target is stable **`v1.2.0`**, without an RC suffix. Upgrade and rollback acceptance covers both `v1.0.0` and `v1.1.0-rc.2`. The completed historical gates for stable `v1.0.0` remain in [acceptance.md](acceptance.md). Re-run all gates for the exact new commit/tag; old checked boxes are not evidence for 1.2. Pushing a `v*` tag triggers CI, archive builds, multiarchitecture image publication, and GitHub Release creation. Only a tag containing a prerelease suffix produces a prerelease on GitHub.

## Maintainer preparation

- Keep the repository default branch `main`, enable GitHub Actions, and allow its token to write repository releases and GHCR packages in the release job.
- Enable GitHub's immutable releases setting and protect published version tags against movement or deletion. The workflow also refuses an existing GitHub release or container tag.
- Verify that `ghcr.io/dkplugins/tokenresetsmonitor` can be pulled anonymously; package visibility is independent of repository visibility. The initial release line passed this check.
- Require CI before merging and use reviewed dependency updates from Dependabot. Actions are pinned to commit SHAs.

## Historical 1.0 acceptance checklist

- [x] Linux and Windows `go test -race ./...`, `go vet ./...`, and formatting checks pass.
- [x] Local HTTP fixture tests cover page caching, complete baselines, filters, revisions, retries, test notifications, redaction, rotation, and diagnostics.
- [x] A clean Windows installation works as LocalService; stop/start, executable replacement, and uninstall preserve configuration/state.
- [x] A clean Linux systemd installation works as its own user; stop/start and executable replacement preserve configuration/state.
- [x] Release installers download a candidate, reject a changed checksum, preserve configuration/state on update, and leave usable backups on failure.
- [x] Docker images build and run on amd64 and arm64, use a non-root account, and preserve history in a named volume across replacement.
- [x] Older state/config fixtures migrate with backups and unsupported future versions are refused.
- [x] README commands match the candidate CLI and all release assets match checksums.
- [x] Upgrade and rollback were exercised with an actual prior candidate, including pending deliveries.
- [x] Update `CHANGELOG.md` and record the evidence and any remaining candidate-only limitations in release notes.

CI automatically tests native service lifecycles on disposable runners, container architectures, and actual binary migration/backup rollback from both supported previous versions. After publication, dispatch **Installer acceptance** with the release tag and `previous_ref=all` (the default), or select one distinct previous tag. Its Linux/Windows matrix rejects tampered archives, installs the published release, upgrades the previous version built from its exact tagged source, and restores a working previous service from the installer's actual backup. Configuration/state hashes and source commit provenance are recorded. Expected schemas are read from the candidate compatibility manifest and previous runtime status (schema 1 for the pre-metadata 1.0 binary). The old binary must refuse migrated state without changing it, and the restored backup must recover the original hashes. The pending-delivery fixture additionally verifies stable idempotency keys and no historical replay. The new version under installer acceptance is always downloaded from its published release; the previous version is explicitly source-built.

## Build and publish

The local build script requires a POSIX environment, Go, Git, `tar`, `zip`, and `sha256sum`:

```sh
sh scripts/release.sh v1.2.0
python3 scripts/verify-release.py dist/v1.2.0
```

It creates `dist/<tag>/` containing Linux amd64/arm64 `.tar.gz` archives, a Windows amd64 `.zip`, `checksums.txt`, and standalone installers. Archives contain the binary, example configuration, README, license, changelog, contributor guide, complete docs, Compose file, installers, compatibility manifest, and systemd unit. Release installers default to their own version; compatibility.json is also a standalone checksummed asset. The script refuses to overwrite an existing output directory.

Build information is injected through `internal/buildinfo.Version`, `Commit`, and `Date`. Binaries use the SemVer without the Git tag's `v` prefix. The image uses the exact Git tag, `ghcr.io/dkplugins/tokenresetsmonitor:v1.2.0`. No moving `latest` tag is published.

After the code review, local checks, and archive inspection, commit only the intended changes on `main`, push the branch, and wait for its exact commit's CI result. Create an annotated `v1.2.0` tag on that commit and push the tag. The Release workflow repeats verification before publishing. Check the resulting release is published, stable, and points to the intended commit, with all three archives, installers, compatibility manifest, checksums, and both container architectures.

Do not force-update tags, replace assets, or overwrite a container version. The workflow refuses existing GitHub releases and GHCR tags. If publication partially succeeds, inspect both services before retrying: an existing immutable artifact requires a new version, not deletion or replacement. Local unpublished build output may be removed explicitly only after checking its path and confirming it was not published.

PATCH releases contain compatible fixes, MINOR releases add compatible capabilities, and MAJOR releases change existing public contracts incompatibly. Each release should describe configuration/state/webhook schema changes explicitly even when their numbers stay unchanged.

## Gates for 1.2.0

- Verify strict pagination completion, scan size limits, revision ordering, destination-wide cooldowns, and old-state migration/rollback fixtures.
- Verify hidden setup prompts, pairing nonce/update handling, preservation of active bot webhooks, Slack error handling, Windows configuration/backup ACLs, and atomic save conflict detection.
- Exercise invalid/valid/infrastructure reloads under concurrent polling and delivery with the race detector.
- Run a continuous non-root read-only container; source failure must remain live and become unready. Check heartbeat failure and graceful shutdown separately.
- Validate Prometheus metric shape and secret-free output, doctor offline/non-mutating operation, and update HTTPS/ETag/SemVer/rate-limit/manifest behavior.
- Inspect each release archive for compatibility.json, complete docs, correct embedded versions, and valid checksums. Ensure installers validate with the configured service environment.
- Repeat native systemd/Windows acceptance on disposable runners. Do not reuse the old acceptance record as evidence for a new release.
- Verify read-only history/explanations and candidate filter previews against stored real events while running and stopped; previews must make no notification requests or writes.
- Verify live selected/bulk failed retries, in-flight races, destination changes, cancellation explanations, and persisted cooldowns. Preserve ordinary delivery IDs across schema 3 migration.
- Verify correction/retraction sequencing against each destination's acknowledged revision, meaningful comparisons, explicit withdrawal evidence, and no baseline or newly enabled destination replay.
- Verify control authentication, request limits, read-only offline access, retention markers, and explicit missing legacy detail. No management credentials may enter diagnostics or metrics.
- Run `python scripts/test-upgrade.py` for both previous versions. Confirm configuration/state schema 3 and webhook schema 1 match the compatibility manifest.

## Published-release acceptance

After the tagged release is published, dispatch the workflow from that same tag:

```sh
gh workflow run installer-acceptance.yml --ref v1.2.0 -f version=v1.2.0 -f previous_ref=all
```

Wait for all four native upgrade/rollback jobs and the public container job. Verify the candidate version and source commit in the logs, anonymous GHCR pull on amd64/arm64, non-root execution, checksum rejection, service permissions, backup restoration, and pending notification identity. Record the exact CI, Release, and Installer acceptance run URLs/results in release notes or the delivery report; do not label an unexecuted gate as passed. Configuration/state schema 3 requires the matching old backup for rollback.
