# Configuration and live reload

See [config.example.yaml](../config.example.yaml) for a complete file. Precedence is **CLI → environment → YAML → defaults**. Operational settings reload automatically; restart-only fields are listed below. Relative `state_path` and log directory values resolve relative to the configuration file. Without `--config` or `TRM_CONFIG`, the default is the operating system's user configuration directory under `TokenResetsMonitor/config.yaml`.

| Setting | Default / behavior |
| --- | --- |
| `config_version` | `2`; version 1 files are accepted and normalized in memory |
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

The `config_version` field is required. Version 1 files remain readable without being rewritten. Use `config migrate` to preview and explicitly save version 2. Unversioned files require explicit migration; future unknown versions are refused. API base URLs must not contain a query or fragment, and all configured HTTP URLs must omit embedded username/password credentials. Supply webhook authentication through headers or supported query parameters instead.

## Added settings

| Setting | Default / behavior |
| --- | --- |
| `slack.enabled` | `false` |
| `slack.webhook_url` | HTTPS incoming webhook; treated as a secret |
| `slack.timeout` | `15s` |
| `reload.enabled` | `true`; watch the configuration for operational changes |
| `observability.enabled` | `false`; Compose explicitly enables it |
| `observability.listen` | `127.0.0.1:9090`; host:port, no public exposure by default |
| `updates.enabled` | `true`; periodic read-only release checks |
| `updates.interval` | `24h` |
| `updates.include_prerelease` | `false`; stable release channel |

All settings also accept their corresponding `TRM_...` environment overrides, for example `TRM_SLACK_WEBHOOK_URL` or `TRM_UPDATES_ENABLED=false`.

## Applying edits

The daemon checks for changes automatically, validates the complete effective configuration, waits for current requests to finish, reconciles pending work, and switches generations together. A partially written YAML file, unknown field, missing secret, invalid destination, or restart-only change leaves the last valid generation active. Saving the corrected file allows a later retry. Status exposes the generation, last reload result, and whether draining is in progress.

Reloadable settings include providers/filters, poll and request timeouts, channel enablement and destination credentials/options, log level, and update-check preferences. Provider additions and newly enabled channels establish baselines; changing settings never replays old unchanged events. Queue entries for disabled or changed recipients are canceled; rotating Telegram credentials for the same chat/topic retains pending identity. A Slack incoming webhook URL identifies its destination, so changing it cancels the old queue.

These fields require restart: `state_path`, `api_base_url`, `observability.*`, and logging fields other than `logging.level`. A reload containing any such change is rejected as a whole. Re-enabling reload after `reload.enabled: false` requires restarting the daemon.

The environment of the running process is fixed. YAML secret references are reevaluated against that same environment on reload; edited systemd environment files, Windows service environment, or Compose `env_file` require a service/container restart. Explicit CLI and environment overrides retain precedence across reloads.

For Docker, mount a configuration directory, not a single file, so atomic editor saves remain visible. Setup commands use private backups and atomic replacement; ordinary editors must preserve readable permissions.

## Version and storage boundaries

New files use configuration schema 2. State schema 2 adds durable channel cooldowns and migrates existing supported state with a backup. The standard webhook payload remains schema 1. An older 1.0 binary cannot read state schema 2: rollback requires its matching stopped backup. See [upgrading.md](upgrading.md).

## Custom webhook templates

`webhook.body_template` uses Go `text/template` and is executed during offline validation against representative notifications. Rendering is limited to 1 MiB of output and 10,000 execution steps, including empty loops and recursive templates. Intermediate formatting, width, and precision are bounded before allocation. Keep templates small and guard optional dates before calling date methods; an unknown effective reset time is valid input. Use the `json` helper when inserting text into a JSON body, and run `test-notification webhook --dry-run` before enabling the destination. See [notification examples](notifications.md#custom-webhook).
