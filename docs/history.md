# History, missing notifications, and delivery management

Use these commands with the same `--config` as your daemon. They work while monitoring continues and also when it is stopped. Add `--json` for automation. JSON identifies whether the effective configuration came from the running daemon or the saved file; a rejected reload can make those configurations differ.

## Find an event and explain its outcome

```sh
tokenresetsmonitor history list --config ./config.yaml --provider openai-codex --limit 50
tokenresetsmonitor history show --config ./config.yaml --id EVENT_ID
tokenresetsmonitor history explain --config ./config.yaml --id EVENT_ID --json
tokenresetsmonitor history list --config ./config.yaml --since 2026-09-07T00:00:00Z --until 2026-09-08T00:00:00Z
```

The explanation separates the event's observed history from its outcome at each destination. An announcement can belong to the initial history, fail a provider/type/confidence/scope filter, be ineligible for a newly enabled destination, await its first attempt or a scheduled retry, fail permanently, be canceled, or already be acknowledged. Follow the delivery ID to inspect the specific attempts and next retry. A source event that has not been observed cannot be explained from local history; check provider readiness and the last successful scan.

Explanations show recorded decisions where available and distinguish them from evaluation against today's filters. Versions before 1.2 did not record every attempt or historical decision. Migrated data reports unavailable detail; it does not infer a successful attempt or a historical filter choice from current settings.

## Preview proposed filters

Save the proposed configuration as a separate file, leaving the daemon's current file in place:

```sh
tokenresetsmonitor filters preview --config ./config.yaml --candidate-config ./candidate.yaml --limit 50 --json
```

The preview compares active and proposed filters against retained real events. The candidate can contain a complete configuration or just filter settings. The `CHANGED` column (`changed` in JSON) identifies a changed match result or rejection reason, including events that remain rejected for a different reason. It reads local history; it does not poll TokenResets, save configuration, enqueue deliveries, or contact notification destinations. A filter match means the event satisfies those filters; baseline suppression and destination history can still prevent an actual notification. Applying the candidate remains a separate configuration edit.

## Inspect and retry deliveries

```sh
tokenresetsmonitor deliveries list --config ./config.yaml --status failed --channel telegram --json
tokenresetsmonitor deliveries list --config ./config.yaml --event-id EVENT_ID
tokenresetsmonitor deliveries show --config ./config.yaml --id DELIVERY_ID --json
tokenresetsmonitor deliveries retry --config ./config.yaml --id DELIVERY_ID
tokenresetsmonitor deliveries retry-failed --config ./config.yaml
```

Delivery details include recorded attempts and their outcomes, cancellation reasons, and the next eligible retry time, including destination cooldowns. Delivery states are `pending`, `failed`, `delivered`, and `canceled`; an active attempt marks pending work as in flight. Retrying preserves the notification's idempotency key and schedules an eligible failed delivery; it does not reset baseline history or force already delivered/canceled work to send again. A running daemon resumes sending without a restart. When stopped, start the monitor after requeuing.

The selected recipient, accepted configuration, and latest event revision are checked again when retrying. A disabled/changed destination, revoked event, or no-longer-eligible ordinary announcement is not blindly resent. Change messages are evaluated against the announcement that this destination previously acknowledged.

Lists accept `--provider`, `--status`, and `--since` (a positive duration such as `24h`, or an RFC3339 timestamp). History status filters accept `baseline`, `filtered`, `pending`, `failed`, `delivered`, `canceled`, `no_eligible_channel`, or `legacy_unknown`. An event summarizes its channel outcomes with pending work first, then failure, delivery, or cancellation; inspect its individual channels for the full picture. Delivery lists also accept `--event-id` and `--channel`. The default page size is 50; `--limit` accepts up to 200. Pass the returned opaque cursor as `--cursor` to continue with the same filters. Relative `--since` keeps the original page's cutoff when continuing with a cursor. Delivery attempt details are paginated as well.

Use `--until RFC3339` for an upper time bound; by default neither time bound is set. Date bounds also apply to history/delivery details and filter previews. The upper bound must not precede the lower bound. Explanations and retry commands do not accept date filters because they operate on the selected current event or delivery.

## Retention and local access

```yaml
history:
  retention_days: 0
```

Zero retains details indefinitely. A positive number limits old detailed history; the state retains the compact identity, baseline, and acknowledgement information needed to prevent replay and support future corrections. Pending work is preserved. Pruned detail is reported as unavailable rather than reconstructed. A retention change can be reloaded; take a stopped backup before shortening retention if you need to retain the old details elsewhere.

The running daemon serves management requests through a private local control endpoint. The CLI reads its protected control descriptor beside the state database. Run management commands as the daemon's account or an authorized administrator; a different account without the required file permissions cannot control the process. For a container, use `docker compose exec monitor tokenresetsmonitor ... --config /config/config.yaml`. No management port needs publishing, and the metrics listener does not expose these operations.

Stopped history/preview commands open existing state read-only and do not migrate it. Start the new daemon once to perform an upgrade before using new history features on an old database. A live control failure is an error, not permission to bypass the daemon's database lock. History intentionally displays source announcement details locally; exported diagnostics continue to exclude raw databases, credentials, and message bodies.
