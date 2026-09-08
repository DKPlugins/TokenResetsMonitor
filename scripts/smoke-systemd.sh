#!/bin/sh
# Disposable CI runner acceptance check; never reuse an existing installation.
set -eu
[ "${CI:-}" = true ] || { echo 'Only run on a disposable CI runner (CI=true).' >&2; exit 2; }
[ "$(id -u)" -eq 0 ] || { echo 'Root is required for the systemd lifecycle test.' >&2; exit 2; }
for existing in /usr/local/bin/tokenresetsmonitor /etc/tokenresetsmonitor /var/lib/tokenresetsmonitor /var/log/tokenresetsmonitor /etc/systemd/system/tokenresetsmonitor.service; do
    if [ -e "$existing" ] || [ -L "$existing" ]; then
        echo 'An installation or retained data already exists; refusing to change it.' >&2; exit 1
    fi
done
cleanup() {
    result=$?
    trap - EXIT
    systemctl stop tokenresetsmonitor.service || true
    systemctl disable tokenresetsmonitor.service || true
    rm -f /etc/systemd/system/tokenresetsmonitor.service /usr/local/bin/tokenresetsmonitor
    systemctl daemon-reload
    # All of these fixed paths were absent before this CI-only test.
    rm -rf /etc/tokenresetsmonitor /var/lib/tokenresetsmonitor /var/log/tokenresetsmonitor
    exit "$result"
}
trap cleanup EXIT
getent passwd tokenresetsmonitor >/dev/null || useradd --system --user-group --no-create-home --shell /bin/false tokenresetsmonitor
install -d -m 750 -o root -g tokenresetsmonitor /etc/tokenresetsmonitor
install -d -m 700 -o tokenresetsmonitor -g tokenresetsmonitor /var/lib/tokenresetsmonitor /var/log/tokenresetsmonitor
install -m 755 tokenresetsmonitor /usr/local/bin/tokenresetsmonitor
TRM_TELEGRAM_BOT_TOKEN=fixture-service-token TRM_UPDATES_ENABLED=false \
    TRM_STATE_PATH=/var/lib/tokenresetsmonitor/state.db TRM_API_BASE_URL=http://127.0.0.1:1/api/v1 \
    TRM_LOGGING_FORMAT=json TRM_LOGGING_FILE_ENABLED=true TRM_LOGGING_DIRECTORY=/var/log/tokenresetsmonitor \
    /usr/local/bin/tokenresetsmonitor init --defaults --config /etc/tokenresetsmonitor/config.yaml
printf '%s\n' 'TRM_TELEGRAM_BOT_TOKEN=fixture-service-token' 'TRM_UPDATES_ENABLED=false' > /etc/tokenresetsmonitor/environment
chmod 600 /etc/tokenresetsmonitor/environment
# Load only the production validation function; never execute the installer.
validation_helper=/etc/tokenresetsmonitor/validate-helper.sh
sed -n '/^validate_configuration() {/,/^}/p' scripts/install.sh > "$validation_helper"
CONFIG=/etc/tokenresetsmonitor/config.yaml
# shellcheck disable=SC1090
. "$validation_helper"
if /usr/local/bin/tokenresetsmonitor config validate --config "$CONFIG" >/dev/null 2>&1; then
    echo 'Fixture should require the service environment.' >&2; exit 1
fi
validate_configuration /usr/local/bin/tokenresetsmonitor
chown root:tokenresetsmonitor /etc/tokenresetsmonitor/config.yaml
chmod 640 /etc/tokenresetsmonitor/config.yaml
install -m 644 packaging/systemd/tokenresetsmonitor.service /etc/systemd/system/tokenresetsmonitor.service
systemd-analyze verify /etc/systemd/system/tokenresetsmonitor.service
systemctl daemon-reload
systemctl enable --now tokenresetsmonitor.service
sleep 2
systemctl is-active --quiet tokenresetsmonitor.service
systemctl stop tokenresetsmonitor.service
[ -s /var/lib/tokenresetsmonitor/state.db ]
# Replacing the stopped executable models the installer update operation.
install -m 755 tokenresetsmonitor /usr/local/bin/tokenresetsmonitor
systemctl start tokenresetsmonitor.service
sleep 2
systemctl is-active --quiet tokenresetsmonitor.service
systemctl stop tokenresetsmonitor.service
echo 'systemd install/start/stop/update passed.'
