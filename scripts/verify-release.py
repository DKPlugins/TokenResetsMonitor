#!/usr/bin/env python3
"""Inspect locally built release archives without extracting or executing them."""
import hashlib
import json
from pathlib import Path, PurePosixPath
import posixpath
import re
import sys
import tarfile
import zipfile

def fail(message):
    raise SystemExit(message)

def verify(root):
    tag = root.name
    if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?", tag):
        fail("Expected a dist/vX.Y.Z[-prerelease] directory")
    seen = set()
    for line in (root / "checksums.txt").read_text(encoding="utf-8").splitlines():
        digest, name = line.split()
        if name in seen or Path(name).name != name:
            fail("Duplicate or unsafe checksum filename")
        seen.add(name)
        if hashlib.sha256((root / name).read_bytes()).hexdigest() != digest.lower():
            fail("Checksum mismatch: " + name)
    manifest = json.loads((root / "compatibility.json").read_text(encoding="utf-8"))
    if manifest["manifest_version"] != 1 or manifest["version"] != tag[1:]:
        fail("Compatibility manifest does not match the release")
    if "VERSION=" + tag not in (root / "install.sh").read_text(encoding="utf-8"):
        fail("Linux installer does not default to this release")
    if "$Version = '" + tag + "'" not in (root / "install.ps1").read_text(encoding="utf-8"):
        fail("Windows installer does not default to this release")
    archives = sorted(root.glob("*.tar.gz")) + sorted(root.glob("*.zip"))
    if len(archives) != 3:
        fail("Expected the three supported native release archives")
    required = {"README.md", "LICENSE", "CHANGELOG.md", "CONTRIBUTING.md",
                "config.example.yaml", "compose.yaml", "compatibility.json",
                "install.sh", "install.ps1", "tokenresetsmonitor.service",
                "docs/architecture.md", "docs/upgrading.md", "docs/observability.md", "docs/history.md",
                "docs/examples/prometheus.yml", "docs/examples/alerts.yml", "docs/examples/alerts.test.yml"}
    payloads = {archive.name for archive in archives} | {"install.sh", "install.ps1", "compatibility.json"}
    if not payloads.issubset(seen):
        fail("A release payload is missing from checksums.txt")
    for archive in archives:
        if archive.suffix == ".zip":
            with zipfile.ZipFile(archive) as source:
                files = {name: source.read(name) for name in source.namelist() if not name.endswith("/")}
        else:
            with tarfile.open(archive, "r:gz") as source:
                files = {member.name: source.extractfile(member).read()
                         for member in source.getmembers() if member.isfile()}
        if not required.issubset(files):
            fail("Missing documentation/runtime files in " + archive.name)
        binary = "tokenresetsmonitor.exe" if archive.suffix == ".zip" else "tokenresetsmonitor"
        if binary not in files or not files[binary]:
            fail("Missing executable in " + archive.name)
        if json.loads(files["compatibility.json"]) != manifest:
            fail("Archive compatibility manifest differs from standalone manifest")
        for name, contents in files.items():
            if not name.endswith(".md"):
                continue
            text = contents.decode("utf-8")
            for target in re.findall(r"(?<!!)\[[^\]]+\]\(([^)]+)\)", text):
                target = target.split("#", 1)[0].strip("<>")
                if not target or re.match(r"^[a-zA-Z][a-zA-Z0-9+.-]*:", target):
                    continue
                resolved = posixpath.normpath(str(PurePosixPath(name).parent / target))
                if resolved not in files:
                    fail("Broken local link in " + archive.name + ": " + name + " -> " + target)
        print("Verified checksums, manifest, installers, content, and local links:", archive.name)

if __name__ == "__main__":
    if len(sys.argv) != 2:
        fail("Usage: python3 scripts/verify-release.py dist/vX.Y.Z")
    verify(Path(sys.argv[1]).resolve())
