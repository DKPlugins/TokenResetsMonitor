package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestInstallationDefersMissingServiceSecretsWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte("config_version: 2\ntelegram:\n  enabled: true\n  bot_token: '${INSTALL_MISSING_BOT_TOKEN}'\n  chat_id: '42'\nslack:\n  enabled: true\n  webhook_url: '${INSTALL_MISSING_SLACK_URL}'\n")
	if err := WriteNewBytes(path, original); err != nil {
		t.Fatal(err)
	}
	deferred, err := ValidateInstallation(path, nil)
	if err != nil || !reflect.DeepEqual(deferred, []string{"slack.webhook_url", "telegram.bot_token"}) {
		t.Fatalf("deferred=%v err=%v", deferred, err)
	}
	current, _ := os.ReadFile(path)
	if !bytes.Equal(current, original) {
		t.Fatal("structural validation rewrote configuration")
	}
	if _, ok := os.LookupEnv("INSTALL_MISSING_BOT_TOKEN"); ok {
		t.Fatal("validation created service environment")
	}
	if _, err := Load(path, nil); err == nil {
		t.Fatal("full runtime load silently accepted missing secrets")
	}
}

func TestInstallationRejectsInvalidLiteralsBesideMissingSecret(t *testing.T) {
	cases := []string{
		"poll_interval: 59s\ntelegram:\n  enabled: true\n  bot_token: '${INSTALL_MISSING_SECRET}'\n  chat_id: '42'\n",
		"telegram:\n  enabled: true\n  bot_token: '${INSTALL_MISSING_SECRET}'\n  chat_id: '42'\n  message_thread_id: -1\n",
		"webhook:\n  enabled: true\n  url: '${INSTALL_MISSING_SECRET}'\n  method: invalid\n",
		"webhook:\n  enabled: true\n  url: 'ftp://example.invalid/${INSTALL_MISSING_SECRET}'\n",
		"webhook:\n  enabled: true\n  url: 'https://example.invalid/hook'\n  headers:\n    Authorization: \"Bearer ${INSTALL_MISSING_SECRET}\\ninvalid\"\n",
		"slack:\n  enabled: true\n  webhook_url: '${INSTALL_MISSING_SECRET}'\n  timeout: bananas\n",
		"logging:\n  level: 'invalid${INSTALL_MISSING_SECRET}'\n",
		"providers:\n  - slug: '${INSTALL_MISSING_SECRET}'\n  - slug: INVALID\n",
	}
	for _, body := range cases {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := WriteNewBytes(path, []byte("config_version: 2\n"+body)); err != nil {
			t.Fatal(err)
		}
		if deferred, err := ValidateInstallation(path, nil); err == nil {
			t.Fatalf("literal defect hidden by deferred secret: %s, %v", body, deferred)
		}
	}
}

func TestInstallationRejectsMalformedYAMLAndUnknownFields(t *testing.T) {
	for _, body := range []string{
		"config_version: 2\ntelegram: [\n",
		"config_version: 2\nunknown_secret_key: private-value\n",
		"config_version: 2\ntelegram:\n  enabled: '${INSTALL_MISSING_SECRET}'\n",
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := WriteNewBytes(path, []byte(body)); err != nil {
			t.Fatal(err)
		}
		if _, err := ValidateInstallation(path, nil); err == nil || strings.Contains(err.Error(), "private-value") {
			t.Fatalf("bad YAML not safely rejected: %v", err)
		}
	}
}

func TestInstallationUsesActualServiceEnvironmentAndOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := []byte("config_version: 2\ntelegram:\n  enabled: true\n  bot_token: '${INSTALL_SERVICE_TOKEN}'\n  chat_id: '42'\n")
	if err := WriteNewBytes(path, body); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INSTALL_SERVICE_TOKEN", "123:real-service-value")
	if deferred, err := ValidateInstallation(path, nil); err != nil || len(deferred) != 0 {
		t.Fatalf("resolved service value was deferred: %v %v", deferred, err)
	}
	cfg, err := Load(path, nil)
	if err != nil || Validate(cfg) != nil {
		t.Fatal("actual service configuration is not runnable")
	}
	t.Setenv("TRM_TELEGRAM_BOT_TOKEN", "invalid token with spaces")
	if _, err := ValidateInstallation(path, nil); err == nil || strings.Contains(err.Error(), "invalid token with spaces") {
		t.Fatalf("invalid effective env override was hidden: %v", err)
	}
	if deferred, err := ValidateInstallation(path, map[string]string{"telegram_bot_token": "456:cli-value"}); err != nil || len(deferred) != 0 {
		t.Fatalf("CLI precedence lost: %v %v", deferred, err)
	}
}

func TestInstallationChecksPartialURLAndEnumReferences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := []byte("config_version: 2\nwebhook:\n  enabled: true\n  url: 'https://${INSTALL_HOST}:${INSTALL_PORT}/hook'\nlogging:\n  level: 'deb${INSTALL_LEVEL_SUFFIX}'\n")
	if err := WriteNewBytes(path, body); err != nil {
		t.Fatal(err)
	}
	deferred, err := ValidateInstallation(path, nil)
	if err != nil || !reflect.DeepEqual(deferred, []string{"logging.level", "webhook.url"}) {
		t.Fatalf("partial references: %v %v", deferred, err)
	}
}
