#!/bin/sh
# Build release archives locally or in CI. Publishing is a separate operation.
set -eu

VERSION=${1:-}
printf '%s\n' "$VERSION" | LC_ALL=C grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$' || {
    echo 'Usage: sh scripts/release.sh vX.Y.Z[-prerelease]' >&2; exit 2;
}
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
COMMIT=$(git rev-parse HEAD)
BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
MODULE=github.com/DKPlugins/TokenResetsMonitor/internal/buildinfo
FLAGS="-s -w -X $MODULE.Version=${VERSION#v} -X $MODULE.Commit=$COMMIT -X $MODULE.Date=$BUILD_DATE"
OUTPUT="$ROOT/dist/$VERSION"
[ ! -e "$OUTPUT" ] || { echo "Output already exists: $OUTPUT; use a new version or remove an unpublished local build explicitly." >&2; exit 1; }
mkdir -p "$OUTPUT"

for TARGET in linux_amd64 linux_arm64 windows_amd64; do
    OS=${TARGET%_*}
    ARCH=${TARGET#*_}
    WORK="$OUTPUT/$TARGET"
    mkdir "$WORK"
    BINARY=tokenresetsmonitor
    [ "$OS" != windows ] || BINARY=tokenresetsmonitor.exe
    CGO_ENABLED=0 GOOS="$OS" GOARCH="$ARCH" go build -trimpath -ldflags "$FLAGS" -o "$WORK/$BINARY" ./cmd/tokenresetsmonitor
    cp LICENSE README.md config.example.yaml "$WORK/"
    cp scripts/install.sh scripts/install.ps1 "$WORK/"
    cp packaging/systemd/tokenresetsmonitor.service "$WORK/"
    ASSET="tokenresetsmonitor_${VERSION}_${TARGET}"
    if [ "$OS" = windows ]; then
        (cd "$WORK" && zip -q "$OUTPUT/$ASSET.zip" ./*)
    else
        chmod 755 "$WORK/tokenresetsmonitor" "$WORK/install.sh"
        tar -czf "$OUTPUT/$ASSET.tar.gz" -C "$WORK" tokenresetsmonitor LICENSE README.md config.example.yaml install.sh install.ps1 tokenresetsmonitor.service
    fi
done
(cd "$OUTPUT" && sha256sum ./*.tar.gz ./*.zip | sed 's|  \./|  |' > checksums.txt)
cp scripts/install.sh scripts/install.ps1 "$OUTPUT/"
echo "Release files: $OUTPUT"
