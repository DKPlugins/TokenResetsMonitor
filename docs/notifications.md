# Connect notification destinations

Choose Telegram, Slack, a custom webhook, or any combination. Each destination has an independent durable queue. Setup does not send a message unless you explicitly accept its TEST prompt.

## Telegram: guided pairing

1. Create a bot with [BotFather](https://core.telegram.org/bots/features#creating-a-new-bot). Keep its token private.
2. Run `tokenresetsmonitor setup telegram --config ./config.yaml`.
3. Paste the token into the hidden terminal prompt, use `${BOT_TOKEN}`, or press Enter to retain the configured value.
4. Follow the pairing link in the intended private chat or add the bot to your group. The wizard recognizes the unique pairing command and proposes the destination.
5. Confirm the destination. Optionally send a TEST, then save.

For a forum topic, send the displayed `/trm_connect@bot <pairing-code>` command inside that topic; its topic ID is discovered automatically. Group pairing also requires the bot to receive the displayed addressed command. The random pairing code ties discovery to your setup session instead of choosing an arbitrary old chat update.

The wizard first checks `getMe` and `getWebhookInfo`. An existing bot webhook is left intact; choose manual chat/topic IDs or use a dedicated bot. Update discovery uses `getUpdates`, acknowledges processed bot updates, and asks before polling. Avoid sharing a bot with another long-polling application. A conflicting poller (HTTP 409) falls back to manual setup. Discovery stops after five minutes.

Manual IDs remain available when discovery is unsuitable. Keep negative group chat IDs quoted, for example `chat_id: "-1001234567890"`. Set `message_thread_id` for a topic; zero means no topic. See the official [Bot API](https://core.telegram.org/bots/api) for bot permissions and update delivery.

Saving enables Telegram, updates only its YAML section and the file version, creates a private backup, and preserves other settings and environment references. Concurrent file edits abort the save. Environment overrides still win; the wizard warns about that. Restart if you change the environment itself.

## Slack: incoming webhook

Create or select a Slack app, enable incoming webhooks, and add a webhook to the intended workspace/channel using Slack's [incoming webhook setup](https://docs.slack.dev/messaging/sending-messages-using-incoming-webhooks/).

```sh
tokenresetsmonitor setup slack --config ./config.yaml
```

Paste the HTTPS webhook URL into the hidden prompt or enter `${SLACK_WEBHOOK_URL}`. Confirm the destination, optionally send a TEST, and save. The URL is a secret; it is omitted from previews, logs, and metrics. The saved configuration can look like:

```yaml
slack:
  enabled: true
  webhook_url: "${SLACK_WEBHOOK_URL}"
  timeout: 15s
```

The webhook belongs to its configured Slack channel. Create another webhook in Slack to change that destination. Changing the URL cancels queued deliveries for the old destination. Messages escape source text and suppress automatic mention parsing. Slack acknowledges delivery only with HTTP 200 and an `ok` response; other statuses or response bodies remain failures, and error bodies are not printed.

Slack retries are at least once, so a crash after Slack accepts a message but before local acknowledgement may produce a duplicate. Rate limits pause the destination queue using a persisted cooldown, including across restarts.

## Custom webhook

Use the example below for an HTTP receiver you control:

```yaml
webhook:
  enabled: true
  url: https://example.com/hooks/reset
  method: POST
  headers:
    Authorization: "Bearer ${WEBHOOK_TOKEN}"
  body_template: ""
  timeout: 15s
```

The default JSON contains `schema_version: 1`, `notification_id`, `detected_at`, `test`, and the source `event`. A stable `Idempotency-Key` header lets receivers deduplicate retries. Return HTTP 2xx after accepting a request. Redirects are not followed; authentication is never forwarded to a redirect target.

A custom body uses Go `text/template` with a `json` escaping helper:

```yaml
body_template: |
  {"id": {{json .ID}}, "test": {{json .Test}}, "title": {{json .Event.Title}}}
```

Template output is limited to 1 MiB. Set a suitable Content-Type for non-JSON bodies. Templates do not execute shell commands. Include the TEST flag when downstream automation distinguishes test events. See the full [webhook contract](architecture.md#webhook-contract).

## Verify without affecting history

```sh
tokenresetsmonitor test-notification telegram --config ./config.yaml
tokenresetsmonitor test-notification slack --config ./config.yaml
tokenresetsmonitor test-notification webhook --dry-run --config ./config.yaml
tokenresetsmonitor test-notification all --config ./config.yaml
```

A real test sends a unique synthetic TEST event and makes one attempt per selected destination. It does not contact TokenResets, write history, or modify queues. A named destination can be tested while disabled. `all` selects enabled destinations and reports independent results. `--provider SLUG` changes the synthetic provider.

Dry-run performs no network requests and reports redacted destination, HTTP method, header names, and body size. It never prints tokens, header values, or the message body.

Operational changes reload automatically. Newly enabled destinations establish their own baseline without a historical backlog. Failed permanent deliveries can be requeued with `deliveries retry --id ID` or `deliveries retry-failed` while the daemon is running or stopped; only entries still eligible under the accepted configuration and current destination are retried. See [history and delivery management](history.md).

On Windows, setup preserves a supported private existing ACL and its LocalService read grant. Legacy files shared with Everyone/Users or unsupported principals are refused without rewriting the original; a private backup is retained. Restrict the file through Windows Security settings or use the installer-managed permissions before retrying. On Linux, replacement preserves the existing owner/group and service group-read permission while removing world access.

## Corrections and withdrawals

After a destination acknowledges an announcement, a meaningful source revision can produce a follow-up describing what changed: for example, `Plans: pro → plus, pro`, a changed effective date, or an explicit withdrawal. The comparison is against the version acknowledged by that destination. It is labeled as a correction or withdrawal so it cannot be mistaken for another quota reset. A revision number change with identical meaningful content does not generate a message. Disappearance from an event list alone is not a withdrawal.

Telegram and Slack use `notify_changes: true` by default. Disable it independently on either channel if wanted. Webhooks default to `notify_changes: false`; enable it only after the receiver understands `kind`, `previous_event`, `changes`, and `related_notification_id`. An absent `kind` remains an ordinary announcement. Each change delivery has its own stable idempotency key. Custom templates can use `.Kind`, `.PreviousEvent`, `.Changes`, and `.RelatedNotificationID`; guard the optional previous snapshot before accessing it.

Some migrated records cannot prove the exact version originally acknowledged. Such comparisons are marked unverified (`previous_event_unverified: true` in JSON) so a receiver does not treat reconstructed legacy detail as confirmed history.

Change messages follow the recipient that acknowledged the original announcement. They do not create a backlog for a newly enabled destination, and current plan filters do not hide a withdrawal of an announcement already sent there. Disabled or changed recipients receive no follow-up. Ordinary unsent announcements are still checked against the current filters and source status before delivery.
