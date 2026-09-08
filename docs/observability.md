# Metrics and health checks

The built-in endpoints expose operational state for external monitoring. They contain no notification bodies, credentials, recipient URLs, chat IDs, or per-event labels. Metrics use [Prometheus naming conventions](https://prometheus.io/docs/practices/naming/) and bounded operational labels.

## Enable the listener

For a native process, add:

```yaml
observability:
  enabled: true
  listen: 127.0.0.1:9090
```

Restart after changing the listener configuration. The default configuration leaves the HTTP listener disabled. The supplied Compose service explicitly listens on `0.0.0.0:9090` inside its private Docker network and does not publish a host port.

| Endpoint | Meaning |
| --- | --- |
| `GET /metrics` | Prometheus text exposition |
| `GET /livez` | Current process heartbeat is live |
| `GET /readyz` | Live and all configured providers have current successful baselines |

Health endpoints return JSON with `healthy` and machine-readable `reasons`. Success returns HTTP 200, failure returns 503. HEAD is supported; mutation methods are rejected. Health/scrape requests read cached operational state and never trigger source requests or test notifications.

The listener has no authentication or TLS. Keep it on loopback or a trusted container network. To scrape across networks, use your authenticated reverse proxy or monitoring network; publishing a public port is not necessary.

## Scrape from Prometheus

For Prometheus in the same Compose network:

```yaml
scrape_configs:
  - job_name: tokenresetsmonitor
    scrape_interval: 30s
    static_configs:
      - targets: ["monitor:9090"]
```

For Prometheus running on the same host as a native daemon, use `127.0.0.1:9090`. To reach a Docker daemon's endpoint from its host, explicitly add `ports: ["127.0.0.1:9090:9090"]` to a Compose override. Port publication is an administrator choice; the default stays private.

## Metrics

All names below have the prefix `tokenresetsmonitor_`.

| Name | Type / labels |
| --- | --- |
| `build_info` | Gauge, `version`, `commit` |
| `provider_scans_total` | Counter, `provider`, `result=success\|error` |
| `provider_scan_duration_seconds` | Histogram, `provider` |
| `provider_ready` | Gauge 0/1, `provider` |
| `provider_last_success_timestamp_seconds` | Unix-time gauge, `provider` |
| `delivery_attempts_total` | Counter, `channel`, `result=success\|retryable_error\|permanent_error` |
| `delivery_duration_seconds` | Histogram, `channel` |
| `deliveries` | Gauge, `channel`, `status=pending\|failed\|delivered\|canceled` |
| `oldest_pending_age_seconds` | Gauge, `channel` |
| `config_reloads_total` | Counter, `result` |
| `config_last_reload_successful` | Gauge 0/1 |
| `update_available` | Last known successful result, gauge 0/1 |
| `update_check_success` | Whether the latest check succeeded, gauge 0/1 |
| `update_last_check_timestamp_seconds` | Unix-time gauge |

Counters cover the current process lifetime and survive configuration reloads. Durable queue gauges recover from the database after restart. Do not interpret `update_available=0` as proof of freshness: consult check success/time, especially when background checks are disabled.

Useful alert expressions:

```promql
up{job="tokenresetsmonitor"} == 0
tokenresetsmonitor_provider_ready == 0
tokenresetsmonitor_deliveries{status="failed"} > 0
tokenresetsmonitor_oldest_pending_age_seconds > 3600
tokenresetsmonitor_config_last_reload_successful == 0
```

Use a suitable `for` duration, such as 5 minutes, to avoid alerts during normal startup. Provider readiness can remain false until the first complete scan succeeds.

## Liveness versus readiness

Liveness requires a running snapshot with a heartbeat no older than 60 seconds. A timestamp more than 5 seconds in the future is invalid. Shutdown, missing/unreadable snapshots, or stalled heartbeat make the probe fail.

Readiness additionally requires every active provider to be ready, have no last error, and have a recent successful scan. Its freshness budget allows two configured poll intervals plus five minutes per configured provider for bounded scans. Delivery failures are exposed through queue metrics; they do not make the process dead.

A source outage should lower readiness while liveness remains healthy. This avoids restart loops during an upstream outage and preserves the pending queue. Docker uses the liveness probe:

```sh
tokenresetsmonitor healthcheck --state-path /data/state.db
tokenresetsmonitor healthcheck --state-path /data/state.db --ready
```

This command does not load YAML or take a database lock. An invalid edited configuration that was rejected by reload therefore does not kill an otherwise healthy daemon.

The image runs this probe every 30 seconds, with a 5-second timeout, a 20-second startup period, and three retries. If you override `TRM_STATE_PATH`, override the Docker healthcheck command to use that path too. Docker records health separately from process state. Its `restart: unless-stopped` policy restarts an exited process; it does not itself restart a merely unhealthy container. See [Docker's healthcheck reference](https://docs.docker.com/reference/dockerfile/#healthcheck).

## Runnable examples and rule checks

Use [prometheus.yml](examples/prometheus.yml) together with [alerts.yml](examples/alerts.yml). Keep both files in the same directory, mount that directory into Prometheus, and select prometheus.yml as its configuration file. The target assumes Prometheus shares the monitor Compose network; use a loopback target for a native deployment.

The rules include a two-hour last-success freshness alert in addition to baseline readiness. Adjust the freshness threshold for nondefault poll intervals. Alert delivery remains your Prometheus/Alertmanager configuration; this monitor does not configure an external alerting account.

The [alert rule fixtures](examples/alerts.test.yml) exercise unavailable scraping and stale source detection. With promtool installed, run from the examples directory:

```sh
promtool check config prometheus.yml
promtool check rules alerts.yml
promtool test rules alerts.test.yml
```
