package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOverridesAndEnvironmentReferences(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := Defaults()
	cfg.PollInterval = "40m"
	cfg.StatePath = "data/state.db"
	cfg.Webhook.Headers = map[string]string{"Authorization": "Bearer ${TEST_TRM_TOKEN}"}
	if e := WriteNew(path, cfg); e != nil {
		t.Fatal(e)
	}
	t.Setenv("TEST_TRM_TOKEN", "abc: 'quoted'\nsecond line")
	t.Setenv("TRM_POLL_INTERVAL", "20m")
	t.Setenv("TRM_LOGGING_MAX_BACKUPS", "3")
	loaded, e := Load(path, map[string]string{"poll_interval": "10m"})
	if e != nil {
		t.Fatal(e)
	}
	if loaded.PollInterval != "10m" || loaded.Logging.MaxBackups != 3 {
		t.Fatal("override precedence failed")
	}
	if loaded.Webhook.Headers["Authorization"] != "Bearer abc: 'quoted'\nsecond line" {
		t.Fatal("parsed string interpolation changed the value")
	}
	if loaded.StatePath != filepath.Join(dir, "data", "state.db") {
		t.Fatal("relative state did not resolve against config directory")
	}
	data, _ := os.ReadFile(path)
	if !bytes.Contains(data, []byte("${TEST_TRM_TOKEN}")) {
		t.Fatal("load rewrote secret reference")
	}
}

func TestInvalidSettingsAreRedacted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	_ = os.WriteFile(path, []byte("config_version: 1\nunknown_secret_key: super-secret-value\n"), 0600)
	_, e := Load(path, nil)
	if e == nil || strings.Contains(e.Error(), "super-secret-value") {
		t.Fatal("unknown field not safely rejected")
	}
	cfg := Defaults()
	cfg.Webhook.Enabled = true
	cfg.Webhook.URL = "https://user:super-secret-value@example.test/"
	if e = Validate(cfg); e == nil || strings.Contains(e.Error(), "super-secret-value") {
		t.Fatal("invalid credential URL not safely rejected")
	}
	if e = WriteNew(path, Defaults()); e == nil {
		t.Fatal("existing config overwritten")
	}
}

func TestMigrationPreviewBackupAndFutureRefusal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte("# keep this comment\nconfig_version: 0\nwebhook:\n  headers:\n    Authorization: '${TOKEN}'\n")
	_ = WriteNewBytes(path, original)
	if _, e := Migrate(path, false); e != nil {
		t.Fatal(e)
	}
	data, _ := os.ReadFile(path)
	if !bytes.Equal(data, original) {
		t.Fatal("preview mutated configuration")
	}
	if _, e := Migrate(path, true); e != nil {
		t.Fatal(e)
	}
	data, _ = os.ReadFile(path)
	if !bytes.Contains(data, []byte("${TOKEN}")) || !bytes.Contains(data, []byte("keep this comment")) {
		t.Fatal("migration changed references/comments")
	}
	backups, _ := filepath.Glob(path + ".backup-*")
	if len(backups) != 1 {
		t.Fatal("missing migration backup")
	}
	backup, _ := os.ReadFile(backups[0])
	if !bytes.Equal(backup, original) {
		t.Fatal("backup differs")
	}
	future := []byte("config_version: 999\n")
	_ = os.WriteFile(path, future, 0600)
	if _, e := Migrate(path, true); e == nil {
		t.Fatal("future schema accepted")
	}
	data, _ = os.ReadFile(path)
	if !bytes.Equal(data, future) {
		t.Fatal("future schema modified")
	}
}

func TestMinimumPollAndDisabledChannels(t *testing.T) {
	cfg := Defaults()
	if e := Validate(cfg); e != nil {
		t.Fatal(e)
	}
	cfg.PollInterval = "59s"
	if e := Validate(cfg); e == nil {
		t.Fatal("too-fast interval accepted")
	}
	cfg.PollInterval = "1m"
	cfg.Webhook.Enabled = true
	if e := Validate(cfg); e == nil {
		t.Fatal("enabled channel without URL accepted")
	}
}

func TestCollectionOverridesReplaceRatherThanRetainOldSecrets(t *testing.T) {
	cfg := Defaults()
	cfg.Webhook.Headers = map[string]string{"Authorization": "Bearer obsolete-secret", "X-Remove": "old"}
	t.Setenv("TRM_WEBHOOK_HEADERS", `{"X-New":"value"}`)
	if err := ApplyEnvironment(&cfg, nil); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Webhook.Headers) != 1 || cfg.Webhook.Headers["X-New"] != "value" {
		t.Fatal("old header credentials survived replacement")
	}
}

func TestProviderOverrideRejectsMisspelledFilterFields(t *testing.T) {
	cfg := Defaults()
	t.Setenv("TRM_PROVIDERS", `[{slug: openai-codex, plnas: [plus]}]`)
	if err := ApplyEnvironment(&cfg, nil); err == nil {
		t.Fatal("misspelled plan filter became an unrestricted provider")
	}
}

func TestOversizedConfigAndURLQueryAreRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 1024*1024+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, nil); err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Fatalf("oversized config: %v", err)
	}
	cfg := Defaults()
	cfg.APIBaseURL = "https://tokenresets.com/api/v1?secret=value"
	if err := Validate(cfg); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("bad API URL: %v", err)
	}
}

func TestMissingConfigurationVersionRequiresExplicitMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte("poll_interval: 30m\n")
	if err := WriteNewBytes(path, original); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, nil); err == nil || !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("unversioned config was accepted: %v", err)
	}
	if _, err := Migrate(path, true); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(path, nil); err != nil || cfg.ConfigVersion != Version {
		t.Fatalf("explicit migration failed: %v", err)
	}
}
