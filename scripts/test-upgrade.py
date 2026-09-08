#!/usr/bin/env python3
"""Exercise migration and rollback against exact prior Git sources in temp dirs."""
import argparse
import io
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile


def run(arguments, **kwargs):
    return subprocess.run(arguments, check=True, **kwargs)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--previous", action="append", help="prior immutable tag (repeatable)")
    args = parser.parse_args()
    previous = args.previous or ["v1.0.0", "v1.1.0-rc.2"]
    if any(not re.fullmatch(r"v\d+\.\d+\.\d+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?", tag) for tag in previous):
        parser.error("previous versions must be immutable vX.Y.Z[-prerelease] tags")
    root = Path(__file__).resolve().parent.parent
    module = "github.com/DKPlugins/TokenResetsMonitor/internal/buildinfo"
    binary_name = "tokenresetsmonitor.exe" if os.name == "nt" else "tokenresetsmonitor"
    commit = run(["git", "rev-parse", "HEAD"], cwd=root, capture_output=True, text=True).stdout.strip()
    with tempfile.TemporaryDirectory(prefix="trm-upgrade-") as directory:
        work = Path(directory)
        candidate = work / binary_name
        run(["go", "build", "-trimpath", "-ldflags", f"-X {module}.Commit={commit}",
             "-o", str(candidate), "./cmd/tokenresetsmonitor"], cwd=root)
        for tag in previous:
            source = work / tag
            source.mkdir()
            old_commit = run(["git", "rev-parse", f"{tag}^{{commit}}"], cwd=root,
                             capture_output=True, text=True).stdout.strip()
            archive = run(["git", "archive", "--format=tar", old_commit], cwd=root, capture_output=True).stdout
            with tarfile.open(fileobj=io.BytesIO(archive)) as contents:
                contents.extractall(source, filter="data")
            old_binary = source / binary_name
            run(["go", "build", "-trimpath", "-ldflags",
                 f"-X {module}.Version={tag[1:]} -X {module}.Commit={old_commit}",
                 "-o", str(old_binary), "./cmd/tokenresetsmonitor"], cwd=source)
            environment = dict(os.environ, TRM_ACCEPTANCE_OLD_BINARY=str(old_binary),
                               TRM_ACCEPTANCE_NEW_BINARY=str(candidate))
            print(f"Checking upgrade and backup rollback from {tag} at {old_commit}", flush=True)
            run(["go", "test", "-count=1", "-v", "-timeout", "2m", "./internal/monitor",
                 "-run", "^TestBinaryUpgradeRollbackAcceptance$"], cwd=root, env=environment)


if __name__ == "__main__":
    main()
