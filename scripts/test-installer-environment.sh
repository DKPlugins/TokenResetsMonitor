#!/bin/sh
# Exercise only the installer validation helper; no downloads or service changes.
set -eu
BINARY=${1:?Usage: sh scripts/test-installer-environment.sh /path/to/tokenresetsmonitor}
SCRIPT_ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
TEST_ROOT=$(mktemp -d /tmp/tokenresetsmonitor-installer-test.XXXXXXXX)
cleanup() {
    case "$TEST_ROOT" in /tmp/tokenresetsmonitor-installer-test.*) rm -rf -- "$TEST_ROOT" ;; esac
}
trap cleanup EXIT HUP INT TERM
unset TRM_INSTALLER_TEST_ABSENT
CONFIG="$TEST_ROOT/config.yaml"
# Extract one audited function, never execute the installer entrypoint.
sed -n '/^validate_configuration() {$/,/^}$/p' "$SCRIPT_ROOT/install.sh" > "$TEST_ROOT/helper.sh"
[ -s "$TEST_ROOT/helper.sh" ] || { echo 'Installer helper missing.' >&2; exit 1; }
# shellcheck disable=SC1091
. "$TEST_ROOT/helper.sh"
cat > "$CONFIG" <<'EOF'
config_version: 2
telegram:
  enabled: true
  bot_token: "${TRM_INSTALLER_TEST_ABSENT}"
  chat_id: "123"
EOF
validate_configuration "$BINARY" > "$TEST_ROOT/report"
grep -Fq telegram.bot_token "$TEST_ROOT/report" || { echo 'Deferred requirement was not reported.' >&2; exit 1; }
if "$BINARY" config validate --config "$CONFIG" >/dev/null 2>&1; then
    echo 'Runtime validation accepted a missing required environment variable.' >&2; exit 1
fi
cp "$CONFIG" "$TEST_ROOT/valid.yaml"
printf 'poll_interval: soon\n' >> "$CONFIG"
if validate_configuration "$BINARY" >/dev/null 2>&1; then
    echo 'Structural validation accepted a literal invalid duration.' >&2; exit 1
fi
cp "$TEST_ROOT/valid.yaml" "$CONFIG"
printf 'unknown_setting: true\n' >> "$CONFIG"
if validate_configuration "$BINARY" >/dev/null 2>&1; then
    echo 'Structural validation accepted an unknown field.' >&2; exit 1
fi
echo 'Installer structural validation passed: deferred environment, strict literals/schema, full runtime validation.'
