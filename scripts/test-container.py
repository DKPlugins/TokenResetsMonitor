#!/usr/bin/env python3
"""Exercise a built monitor image using isolated, disposable Docker resources.

Requires Python 3.10+, Docker, and already available monitor/Node fixture images.
No host ports are published. The test network cannot access the public internet;
updates and notification channels are disabled. Example:
  python scripts/test-container.py --image trm-test:amd64 --platform linux/amd64
"""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time
import uuid


LABEL = "io.tokenresetsmonitor.smoke"
FIXTURE = """const http = require('node:http');
const status = Number(process.env.FIXTURE_STATUS);
const body = JSON.stringify({data: [], pagination: {has_more: false}, meta: {schema_version: '1.0'}});
http.createServer((req, res) => {
  res.writeHead(status, {'Content-Type': 'application/json'});
  res.end(body);
}).listen(8080, '0.0.0.0');
"""


class SmokeFailure(RuntimeError):
    pass


def docker(*args: str, check: bool = True, timeout: float = 60) -> subprocess.CompletedProcess[str]:
    result = subprocess.run(
        ["docker", *args], check=False, capture_output=True, text=True,
        encoding="utf-8", errors="replace", timeout=timeout,
    )
    if check and result.returncode:
        raise SmokeFailure(f"docker {' '.join(args[:3])} failed: {result.stderr.strip() or result.stdout.strip()}")
    return result


def wait_until(label: str, condition, timeout: float):
    deadline = time.monotonic() + timeout
    detail = "condition remains false"
    while time.monotonic() < deadline:
        try:
            value = condition()
            if value:
                return value
        except (SmokeFailure, json.JSONDecodeError) as error:
            detail = str(error)
        time.sleep(0.5)
    raise SmokeFailure(f"Timed out waiting for {label}: {detail}")


