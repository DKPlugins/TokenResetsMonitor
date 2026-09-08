# TokenResetsMonitor

[![CI](https://github.com/DKPlugins/TokenResetsMonitor/actions/workflows/ci.yml/badge.svg)](https://github.com/DKPlugins/TokenResetsMonitor/actions/workflows/ci.yml)

Get notified when [TokenResets](https://tokenresets.com/api/) publishes an AI quota reset announcement. Choose the providers and plans you care about, then receive alerts in **Telegram, Slack, or your own webhook**. Run one small Go program in a terminal, as a Windows/Linux service, or in Docker.

**These are public announcements, not a meter for your account's remaining tokens.** An announcement does not confirm that your personal limits have reset. Unknown scope stays unknown; announcement, effective, and detection times are shown separately. Read the source's [methodology](https://tokenresets.com/methodology/). This independent project is not affiliated with TokenResets, OpenAI, Anthropic, Telegram, or Slack.

This checkout is the **1.1.0-rc.1 release candidate**. Slack, assisted setup, live reload, metrics, health checks, and update checking described here require this version. The earlier published stable [v1.0.0 release](https://github.com/DKPlugins/TokenResetsMonitor/releases/tag/v1.0.0) has webhook/Telegram monitoring; its completed checks are preserved in the [historical acceptance record](docs/acceptance.md). This candidate is intended for testing before the stable 1.1 release; publication and platform acceptance are tracked in the [release guide](docs/releasing.md).

## What it does

- Filters announcements by provider, plan, product, reset window, event type, and confidence.
- Polls immediately and then every 30 minutes by default. First startup quietly records existing history.
- Keeps independent durable delivery queues; a failing destination does not block another.
- Connects Telegram with a guided bot pairing flow and supports Slack incoming webhooks.
- Applies operational YAML changes while running, retaining the last valid configuration on errors.
- Exposes optional Prometheus metrics and separate liveness/readiness checks.
- Checks project releases and source API compatibility without installing updates.
- Produces redacted structured logs and local diagnostic ZIP files.

## Example alert

This compact illustration is synthetic; it is not a report of an actual reset.

```text
EXAMPLE — public reset announcement
Provider: OpenAI Codex | Event: hard_reset
Scope: Codex / Pro / weekly | Confidence: reported
Announced: 2026-09-08 10:00 UTC | Effective: unknown
Detected: 2026-09-08 10:05 UTC
Source: https://tokenresets.com/ — personal account limits are not confirmed
```

## Start in a few minutes

To try this release candidate, build from the source checkout with Go 1.26 or newer:

```sh
go build ./cmd/tokenresetsmonitor
./tokenresetsmonitor init --config ./config.yaml
./tokenresetsmonitor run --config ./config.yaml
```

On Windows PowerShell, use `./tokenresetsmonitor.exe`. The wizard guides provider selection and notification setup. Existing files are preserved; use a `setup` command to connect or change a destination later.

```sh
./tokenresetsmonitor setup telegram --config ./config.yaml
# Or connect Slack:
./tokenresetsmonitor setup slack --config ./config.yaml
```

Telegram setup checks the bot, offers a pairing link, and discovers the destination/chat topic. Create the bot in [BotFather](https://core.telegram.org/bots/features#creating-a-new-bot) first. Slack setup accepts an incoming webhook created for your workspace/channel. Secrets are hidden in terminal prompts; `${ENV_NAME}` references are supported. [Notification setup and examples →](docs/notifications.md)

On first startup, a complete provider scan creates the baseline **without sending old announcements**. Verify delivery whenever you are ready:

```sh
./tokenresetsmonitor test-notification all --config ./config.yaml
./tokenresetsmonitor status --json --config ./config.yaml
```

A test sends a clearly marked synthetic notification to enabled destinations. Add `--dry-run` to preview without contacting them. Stop the foreground process with Ctrl+C. For monitoring without notifications, `init --defaults` starts with all channels disabled.

## Docker

From the source checkout:

```sh
mkdir -p config
cp config.example.yaml config/config.yaml
# On Linux, allow the non-root container user to read the directory and file.
sudo chown "$(id -u):10001" config config/config.yaml
chmod 750 config
chmod 640 config/config.yaml
docker compose up -d --build
docker compose ps
docker compose logs --tail 100 monitor
```

Edit `config/config.yaml` to select providers and destinations. Mounting the directory keeps atomic editor saves visible to live reload. Docker Desktop users can create/copy the same files in PowerShell and omit the Linux ownership commands.

The container runs as UID/GID `10001` with a read-only root filesystem. History is stored in the named `monitor-data` volume. Health checks work without an HTTP port. Metrics listen on port `9090` inside the Compose network; no host port is published. [Scraping metrics and interpreting health →](docs/observability.md)

Once the candidate is published, set `TRM_VERSION=v1.1.0-rc.1`, then use `docker compose pull` and `docker compose up -d --no-build`. The default `local` image is built from this checkout. Preserve the data volume when updating.

## Commands you will use

| Command | Purpose |
| --- | --- |
| `init` / `init --defaults` | Create a configuration, interactively or unattended |
| `setup telegram` / `setup slack` | Connect a destination; preserve other configuration |
| `config validate` | Check settings without sending anything |
| `config migrate [--apply]` | Preview or explicitly save a file-format migration |
| `run [--once]` | Run continuously or perform one scan/delivery pass |
| `status [--json]` | Inspect the daemon's latest operational snapshot |
| `healthcheck --state-path PATH [--ready]` | Probe liveness or readiness without loading YAML |
| `doctor [--offline] [--json]` | Diagnose configuration, platform, snapshot, and source API compatibility |
| `check-update [--json]` | Check published project releases; never install |
| `test-notification webhook\|telegram\|slack\|all` | Send one synthetic TEST per selected destination |
| `logs export --since 24h --output diagnostics.zip` | Create a redacted local support archive |
| `deliveries retry-failed` | Requeue eligible failed deliveries while stopped |
| `service install\|start\|stop\|status\|uninstall` | Manage the native Windows service |
| `version [--json]` | Show build information |

Use `--config PATH` for configuration-dependent commands. The examples use a binary in the current directory; omit `./` when it is on your PATH. Exit codes are `0` for success, `1` for runtime/check failure, and `2` for invalid arguments/configuration. Doctor reports diagnostic failures as `1`; warnings remain visible without failing the command. An available update is a successful check, not an error.

## Configuration and live reload

[config.example.yaml](config.example.yaml) lists every setting with defaults and hints. Precedence is **CLI → environment → YAML → defaults**. Relative state/log paths resolve against the configuration directory.

The daemon automatically reloads operational settings: polling/request timeouts, providers and filters, destinations and credentials, notification options, log level, and update-check preferences. Invalid or infrastructure-changing edits are rejected as a whole; the current configuration continues to run. Check `status --json`, logs, or metrics to see the active generation and reload result.

Changing state location, source API origin, metrics listener, or logging output/rotation requires a restart. Environment changes also require restart: editing a shell, service environment file, or Compose `env_file` cannot modify an already running process's environment. [Complete configuration and reload rules →](docs/configuration.md)

## Installation, diagnosis, and updates

- [Windows service, Linux systemd, and Docker installation](docs/installing.md)
- [Telegram, Slack, and webhook setup](docs/notifications.md)
- [Metrics, liveness/readiness, and alert examples](docs/observability.md)
- [Upgrading, compatibility checks, backups, and rollback](docs/upgrading.md)
- [Architecture, delivery guarantees, and webhook contract](docs/architecture.md)
- [Contributing and local verification](CONTRIBUTING.md)

`doctor --config ./config.yaml` makes read-only requests to the source API to validate its contract and your provider choices. `doctor --offline` makes no network requests. Neither sends notifications, modifies history, or migrates a database.

Background release checks run every 24 hours by default. Set `updates.enabled: false` to disable them. A failed check is reported as unknown/failure, not proof that you are up to date. Stable releases are selected unless `updates.include_prerelease: true` is set. No update is downloaded or installed automatically.

| Symptom | Next step |
| --- | --- |
| No alert on first startup | Expected baseline behavior; send a TEST to confirm delivery |
| A new announcement produces no alert | Inspect provider/type/confidence/scope filters and provider readiness |
| Telegram pairing cannot find a chat | Send the pairing command to the bot/topic; check webhook conflicts or use manual IDs |
| Slack returns an error | Check the webhook's channel/workspace and whether the app was removed |
| Configuration edit has no effect | Inspect reload error; check environment/CLI overrides and restart-only fields |
| Container is live but not ready | Check source availability and complete provider baselines; an outage should not cause restart loops |
| Database is in use | Stop the other daemon using this state file; diagnostic snapshots do not need its write lock |
| Update compatibility is unknown | Older releases may lack metadata; inspect release notes and keep a stopped backup |
| A secret reference is unset | Provide it to the actual service/container environment and restart |
| Service cannot access files | Check its LocalService/systemd account and configured directory permissions |
| Windows setup refuses configuration permissions | Restrict legacy shared ACLs using Windows Security settings or the installer; the original file and private backup are preserved |

To share diagnostics, run `logs export` and review the resulting archive before attaching it to an issue. Exports exclude raw configuration, environment, state databases, and HTTP bodies, and are never uploaded automatically. See the [diagnostics guide](docs/diagnostics.md).

Licensed under [MIT](LICENSE).
