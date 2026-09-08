#!/bin/sh
# Negative acceptance against a published installer on a disposable runner.
# Only the downloaded archive is changed; its real release checksum is retained.
set -eu
[ "${CI:-}" = true ] || { echo 'Only run on a disposable CI runner (CI=true).' >&2; exit 2; }
[ "$(id -u)" -eq 0 ] || { echo 'Root is required to exercise installer checks.' >&2; exit 2; }
[ "$#" -eq 2 ] || { echo 'Usage: test-installer-checksum.sh INSTALLER VERSION' >&2; exit 2; }
INSTALLER=$1
VERSION=$2
[ -f "$INSTALLER" ] || { echo 'Published installer was not downloaded.' >&2; exit 2; }
if [ -e /usr/local/bin/tokenresetsmonitor ] || [ -e /etc/tokenresetsmonitor ] || [ -e /var/lib/tokenresetsmonitor ]; then
    echo 'Run this negative test before installing TokenResetsMonitor.' >&2; exit 2
fi
WORK=$(mktemp -d /tmp/tokenresetsmonitor-checksum-test.XXXXXXXX)
cleanup() {
    result=$?
    trap - EXIT
    case "$WORK" in /tmp/tokenresetsmonitor-checksum-test.*) rm -rf -- "$WORK" ;; esac
    exit "$result"
}
trap cleanup EXIT
REAL_CURL=$(command -v curl)
mkdir "$WORK/bin"
cat > "$WORK/bin/curl" <<'EOF'
#!/bin/sh
set -eu
"$TRM_TEST_REAL_CURL" "$@"
output=
previous=
for argument in "$@"; do
    if [ "$previous" = -o ]; then output=$argument; fi
    previous=$argument
done
case "$output" in
    *.tar.gz) printf 'intentional checksum acceptance corruption\n' >> "$output" ;;
esac
EOF
chmod 755 "$WORK/bin/curl"
if PATH="$WORK/bin:$PATH" TRM_TEST_REAL_CURL="$REAL_CURL" \
    sh "$INSTALLER" --version "$VERSION" --foreground --no-start > "$WORK/output.log" 2>&1; then
    cat "$WORK/output.log" >&2
    echo 'Installer accepted a corrupted archive.' >&2
    exit 1
fi
cat "$WORK/output.log"
grep -Fq 'Checksum verification failed.' "$WORK/output.log" || {
    echo 'Installer failed for a reason other than the checksum mismatch.' >&2; exit 1;
}
[ ! -e /usr/local/bin/tokenresetsmonitor ]
[ ! -e /etc/tokenresetsmonitor ]
[ ! -e /var/lib/tokenresetsmonitor ]
echo 'Published Linux installer rejected a corrupted archive before installation.'
