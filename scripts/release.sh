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
# Each release installer defaults to its own immutable release.
sed -E "s/^(VERSION=)v[0-9][0-9A-Za-z.-]*/\1$VERSION/" scripts/install.sh > "$OUTPUT/install.sh"
sed -E "s/(Version = ')v[0-9][0-9A-Za-z.-]*(',)/\1$VERSION\2/" scripts/install.ps1 > "$OUTPUT/install.ps1"
go run -ldflags "$FLAGS" ./cmd/tokenresetsmonitor compatibility-manifest > "$OUTPUT/compatibility.json"

for TARGET in linux_amd64 linux_arm64 windows_amd64; do
    OS=${TARGET%_*}
    ARCH=${TARGET#*_}
    WORK="$OUTPUT/$TARGET"
    mkdir "$WORK"
    BINARY=tokenresetsmonitor
    [ "$OS" != windows ] || BINARY=tokenresetsmonitor.exe
    CGO_ENABLED=0 GOOS="$OS" GOARCH="$ARCH" go build -trimpath -ldflags "$FLAGS" -o "$WORK/$BINARY" ./cmd/tokenresetsmonitor
    cp LICENSE README.md CHANGELOG.md CONTRIBUTING.md config.example.yaml compose.yaml "$WORK/"
    cp -R docs "$WORK/docs"
    cp "$OUTPUT/install.sh" "$OUTPUT/install.ps1" "$OUTPUT/compatibility.json" "$WORK/"
    cp packaging/systemd/tokenresetsmonitor.service "$WORK/"
    ASSET="tokenresetsmonitor_${VERSION}_${TARGET}"
    if [ "$OS" = windows ]; then
        (cd "$WORK" && zip -qr "$OUTPUT/$ASSET.zip" .)
    else
        chmod 755 "$WORK/tokenresetsmonitor" "$WORK/install.sh"
        tar -czf "$OUTPUT/$ASSET.tar.gz" -C "$WORK" tokenresetsmonitor LICENSE README.md CHANGELOG.md CONTRIBUTING.md config.example.yaml compose.yaml docs compatibility.json install.sh install.ps1 tokenresetsmonitor.service
    fi
done
(cd "$OUTPUT" && sha256sum ./*.tar.gz ./*.zip ./install.sh ./install.ps1 ./compatibility.json | sed 's|  \./|  |' > checksums.txt)
echo "Release files: $OUTPUT"
