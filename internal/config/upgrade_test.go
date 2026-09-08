package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionOneLoadsInMemoryWithVersionTwoDefaults(t *testing.T) {
	data := []byte("config_version: 1\nstate_path: data/state.db\n")
	before := bytes.Clone(data)
	cfg, err := LoadBytes(data, filepath.Join(t.TempDir(), "config.yaml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfigVersion != Version || !cfg.Reload.Enabled || !cfg.Updates.Enabled || cfg.Observability.Enabled || cfg.Slack.Timeout != "15s" {
		t.Fatalf("legacy defaults not applied: %+v", cfg)
	}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, before) {
		t.Fatal("load changed the supplied bytes")
	}
}

func TestValidationExecutesWebhookTemplateOffline(t *testing.T) {
	cfg := Defaults()
	cfg.Webhook.Enabled = true
	cfg.Webhook.URL = "https://example.invalid/secret"
	for _, body := range []string{"{{.Event.DoesNotExist}}", "secret-template-contents {{.Event.PublishedAt.Format \"2006\"}}"} {
		cfg.Webhook.BodyTemplate = body
		err := Validate(cfg)
		if err == nil || strings.Contains(err.Error(), "secret-template-contents") {
			t.Fatalf("bad validation: %v", err)
		}
	}
	cfg.Webhook.BodyTemplate = "{{if .Event.PublishedAt}}{{.Event.PublishedAt.Format \"2006\"}}{{end}}{{json .Event.Title}}"
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Webhook.BodyTemplate = strings.Repeat("x", MaxTemplateBytes+1)
	if err := Validate(cfg); err == nil {
		t.Fatal("unbounded template accepted")
	}
}

func TestStructuralLoadDoesNotRequireNotificationSecrets(t *testing.T) {
	t.Setenv("TRM_UPDATES_INCLUDE_PRERELEASE", "true")
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte("config_version: 2\nstate_path: state.db\ntelegram:\n  enabled: true\n  bot_token: '${THIS_TRM_SECRET_IS_NOT_SET}'\n")
	if err := WriteNewBytes(path, original); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, nil); err == nil {
		t.Fatal("full load accepted missing secret")
	}
	cfg, err := LoadStructural(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Updates.IncludePrerelease {
		t.Fatal("updates environment override was not applied")
	}
	if cfg.StatePath != filepath.Join(filepath.Dir(path), "state.db") {
		t.Fatal("wrong diagnostic path")
	}
}

func TestSaveChannelPreservesUnrelatedReferencesAndConcurrentEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte("# user comment\nconfig_version: 1\nwebhook:\n  url: '${PRIVATE_WEBHOOK}'\ntelegram:\n  bot_token: '${BOT_TOKEN}'\n")
	if err := WriteNewBytes(path, original); err != nil {
		t.Fatal(err)
	}
	slack := Slack{Enabled: true, WebhookURL: "${SLACK_SECRET}", Timeout: "15s"}
	if err := SaveChannel(path, "slack", slack, original); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"# user comment", "config_version: 3", "${PRIVATE_WEBHOOK}", "${BOT_TOKEN}", "${SLACK_SECRET}"} {
		if !bytes.Contains(current, []byte(expected)) {
			t.Fatalf("lost %s", expected)
		}
	}
	backups, err := filepath.Glob(path + ".backup-*")
	if err != nil || len(backups) != 1 {
		t.Fatal("backup missing")
	}
	backup, err := os.ReadFile(backups[0])
	if err != nil || !bytes.Equal(backup, original) {
		t.Fatal("backup did not preserve original bytes")
	}
	if err := SaveChannel(path, "slack", Slack{}, original); err == nil {
		t.Fatal("stale setup overwrote new settings")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(after, current) {
		t.Fatal("concurrent-edit refusal mutated configuration")
	}
}

func TestNewOperationalSettingsRejectInvalidValues(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Observability.Listen = "localhost" },
		func(c *Config) { c.Observability.Listen = "localhost:65536" },
		func(c *Config) { c.Updates.Interval = "1s" },
		func(c *Config) { c.Slack.Enabled = true },
	} {
		cfg := Defaults()
		mutate(&cfg)
		if Validate(cfg) == nil {
			t.Fatal("invalid settings accepted")
		}
	}
}
