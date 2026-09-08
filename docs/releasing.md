# Release process

The release line began with `v1.0.0-rc.1`. The completed gates for stable `v1.0.0` are recorded in [acceptance.md](acceptance.md). Re-run these gates for later releases. Pushing a `v*` tag triggers CI, archive builds, multiarchitecture image publication, and GitHub Release creation. A prerelease tag creates a prerelease on GitHub.

## Maintainer preparation

- Keep the repository default branch `main`, enable GitHub Actions, and allow its token to write repository releases and GHCR packages in the release job.
- Enable GitHub's immutable releases setting and protect published version tags against movement or deletion. The workflow also refuses an existing GitHub release or container tag.
- Verify that `ghcr.io/dkplugins/tokenresetsmonitor` can be pulled anonymously; package visibility is independent of repository visibility. The initial release line passed this check.
- Require CI before merging and use reviewed dependency updates from Dependabot. Actions are pinned to commit SHAs.

## Acceptance checklist

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

CI automatically tests native service lifecycles on disposable runners and container architecture startup. After publication, dispatch **Installer acceptance** with the candidate tag and a distinct previous Git tag. It rejects tampered release archives, installs the published candidate, runs the previous version built from its exact tagged source, upgrades through the published installer, and restores a working previous service from the installer's actual backup. Configuration/state hashes and source commit provenance are recorded. A separate binary fixture then verifies that pending deliveries survive upgrade and restored-backup rollback with the same idempotency key and without historical notifications. This distinguishes a source-built previous candidate from a previously published binary; the candidate under acceptance is always downloaded from its release.

## Build and publish

The local build script requires a POSIX environment, Go, Git, `tar`, `zip`, and `sha256sum`:

```sh
sh scripts/release.sh v1.0.0
```

It creates `dist/<tag>/` containing Linux amd64/arm64 `.tar.gz` archives, a Windows amd64 `.zip`, `checksums.txt`, and standalone installers. Archives contain the binary, example configuration, README, license, installers, and systemd unit. The script refuses to overwrite an existing output directory.

Build information is injected through `internal/buildinfo.Version`, `Commit`, and `Date`. Binaries use the SemVer without the Git tag's `v` prefix. The image uses the exact Git tag, for example `ghcr.io/dkplugins/tokenresetsmonitor:v1.0.0`. No moving `latest` tag is published.

After review and acceptance, create an annotated version tag on the intended `main` commit and push that tag. The workflow runs verification before publishing. Do not force-update tags, replace assets, or overwrite a container version. If a publishing job partially succeeds, inspect its artifacts and publish a new candidate tag rather than reusing that version.

PATCH releases contain compatible fixes, MINOR releases add compatible capabilities, and MAJOR releases change existing public contracts incompatibly. Each release should describe configuration/state/webhook schema changes explicitly even when their numbers stay unchanged.