class Smoke:
    def __init__(self, args: argparse.Namespace, directory: Path):
        self.args = args
        self.directory = directory.resolve()
        self.identity = uuid.uuid4().hex
        self.prefix = "trm-smoke-" + self.identity
        self.network = self.prefix + "-network"
        self.volume = self.prefix + "-data"
        self.monitor = self.prefix + "-monitor"
        self.fixture = self.prefix + "-source"
        self.resources: list[tuple[str, str]] = []
        self.config_dir = self.directory / "config"
        self.config_dir.mkdir(mode=0o755)
        self.config_path = self.config_dir / "config.yaml"
        self.platform = ["--platform", args.platform] if args.platform else []
        self.config = {
            "config_version": 2,
            "api_base_url": "http://upstream:8080",
            "poll_interval": "1m",
            "request_timeout": "5s",
            "state_path": "/data/state.db",
            "providers": [{"slug": "openai-codex"}],
            "event_types": ["hard_reset"],
            "minimum_confidence": "reported",
            "unknown_scope": "include",
            "webhook": {"enabled": False},
            "telegram": {"enabled": False},
            "slack": {"enabled": False},
            "reload": {"enabled": True},
            "observability": {"enabled": True, "listen": "0.0.0.0:9090"},
            "updates": {"enabled": False},
            "logging": {"level": "info", "format": "json", "file_enabled": False},
        }

    def config_write(self, malformed: bool = False):
        # JSON is also valid YAML. Replace the file inside the mounted directory,
        # exercising the same atomic-save behavior used by editors.
        candidate = self.config_dir / "config.next"
        data = "config_version: [invalid-smoke" if malformed else json.dumps(self.config, indent=2)
        candidate.write_text(data + "\n", encoding="utf-8")
        candidate.chmod(0o644)
        for attempt in range(20):
            try:
                os.replace(candidate, self.config_path)
                return
            except PermissionError:
                if attempt == 19:
                    raise
                time.sleep(0.05)

    def owns(self, kind: str, name: str) -> bool:
        label_path = ".Config.Labels" if kind == "container" else ".Labels"
        result = docker(kind, "inspect", "--format", "{{json " + label_path + "}}", name, check=False)
        if result.returncode:
            return False
        return (json.loads(result.stdout) or {}).get(LABEL) == self.identity

    def remove(self, kind: str, name: str):
        if not self.owns(kind, name):
            return
        options = ["--force"] if kind == "container" else []
        docker(kind, "rm", *options, name)

    def fixture_start(self, status: int):
        if ("container", self.fixture) in self.resources:
            self.remove("container", self.fixture)
        else:
            self.resources.append(("container", self.fixture))
        docker(
            "run", "--detach", "--pull=never", "--name", self.fixture,
            "--label", f"{LABEL}={self.identity}", "--network", self.network,
            "--network-alias", "upstream", "--read-only", "--user", "1000:1000",
            "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
            "--env", f"FIXTURE_STATUS={status}", self.args.fixture_image,
            "node", "-e", FIXTURE,
        )
        probe = "fetch('http://127.0.0.1:8080').then(r=>process.exit(r.status===Number(process.env.FIXTURE_STATUS)?0:1)).catch(()=>process.exit(1))"
        wait_until("fixture startup", lambda: docker("exec", self.fixture, "node", "-e", probe, check=False).returncode == 0, self.args.timeout)

    def http(self, path: str) -> tuple[int, str]:
        result = docker("exec", self.monitor, "wget", "-S", "-O", "-", "http://127.0.0.1:9090" + path, check=False)
        statuses = re.findall(r"HTTP/\d(?:\.\d)?\s+(\d{3})", result.stderr)
        if not statuses:
            raise SmokeFailure("observability endpoint did not return an HTTP status")
        return int(statuses[-1]), result.stdout

    def status(self) -> dict:
        return json.loads(docker("exec", self.monitor, "cat", "/data/state.db.status.json").stdout)

    def ready(self, expected: int) -> bool:
        return self.http("/livez")[0] == 200 and self.http("/readyz")[0] == expected

    def metrics(self) -> str:
        code, body = self.http("/metrics")
        if code != 200:
            raise SmokeFailure(f"metrics returned HTTP {code}")
        return body

    def generation(self, minimum: int, error: str = "") -> bool:
        runtime = self.status().get("runtime", {})
        return (runtime.get("config_generation", 0) >= minimum
                and not runtime.get("reload_pending", False)
                and runtime.get("reload_error", "") == error)

    def phase(self, text: str):
        print("PASS " + text, flush=True)

    def run(self):
        for image in (self.args.image, self.args.fixture_image):
            docker("image", "inspect", image)
        self.resources.append(("network", self.network))
        docker("network", "create", "--internal", "--label", f"{LABEL}={self.identity}", self.network)
        self.resources.append(("volume", self.volume))
        docker("volume", "create", "--label", f"{LABEL}={self.identity}", self.volume)
        self.config_write()
        self.fixture_start(200)
        self.resources.append(("container", self.monitor))
        docker(
            "run", "--detach", "--pull=never", "--name", self.monitor, *self.platform,
            "--label", f"{LABEL}={self.identity}", "--network", self.network,
            "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
            "--health-interval=2s", "--health-start-period=2s", "--health-timeout=5s", "--health-retries=3",
            "--mount", f"type=bind,src={self.config_dir},dst=/config,readonly",
            "--mount", f"type=volume,src={self.volume},dst=/data", self.args.image,
        )
        wait_until("initial live and ready", lambda: self.ready(200), self.args.timeout)
        wait_until("Docker healthcheck", lambda: docker("inspect", "--format", "{{.State.Health.Status}}", self.monitor).stdout.strip() == "healthy", self.args.timeout)
        initial = self.status()
        if initial.get("pending") != 0 or not initial["providers"]["openai-codex"]["ready"]:
            raise SmokeFailure("initial complete scan did not create an empty baseline")
        metrics = self.metrics()
        for required in ("tokenresetsmonitor_build_info", "tokenresetsmonitor_provider_scans_total", 'tokenresetsmonitor_provider_ready{provider="openai-codex"} 1'):
            if required not in metrics:
                raise SmokeFailure("initial metrics are missing " + required)
        self.phase("initial baseline, live/ready endpoints, Docker healthcheck and metrics")

        # Exercise local management as the image's non-root service user with
        # read-only root/config mounts and no management port exposed to the host.
        for command in (
            ["history", "list"],
            ["deliveries", "list"],
            ["filters", "preview", "--candidate-config", "/config/config.yaml"],
        ):
            response = json.loads(docker("exec", self.monitor, "tokenresetsmonitor", *command,
                                         "--config", "/config/config.yaml", "--json").stdout)
            if response.get("configuration_source") != "running" or "data" not in response:
                raise SmokeFailure("management did not query the running daemon")
        if self.status().get("pending") != 0:
            raise SmokeFailure("read-only management changed the delivery queue")
        self.phase("private live history, delivery listing and filter preview under the non-root container user")

        self.fixture_start(500)
        self.config["logging"]["level"] = "debug"
        self.config_write()
        wait_until("reload with failed upstream", lambda: self.generation(2), self.args.timeout)
        wait_until("upstream failure readiness", lambda: self.ready(503), self.args.timeout)
        docker("exec", self.monitor, "tokenresetsmonitor", "healthcheck", "--state-path", "/data/state.db")
        if docker("exec", self.monitor, "tokenresetsmonitor", "healthcheck", "--state-path", "/data/state.db", "--ready", check=False).returncode != 1:
            raise SmokeFailure("source failure did not fail readiness independently of liveness")
        metrics = self.metrics()
        if 'result="error"' not in metrics or 'tokenresetsmonitor_provider_ready{provider="openai-codex"} 0' not in metrics:
            raise SmokeFailure("upstream failure was absent from metrics")
        self.phase("upstream HTTP 500 keeps liveness healthy and makes readiness fail")

        self.fixture_start(200)
        self.config["logging"]["level"] = "info"
        self.config_write()
        wait_until("recovery reload", lambda: self.generation(3), self.args.timeout)
        wait_until("upstream recovery readiness", lambda: self.ready(200), self.args.timeout)
        self.phase("upstream recovery restores readiness without restarting monitor")

        self.config_write(malformed=True)
        wait_until("invalid candidate rejection", lambda: self.generation(3, "invalid_config"), self.args.timeout)
        if not self.ready(200):
            raise SmokeFailure("invalid config disrupted the active generation")
        docker("exec", self.monitor, "tokenresetsmonitor", "healthcheck", "--state-path", "/data/state.db")
        self.phase("invalid YAML retains working settings and independent healthcheck")

        self.config["logging"]["level"] = "debug"
        self.config_write()
        wait_until("valid candidate application", lambda: self.generation(4), self.args.timeout)
        wait_until("readiness after valid reload", lambda: self.ready(200), self.args.timeout)
        if 'result="applied"' not in self.metrics():
            raise SmokeFailure("successful reload is missing from process metrics")
        self.phase("valid YAML applies after rejection and keeps process counters")

        docker("stop", "--time", "45", self.monitor)
        if docker("inspect", "--format", "{{.State.ExitCode}}", self.monitor).stdout.strip() != "0":
            raise SmokeFailure("monitor exited unsuccessfully on graceful stop")
        snapshot = self.directory / "stopped-status.json"
        docker("cp", self.monitor + ":/data/state.db.status.json", str(snapshot))
        stopped = json.loads(snapshot.read_text(encoding="utf-8"))
        if stopped.get("running") is not False or stopped.get("pending") != 0:
            raise SmokeFailure("graceful shutdown did not publish stopped durable state")
        self.phase("graceful shutdown publishes running=false and preserves baseline")

    def cleanup(self):
        failures = []
        for kind, name in reversed(self.resources):
            try:
                self.remove(kind, name)
            except (SmokeFailure, subprocess.TimeoutExpired) as error:
                failures.append(str(error))
        if failures:
            raise SmokeFailure("Resource cleanup failed: " + "; ".join(failures))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", default="tokenresetsmonitor:implementation-test")
    parser.add_argument("--fixture-image", default="node:24-alpine")
    parser.add_argument("--platform", choices=("linux/amd64", "linux/arm64"))
    parser.add_argument("--timeout", type=float, default=60, help="Maximum wait per lifecycle stage in seconds")
    args = parser.parse_args()
    if args.timeout <= 0:
        parser.error("--timeout must be positive")
    try:
        with tempfile.TemporaryDirectory(prefix="trm-container-smoke-") as directory:
            smoke = Smoke(args, Path(directory))
            try:
                smoke.run()
            except Exception:
                logs = docker("logs", "--tail", "80", smoke.monitor, check=False)
                if logs.stdout or logs.stderr:
                    print(logs.stdout + logs.stderr, file=sys.stderr)
                raise
            finally:
                # Only UUID-named resources carrying this invocation's label are
                # removed; no broad Docker prune or shell-built deletion occurs.
                smoke.cleanup()
        print("PASS isolated container lifecycle; temporary Docker resources removed", flush=True)
        return 0
    except (SmokeFailure, OSError, subprocess.SubprocessError, ValueError) as error:
        print("FAIL " + str(error), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
