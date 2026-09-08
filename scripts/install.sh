#!/bin/sh
# Download, inspect, then run with sudo. Existing config/state are preserved.
set -eu
umask 077

VERSION=v1.0.0-rc.4
START=1
SYSTEMD=1
REPOSITORY=DKPlugins/TokenResetsMonitor
BIN=/usr/local/bin/tokenresetsmonitor
CONFIG=/etc/tokenresetsmonitor/config.yaml
DATA=/var/lib/tokenresetsmonitor
LOGS=/var/log/tokenresetsmonitor

usage() {
    cat <<'EOF'
Usage: sudo sh install.sh [--version vX.Y.Z[-prerelease]] [--no-start] [--foreground]

Installs a checksum-verified release for Linux amd64/arm64. Defaults to
v1.0.0-rc.4. Existing configuration and state are preserved. --foreground
skips systemd registration; --no-start leaves the installed service stopped.
EOF
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --version) [ "$#" -ge 2 ] || { usage >&2; exit 2; }; VERSION=$2; shift 2 ;;
        --no-start) START=0; shift ;;
        --foreground) SYSTEMD=0; START=0; shift ;;
        --help|-h) usage; exit 0 ;;
        *) usage >&2; exit 2 ;;
    esac
done
printf '%s\n' "$VERSION" | LC_ALL=C grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$' || {
    echo 'Version must be a release tag such as v1.0.0-rc.4.' >&2; exit 2;
}
[ "$(uname -s)" = Linux ] || { echo 'This installer supports Linux only.' >&2; exit 2; }
[ "$(id -u)" -eq 0 ] || { echo 'Run this installer with sudo or as root.' >&2; exit 2; }
case "$(uname -m)" in
    x86_64|amd64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) echo 'Supported architectures: amd64 and arm64.' >&2; exit 2 ;;
esac
for dependency in curl tar sha256sum awk install mktemp getent useradd fuser; do
    command -v "$dependency" >/dev/null 2>&1 || { echo "Required command missing: $dependency" >&2; exit 2; }
done
if [ "$SYSTEMD" -eq 1 ]; then
    if ! command -v systemctl >/dev/null 2>&1 || [ ! -d /run/systemd/system ]; then
        echo 'systemd is not running; use --foreground for a manual installation.' >&2; exit 2;
    fi
fi

WORK=$(mktemp -d /tmp/tokenresetsmonitor-install.XXXXXXXX)
BACKUP=
cleanup() {
    result=$?
    trap - EXIT HUP INT TERM
    # WORK is created by mktemp in this fixed directory, never user supplied.
    case "$WORK" in /tmp/tokenresetsmonitor-install.*) rm -rf -- "$WORK" ;; esac
    if [ "$result" -ne 0 ] && [ -n "$BACKUP" ]; then
        echo "Installation failed. Backup retained at $BACKUP. Check service state before restoring." >&2
    fi
    exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' HUP TERM

ASSET="tokenresetsmonitor_${VERSION}_linux_${ARCH}.tar.gz"
BASE="https://github.com/$REPOSITORY/releases/download/$VERSION"
curl --fail --location --silent --show-error --retry 3 --connect-timeout 10 --max-time 180 "$BASE/$ASSET" -o "$WORK/$ASSET"
curl --fail --location --silent --show-error --retry 3 --connect-timeout 10 --max-time 60 "$BASE/checksums.txt" -o "$WORK/checksums.txt"
EXPECTED=$(awk -v name="$ASSET" '$2 == name { print $1 }' "$WORK/checksums.txt")
printf '%s\n' "$EXPECTED" | LC_ALL=C grep -Eq '^[0-9a-fA-F]{64}$' || { echo 'Release checksum is missing or invalid.' >&2; exit 1; }
ACTUAL=$(sha256sum "$WORK/$ASSET" | awk '{ print $1 }')
[ "$ACTUAL" = "$EXPECTED" ] || { echo 'Checksum verification failed.' >&2; exit 1; }
mkdir "$WORK/extracted"
tar -xzf "$WORK/$ASSET" -C "$WORK/extracted" tokenresetsmonitor tokenresetsmonitor.service
chmod 755 "$WORK/extracted/tokenresetsmonitor"
"$WORK/extracted/tokenresetsmonitor" version
if [ -f "$CONFIG" ]; then
    "$WORK/extracted/tokenresetsmonitor" config validate --config "$CONFIG"
fi

if ! getent passwd tokenresetsmonitor >/dev/null; then
    NOLOGIN=$(command -v nologin || printf /bin/false)
    useradd --system --user-group --home-dir "$DATA" --no-create-home --shell "$NOLOGIN" tokenresetsmonitor
fi
install -d -m 750 -o root -g tokenresetsmonitor /etc/tokenresetsmonitor
install -d -m 700 -o tokenresetsmonitor -g tokenresetsmonitor "$DATA" "$LOGS"

if [ -f "$BIN" ] || [ -f "$CONFIG" ]; then
    if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
        if systemctl cat tokenresetsmonitor.service >/dev/null 2>&1; then
            systemctl stop tokenresetsmonitor.service
        fi
    fi
    if [ -f "$BIN" ] && fuser "$BIN" >/dev/null 2>&1; then
        echo 'Stop the foreground monitor before updating.' >&2; exit 1
    fi
    BACKUP="/var/backups/tokenresetsmonitor/$(date -u +%Y%m%dT%H%M%SZ)-$$"
    install -d -m 700 "$BACKUP"
    [ ! -f "$BIN" ] || cp -p "$BIN" "$BACKUP/tokenresetsmonitor"
    [ ! -f "$CONFIG" ] || cp -p "$CONFIG" "$BACKUP/config.yaml"
    cp -a "$DATA" "$BACKUP/data"
    [ ! -f /etc/systemd/system/tokenresetsmonitor.service ] || cp -p /etc/systemd/system/tokenresetsmonitor.service "$BACKUP/tokenresetsmonitor.service"
    echo "Backup: $BACKUP"
fi

install -m 755 "$WORK/extracted/tokenresetsmonitor" "$BIN.new"
mv -f "$BIN.new" "$BIN"
if [ ! -f "$CONFIG" ]; then
    TRM_STATE_PATH="$DATA/state.db" TRM_LOGGING_FORMAT=json TRM_LOGGING_FILE_ENABLED=true TRM_LOGGING_DIRECTORY="$LOGS" \
        "$BIN" init --defaults --config "$CONFIG"
fi
chown root:tokenresetsmonitor "$CONFIG"
chmod 640 "$CONFIG"
"$BIN" config validate --config "$CONFIG"
if [ "$SYSTEMD" -eq 1 ]; then
    install -m 644 "$WORK/extracted/tokenresetsmonitor.service" /etc/systemd/system/tokenresetsmonitor.service
    systemctl daemon-reload
    systemctl enable tokenresetsmonitor.service
    if [ "$START" -eq 1 ]; then
        systemctl start tokenresetsmonitor.service
        sleep 2
        systemctl is-active --quiet tokenresetsmonitor.service || { echo 'Service failed to start; inspect journalctl -u tokenresetsmonitor.' >&2; exit 1; }
    fi
fi
echo "Installed $VERSION. Configure $CONFIG and restart the monitor to apply changes."
echo 'Both notification channels are disabled in a new default configuration.'
echo 'Use test-notification to verify a configured channel before enabling it.'
