# Installing TokenResetsMonitor

The service installer examples below target stable `v1.2.0`. Use the same exact tag for the installer and binary archive. Before upgrading an older installation, preserve its configuration/state backup as described in the [upgrade guide](upgrading.md). For the source Docker setup, follow the [README](../README.md#docker).

### Linux: systemd or foreground

The installer supports Linux amd64/arm64 and requires root, `curl`, `tar`, `sha256sum`, `fuser` (usually the `psmisc` package), and standard account-management tools. Download the script for the desired version and inspect it before running:

```sh
curl -fL https://github.com/DKPlugins/TokenResetsMonitor/releases/download/v1.2.0/install.sh -o install.sh
less install.sh
sudo sh install.sh --version v1.2.0 --no-start
sudoedit /etc/tokenresetsmonitor/config.yaml
sudo tokenresetsmonitor config validate --config /etc/tokenresetsmonitor/config.yaml
sudo tokenresetsmonitor test-notification all --config /etc/tokenresetsmonitor/config.yaml
sudo systemctl start tokenresetsmonitor
sudo systemctl status tokenresetsmonitor
```

It verifies the archive's SHA-256, creates the `tokenresetsmonitor` system user, installs a hardened systemd unit, enables startup at boot, and preserves existing settings and state on updates. Omit `--no-start` to start immediately. Use `--foreground` to install without systemd; then launch `run` as the service user using your supervisor. Stop existing foreground processes before updating.

| Item | Installed path |
| --- | --- |
| Executable | `/usr/local/bin/tokenresetsmonitor` |
| Configuration | `/etc/tokenresetsmonitor/config.yaml` |
| Optional systemd environment file | `/etc/tokenresetsmonitor/environment` |
| State and status snapshot | `/var/lib/tokenresetsmonitor/` |
| Rotating file logs | `/var/log/tokenresetsmonitor/` |
| Installer backups | `/var/backups/tokenresetsmonitor/` |

For environment-based secrets, put `TRM_TELEGRAM_BOT_TOKEN=...` or similar assignments in the optional environment file and restrict it to root (`chmod 600`). systemd reads it before dropping privileges. Shell variables in an interactive terminal are not automatically available to the service. Restart after changing the file.

The supplied unit permits writes only in the default state/log directories. If you move them, add the new paths with `systemctl edit tokenresetsmonitor` and `ReadWritePaths=...`, preserving the service user's permissions.

### Windows: native service

Run the installer in **Administrator PowerShell**:

```powershell
Invoke-WebRequest https://github.com/DKPlugins/TokenResetsMonitor/releases/download/v1.2.0/install.ps1 -OutFile install.ps1
Get-Content ./install.ps1
./install.ps1 -Version v1.2.0 -NoStart
notepad "$env:ProgramData\TokenResetsMonitor\config.yaml"
$monitor = "$env:ProgramFiles\TokenResetsMonitor\tokenresetsmonitor.exe"
& $monitor config validate --config "$env:ProgramData\TokenResetsMonitor\config.yaml"
& $monitor test-notification all --config "$env:ProgramData\TokenResetsMonitor\config.yaml"
& $monitor service start
& $monitor service status
```

The installer verifies SHA-256 and runs the service as `NT AUTHORITY\LocalService`, with delayed automatic startup and restart-on-failure recovery. It grants LocalService read access to configuration/binaries and write access to data/logs. `-InstallDir` and `-DataDir` support custom absolute paths; use the same paths for later upgrades. These must be separate dedicated directories; nonempty directories without an installation marker are refused before their permissions are changed.

| Item | Installed path |
| --- | --- |
| Executable | `%ProgramFiles%\TokenResetsMonitor\tokenresetsmonitor.exe` |
| Configuration | `%ProgramData%\TokenResetsMonitor\config.yaml` |
| State | `%ProgramData%\TokenResetsMonitor\data\` |
| Logs | `%ProgramData%\TokenResetsMonitor\logs\` |
| Backups | `%ProgramData%\TokenResetsMonitor\backups\` |

`service stop`, `service start`, `service status`, and `service uninstall` work from an Administrator terminal. Uninstall removes service registration and preserves configuration, state, and logs. Startup failures appear in **Event Viewer → Windows Logs → Application**, source `TokenResetsMonitor`.

For manual installation, place the executable and configuration in permanent locations, grant LocalService the permissions above, then run `tokenresetsmonitor service install --config C:\absolute\config.yaml`. A file created by `init` is private to its owner, SYSTEM, and Administrators. Add an explicit LOCAL SERVICE Read entry through Windows Security settings before manual service installation; registration is refused with an actionable error when this grant is missing or cannot be verified. The installer applies this read-only grant automatically after initialization and keeps historical backups private. The service stores absolute, correctly quoted paths. Its working directory is not your shell's working directory. Process environment variables set in PowerShell are not inherited by SCM; use the protected configuration file or explicitly configure the service's environment through Windows administration.

## Docker directory mounts

The development Compose file builds a local image. Put the configuration at `config/config.yaml` and mount the directory read-only. Editing tools often save by replacing an inode; a single-file bind mount can keep the old inode visible indefinitely. Directory mounts let the daemon read the new file.

All files must remain readable by UID/GID 10001. On Linux use directory mode 750 and config mode 640 with group 10001. Match permissions again after editing if your editor replaces them. Keep the named state volume unchanged. Do not run `docker compose down --volumes` during an update.

Compose `.env` controls variable interpolation, including `TRM_VERSION`; it does not automatically inject arbitrary secrets into the container. Add a protected service `env_file` or explicit `environment` mapping for YAML `${NAME}` references.

The provided listener is reachable at `monitor:9090` only from the Compose network. No host port is published. If Prometheus runs on the host, explicitly publish `127.0.0.1:9090:9090`; do not expose this unauthenticated endpoint to the public internet. [Observability setup](observability.md).

## Validation during installer updates

The Linux installer validates the candidate using systemd's parser for the standard `/etc/tokenresetsmonitor/environment` file when present. This requires `systemd-run` on a systemd host. It does not execute the environment file as shell code.

The Windows installer temporarily includes the service's `Environment` registry assignments while validating the candidate, then restores the installer's environment. If you use additional custom service wrappers or environment drop-ins, verify the candidate under that same environment before replacing the executable.

The installers run `config validate --structural` before replacement and after copying. Unknown fields, unsupported schemas, and invalid literal settings still fail. References to unavailable environment variables are reported as deferred requirements, allowing `--no-start` / `-NoStart` to finish before secrets are provisioned. This check does not certify runtime readiness: ordinary `config validate` and daemon startup still require the complete service environment.

The installers preserve existing configuration/state and make stopped backups. Paths outside the installer-managed state directory require your own backup. Follow [upgrading.md](upgrading.md) before changing a release.
