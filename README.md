# TokenResetsMonitor

[![CI](https://github.com/DKPlugins/TokenResetsMonitor/actions/workflows/ci.yml/badge.svg)](https://github.com/DKPlugins/TokenResetsMonitor/actions/workflows/ci.yml)

Monitor public AI quota reset announcements from [TokenResets](https://tokenresets.com/api/) and call your webhook, send a Telegram notification, or use both. A single Go executable runs in a terminal, as a Windows service, under Linux systemd, or in Docker.

**This monitors public announcements, not your account's remaining tokens.** An announcement does not confirm that your personal limits have reset. Announced, published, effective, and detected times may differ; unknown scope stays unknown. See the source's [methodology](https://tokenresets.com/methodology/). This is an independent project and is not affiliated with TokenResets, OpenAI, Anthropic, or Telegram.

The initial release line is **`v1.0.0-rc.4`**. Stable `v1.0.0` follows the [release acceptance checklist](docs/releasing.md). Download artifacts from [GitHub Releases](https://github.com/DKPlugins/TokenResetsMonitor/releases); if a candidate has not been published yet, use the source build below.

## Features

- Poll selected providers every 30 minutes by default, with per-provider plan, product, and reset-window filters.
- Require a minimum confidence level and select event types; only `hard_reset` is enabled by default.
- Persistent history and independent delivery queues survive restarts and updates.
- First startup establishes history without sending a backlog of old events.
- Configurable webhook method, headers, JSON payload or body template; independent Telegram support.
- Test each destination with a synthetic `TEST` event, or preview a request without sending it.
- Structured, redacted logs, rotating files, and local diagnostic ZIP export while the monitor runs.
- Versioned configuration, database, and webhook format, with migration backups and explicit updates.

## Quick start

Extract the archive for Windows x64, Linux x64, or Linux ARM64. No Go installation is needed for release binaries.

```sh
tokenresetsmonitor init --config ./config.yaml
tokenresetsmonitor config validate --config ./config.yaml
tokenresetsmonitor test-notification all --config ./config.yaml
tokenresetsmonitor run --config ./config.yaml
```

On Windows, use `./tokenresetsmonitor.exe` in PowerShell. The wizard downloads the provider catalog and prompts for filters and notification destinations. It refuses to overwrite an existing file. If the API is unavailable, enter provider slugs manually and validate them against the source when it recovers.

For unattended setup, `init --defaults --config ./config.yaml` writes a default configuration without contacting the API. Both notification channels start disabled unless enabled through environment overrides; monitoring and event logging still work. `test-notification all` requires at least one enabled channel. You can test a named channel before enabling it.

`run --once` performs one complete scan and delivery pass, then exits. Continuous operation polls immediately, then at the configured interval. Stop a foreground process with Ctrl+C.

Use `--help` for the command list and `version --json` for machine-readable build information. Runtime CLI overrides are `--state-path`, `--poll-interval`, and `--log-level`; `--config` selects the file. Service-management and configuration-migration commands use the saved settings and reject runtime overrides.

## Install as a service

### Linux: systemd or foreground

The installer supports Linux amd64/arm64 and requires root, `curl`, `tar`, `sha256sum`, `fuser` (usually the `psmisc` package), and standard account-management tools. Download the script for the desired version and inspect it before running:

```sh
curl -fL https://github.com/DKPlugins/TokenResetsMonitor/releases/download/v1.0.0-rc.4/install.sh -o install.sh
less install.sh
sudo sh install.sh --version v1.0.0-rc.4 --no-start
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
Invoke-WebRequest https://github.com/DKPlugins/TokenResetsMonitor/releases/download/v1.0.0-rc.4/install.ps1 -OutFile install.ps1
Get-Content ./install.ps1
./install.ps1 -Version v1.0.0-rc.4 -NoStart
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

For manual installation, place the executable and configuration in permanent locations, grant LocalService the permissions above, then run `tokenresetsmonitor service install --config C:\absolute\config.yaml`. The service stores absolute, correctly quoted paths. Its working directory is not your shell's working directory. Process environment variables set in PowerShell are not inherited by SCM; use the protected configuration file or explicitly configure the service's environment through Windows administration.

## Docker

Copy `config.example.yaml` to `config.yaml`, edit it, and use the supplied `compose.yaml`:

```sh
cp config.example.yaml config.yaml
# The non-root container user must be able to read the bind-mounted file.
sudo chown "$(id -u):10001" config.yaml
chmod 640 config.yaml
docker compose up -d
docker compose logs --tail 100 monitor
docker compose exec monitor tokenresetsmonitor status --config /config/config.yaml
```

The image supports `linux/amd64` and `linux/arm64`, runs as UID/GID `10001`, exposes no ports, and makes only outbound requests. Compose mounts configuration read-only and stores history in the persistent `monitor-data` volume. It uses JSON container logs with Docker rotation; application file logs are disabled by default. A read-only root filesystem is supported.

Set `TRM_VERSION` in your shell or Compose `.env` file to an exact tag for upgrades. Do not delete the data volume during upgrades. For a local source build:

```sh
docker build -t tokenresetsmonitor:local .
docker run --rm tokenresetsmonitor:local version
```

Add secret variables to the Compose service's `environment` or an `env_file` if you use `${NAME}` references in YAML. Compose's `.env` file alone does not pass arbitrary variables into the container. Keep secret files out of Git.

## Configuration reference

See [config.example.yaml](config.example.yaml) for a complete file. Precedence is **CLI → environment → YAML → defaults**. Restart to apply changes. Relative `state_path` and log directory values resolve relative to the configuration file. Without `--config` or `TRM_CONFIG`, the default is the operating system's user configuration directory under `TokenResetsMonitor/config.yaml`.

| Setting | Default / behavior |
| --- | --- |
| `config_version` | `1`; explicit file-format version |
| `api_base_url` | `https://tokenresets.com/api/v1` |
| `poll_interval` | `30m`; minimum `1m` |
| `request_timeout` | `30s`; per API request |
| `state_path` | `state.db` under the default user configuration directory; installer/example overrides differ |
| `providers` | Explicit `openai-codex` and `anthropic-claude` entries |
| `providers[].plans/products/windows` | Empty lists accept all known or unknown values in that dimension |
| `event_types` | `[hard_reset]` |
| `minimum_confidence` | `reported` |
| `unknown_scope` | `include`; use `exclude` to reject unknown values in a dimension you filter |
| `webhook.enabled` | `false` |
| `webhook.url` | Destination HTTP(S) URL; required when enabled or explicitly tested |
| `webhook.method` | `POST` |
| `webhook.headers` | Header-name/value mapping; empty by default |
| `webhook.body_template` | Empty sends the standard JSON notification |
| `webhook.timeout` | `15s` |
| `telegram.enabled` | `false` |
| `telegram.bot_token`, `telegram.chat_id` | Required when enabled or explicitly tested |
| `telegram.message_thread_id` | `0`, meaning no topic selection |
| `telegram.disable_notification` | `false`; set `true` for silent delivery |
| `telegram.timeout` | `15s` |
| `telegram.api_base_url` | `https://api.telegram.org`; override for a test server or your Bot API server |
| `logging.level` | `info`; also `debug`, `warn`, `error` |
| `logging.format` | `text` for console; `json` in installer/container configurations |
| `logging.file_enabled` | `false`; installers enable it |
| `logging.directory` | `logs` under the default user configuration directory |
| `logging.max_size_mb` | `10` MiB per active file |
| `logging.max_backups` | `5` compressed archives |
| `logging.max_age_days` | `14` days |

Provider and scope choices come from the API catalog; new providers are never silently enabled. Filters use OR within a list and AND across dimensions. `any-paid` covers paid plans; explicit `free` does not match that wildcard. No plan/product/window is inferred from announcement prose.

Confidence levels, in increasing order: `unverified`, `probable`, `reported`, `verified`, `official`. Additional event types include `banked_reset_grant`, `quota_policy_change`, `temporary_multiplier`, and `scheduled_reset`; notifications retain their actual type instead of describing every event as a completed reset.

Environment names use `TRM_` plus uppercase YAML keys separated by underscores, for example:

```sh
export TRM_POLL_INTERVAL=15m
export TRM_TELEGRAM_BOT_TOKEN='your-token'
export TRM_PROVIDERS='[{slug: openai-codex, plans: [pro], products: [codex], windows: [weekly]}]'
export TRM_EVENT_TYPES='[hard_reset, banked_reset_grant]'
tokenresetsmonitor run --config ./config.yaml --log-level debug
```

Collections are parsed as YAML and replace the corresponding list or map rather than merging it; for example, `TRM_WEBHOOK_HEADERS='{}'` removes all configured custom headers. Unknown provider-filter keys are rejected. Any parsed string can contain `${NAME}` references; an unset referenced variable is an error. For example, `Authorization: "Bearer ${WEBHOOK_TOKEN}"` keeps the secret outside your YAML. The wizard preserves references. `init --defaults` saves effective nonsecret overrides, while webhook URL/template and Telegram token/chat ID overrides are saved as `${TRM_...}` references. `TRM_WEBHOOK_HEADERS` is a runtime override and is not saved. Keep these secret environment variables available to the actual service/container after initialization.

The `config_version` field is required. An unversioned configuration must first pass through `config migrate`; it is not silently interpreted as the latest format. API base URLs must not contain a query or fragment, and all configured HTTP URLs must omit embedded username/password credentials. Supply webhook authentication through headers or supported query parameters instead.

## Webhook payload and templates

The default request is `POST application/json`. Its stable top-level fields are `schema_version`, `notification_id`, `detected_at`, `test`, and `event`. The event contains the source ID/revision, provider, type, status, title/summary, source timestamps, scope, confidence, and source links. See the [webhook contract](docs/architecture.md#webhook-contract).

The `Idempotency-Key` header is the notification ID and is stable across retries. Return HTTP `2xx` to acknowledge receipt. Redirects are not followed. Transport and protocol headers such as `Host`, `Content-Length`, `Idempotency-Key`, and `X-TokenResetsMonitor-Test` are reserved.

A custom JSON body can use Go `text/template` and the `json` function for escaping:

```yaml
webhook:
  enabled: true
  url: "https://example.com/hooks/reset"
  method: POST
  headers:
    Authorization: "Bearer ${WEBHOOK_TOKEN}"
  timeout: 15s
  body_template: |
    {"id": {{json .ID}}, "test": {{json .Test}}, "title": {{json .Event.Title}}, "event": {{json .Event}}}
```

Templates receive the notification struct; use `.ID`, `.DetectedAt`, `.Test`, `.Event.Provider.Slug`, etc. Templates cannot run shell commands. Template output is limited to 1 MiB. Set an appropriate `Content-Type` header if you deliberately render a non-JSON body. Custom templates define their own body contract; include `.Test` so test requests are recognizable in your application.

## Telegram

Create a bot through [BotFather](https://core.telegram.org/bots/features#creating-a-new-bot), send it a message or add it to the intended group, and obtain the destination chat ID. Supply `bot_token` and `chat_id`; use `message_thread_id` for a forum topic. Sending uses [Bot API `sendMessage`](https://core.telegram.org/bots/api#sendmessage) and requires no inbound HTTP endpoint.

For a new bot, send `/start` in the intended private conversation, or a command addressed to the bot in its group. Read the bot's [getUpdates response](https://core.telegram.org/bots/api#getupdates) and copy `message.chat.id` into `chat_id` as a quoted string, preserving any minus sign. A message sent within a forum topic also supplies `message_thread_id`. `getUpdates` is unavailable while that bot has an incoming webhook configured; use a dedicated bot or obtain the chat/topic IDs from the bot's existing update handler. Run `test-notification telegram` to confirm the selected destination.

Messages include the provider, event type, confidence, scope, dates, source link, and the distinction between a public announcement and personal account limits. A Telegram HTTP `200` response counts as success only when its JSON contains `ok: true`.

## Test notification delivery

```sh
tokenresetsmonitor test-notification webhook --config ./config.yaml
tokenresetsmonitor test-notification telegram --config ./config.yaml
tokenresetsmonitor test-notification all --config ./config.yaml
tokenresetsmonitor test-notification webhook --dry-run --config ./config.yaml
```

**Without `--dry-run`, a test really calls the configured destination and may trigger your automation.** Each test uses a unique synthetic ID and a `TEST` marker; webhook tests include `X-TokenResetsMonitor-Test: true`. The standard JSON also includes `test: true`. Test dispatch shares production rendering, headers, authorization, and HTTP clients.

Tests make one attempt per channel with no retries, do not contact TokenResets, and do not change history or delivery queues. `all` tests enabled channels; an explicit channel can be tested while disabled if its required settings exist. `--provider SLUG` selects the synthetic provider. Results show the channel, HTTP status, duration, and a sanitized error when applicable.

Dry-run renders without any network calls and prints only the method, redacted destination, header names, and body byte count. It does not print credentials, header values, or arbitrary request bodies.

Exit codes: `0` success, `1` delivery/runtime failure, `2` invalid configuration or arguments. For `all`, a destination or template error in one channel does not prevent the other channel's test. A configuration error takes precedence over delivery failures in the final exit code.

## Delivery and history guarantees

Every cycle traverses all pages for each selected provider using its dedicated `/providers/{slug}/events` endpoint. Each page has its own cached body and ETag. A `304` on page one does not skip later pages. This detects announcements published late or backdated at the source.

The first successful **complete** scan of a provider establishes history and sends nothing. A failed page does not complete that provider's baseline. New providers and newly enabled channels also start without historical notifications. Widening filters does not replay unchanged old records. A later source revision can make a previously filtered new event eligible, but an already delivered event is not announced as another reset.

History and queue insertion are transactional in a local bbolt database. Run only one monitor per state file, on a local filesystem. Webhook and Telegram deliveries have independent queues. Network errors, `429`, and `5xx` retry with backoff capped at 30 minutes; a longer `Retry-After` is honored up to a seven-day safety limit. Permanent failures remain visible through `status`.

After correcting a permanent failure, stop the monitor and requeue eligible failed deliveries:

```sh
tokenresetsmonitor deliveries retry-failed --config ./config.yaml
```

Changing filters cancels pending deliveries that no longer match. Changing a recipient cancels its old queue; changing credentials preserves it. Confirmed source withdrawals cancel pending deliveries, but disappearance from a list alone is not proof of withdrawal. Separate correction/retraction notifications are deferred to a later version.

Webhook recipient identity consists of its URL with recognized credential query parameters removed; header values do not define the recipient. Changing the URL path therefore counts as a recipient change, including when a provider embeds a token in that path. Telegram recipient identity uses the API address, chat ID, and topic ID, independently of the bot token. If custom headers or a body template route messages to different recipients, resolve pending deliveries before changing that routing.

A crash after the destination accepts a request but before local confirmation may cause a repeat. Webhook receivers should persist and deduplicate `Idempotency-Key`. Telegram provides no equivalent idempotency key, so occasional duplicates are possible. Never delete the state file as an update procedure: doing so starts a new baseline and loses queued work.

## Logs and diagnostics

Logs use UTC and structured fields such as `run_id`, `cycle_id`, `event_id`, `notification_id`, `provider`, and `channel`. They record startup/shutdown, poll results, delivery attempts, failures and recovery. Filtering reasons are at `debug`. Tokens, authorization values, and secret URL parts are redacted at every level; raw request/response bodies are excluded.

File logging always writes JSON lines to `tokenresetsmonitor.jsonl`, even when console output is text. It rotates at 10 MiB by default, compresses archives, and keeps at most five archives for up to 14 days. Only the daemon writes these files; commands such as test-notification print their own results separately.

A single log record is limited to 1 MiB. File or console write failures are reported separately on stderr and make the run finish with an error. Export skips oversized input records, retains following valid records, and records the omission in its manifest.

```sh
tokenresetsmonitor status --config ./config.yaml
tokenresetsmonitor status --json --config ./config.yaml
tokenresetsmonitor logs export --since 24h --output diagnostics.zip --config ./config.yaml
```

The ZIP includes available redacted logs, build information, and an operational status snapshot. It excludes configuration, environment, the state database, and message bodies. Export works while the monitor is running, records missing periods in `manifest.json`, refuses to overwrite an existing archive, and never uploads anything. Its snapshot may be stale after an unclean shutdown; `status` reports the snapshot age.

Diagnostic commands also load and validate the configuration. Run them with the same required environment references as the service; an interactive `sudo` command does not automatically import systemd's `EnvironmentFile`. An invalid enabled destination must be corrected before using `status` or `logs export`.

For systemd logs, request the raw JSON message instead of journald's wrapper:

```sh
sudo journalctl -u tokenresetsmonitor --since '24 hours ago' -o cat --no-pager |
  sudo tokenresetsmonitor logs export --input - --since 24h --output diagnostics.zip --config /etc/tokenresetsmonitor/config.yaml
```

For Docker, pass unprefixed logs to the container's export command and copy the result out:

```sh
docker compose logs --no-log-prefix --no-color --since 24h monitor |
  docker compose exec -T monitor tokenresetsmonitor logs export --input - --since 24h --output /data/diagnostics.zip --config /config/config.yaml
docker compose cp monitor:/data/diagnostics.zip ./diagnostics.zip
```

`--input structured-logs.jsonl` also accepts a file. Plain text or prefixed log lines are skipped and counted. Export archives are created with owner-only permissions on Linux. On Windows, a protected ACL grants access only to the creating user, SYSTEM, and Administrators, even in a shared output directory; filesystems that cannot preserve that protection are refused. Review diagnostic archives before attaching them to a public issue.

## Updates, development, and troubleshooting

Updates are explicit; the monitor never replaces itself. Use [the upgrade and rollback guide](docs/upgrading.md). Application versions follow [Semantic Versioning](https://semver.org/); configuration, state, and standard webhook JSON each have independent schema versions. Published release artifacts and version tags are never replaced.

Build and test from source with Go 1.26 or newer:

```sh
go test ./...
go vet ./...
go build -trimpath ./cmd/tokenresetsmonitor
```

CI runs race tests on Linux/Windows, native service lifecycle checks, and Docker amd64/arm64 smoke checks. Tests use local HTTP fixtures rather than real recipient tokens. See [CONTRIBUTING.md](CONTRIBUTING.md) and the [architecture notes](docs/architecture.md).

| Symptom | Check |
| --- | --- |
| No notification on first start | Expected: the first complete scan creates the baseline; use a TEST command to verify delivery. |
| No notification for an announcement | Check provider/type/confidence/scope filters and provider readiness in `status`; use debug logs for filter reasons. |
| State database is in use | Stop the other process, service, or container using that state file. `status` and log export work without its write lock. |
| Telegram rejected the request | Verify token, chat ID, group permissions, and topic ID; send a test after correcting settings. |
| Service cannot access configuration/state | Check absolute paths and LocalService/systemd-user directory permissions. |
| Provider poll fails | Check outbound HTTPS, source availability, and sanitized logs; no partial baseline is committed. |
| A configured secret reference is unset | Set it in the actual service/container environment, then restart. |
| Export is incomplete | Logs may have rotated out or file logging may be disabled; import available Docker/journald JSON logs. |

Licensed under [MIT](LICENSE).
