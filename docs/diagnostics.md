# Logs and local diagnostics


Logs use UTC and structured fields such as `run_id`, `cycle_id`, `event_id`, `notification_id`, `provider`, and `channel`. They record startup/shutdown, poll results, delivery attempts, failures and recovery. Filtering reasons are at `debug`. Tokens, authorization values, and secret URL parts are redacted at every level; raw request/response bodies are excluded.

File logging always writes JSON lines to `tokenresetsmonitor.jsonl`, even when console output is text. It rotates at 10 MiB by default, compresses archives, and keeps at most five archives for up to 14 days. Only the daemon writes these files; commands such as test-notification print their own results separately.

A single log record is limited to 1 MiB. File or console write failures are reported separately on stderr and make the run finish with an error. Export skips oversized input records, retains following valid records, and records the omission in its manifest.

```sh
tokenresetsmonitor status --config ./config.yaml
tokenresetsmonitor status --json --config ./config.yaml
tokenresetsmonitor logs export --since 24h --output diagnostics.zip --config ./config.yaml
```

The ZIP includes available redacted logs, build information, and an operational status snapshot. It excludes configuration, environment, the state database, and message bodies. Export works while the monitor is running, records missing periods in `manifest.json`, refuses to overwrite an existing archive, and never uploads anything. Its snapshot may be stale after an unclean shutdown; use `healthcheck` to test freshness.

Status and log export load only their operational configuration needs when notification secrets are unavailable. The daemon and notification tests still require all referenced secrets. An interactive sudo command does not automatically import systemd's EnvironmentFile; doctor reports missing references with its other checks.

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


Doctor reports configuration failures with other useful checks instead of stopping at the first error. Its online mode reads source catalog/provider metadata and complete event pagination; `--offline` makes no network requests. A running daemon's schema is checked through its bounded status snapshot without taking the database lock. Missing or stale metadata is reported as unknown, never inferred by opening/migrating live state.

## When the configuration itself is broken

Use explicit absolute paths to inspect the existing daemon snapshot and logs even if YAML parsing or a notification secret fails. Substitute the installation's actual paths:

```sh
tokenresetsmonitor status --config ./broken.yaml --state-path /actual/state.db
tokenresetsmonitor healthcheck --state-path /actual/state.db
tokenresetsmonitor doctor --offline --config ./broken.yaml --state-path /actual/state.db
tokenresetsmonitor logs export --config ./broken.yaml --state-path /actual/state.db --log-directory /actual/logs --output diagnostics.zip
```

Doctor still reports the configuration problem; offline mode sends no network requests. An explicit path directs these read-only diagnostics to existing files and does not repair or migrate the database. For streamed logs, replace `--log-directory` with `--input -`.
