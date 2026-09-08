package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestVersionThreeSettingsAndLegacyDefaults(t *testing.T) {
	for _, version := range []string{"1", "2", "3"} {
		cfg, err := LoadBytes([]byte("config_version: "+version+"\n"), "config.yaml", nil)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ConfigVersion != 3 || !cfg.Telegram.NotifyChanges || !cfg.Slack.NotifyChanges || cfg.Webhook.NotifyChanges || cfg.History.RetentionDays != 0 {
			t.Fatalf("incorrect migration defaults for config %s", version)
		}
	}
	cfg, err := LoadBytes([]byte("config_version: 3\ntelegram:\n  notify_changes: false\nslack:\n  notify_changes: false\nwebhook:\n  notify_changes: true\nhistory:\n  retention_days: 90\n"), "config.yaml", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telegram.NotifyChanges || cfg.Slack.NotifyChanges || !cfg.Webhook.NotifyChanges || cfg.History.RetentionDays != 90 {
		t.Fatal("explicit settings lost")
	}
	for _, days := range []int{-1, 36501} {
		cfg.History.RetentionDays = days
		if Validate(cfg) == nil {
			t.Fatalf("invalid retention accepted: %d", days)
		}
	}
}

func TestFilterPreviewLoadsWithoutChannelCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "candidate.yaml")
	original := []byte("config_version: 2\nstate_path: history.db\nminimum_confidence: verified\ntelegram:\n  enabled: true\n  bot_token: '${TRM_PREVIEW_MISSING_TOKEN}'\nwebhook:\n  headers:\n    Authorization: '${TRM_PREVIEW_MISSING_HEADER}'\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRM_UNKNOWN_SCOPE", "exclude")
	spec, err := LoadFilterSpec(path)
	if err != nil {
		t.Fatal(err)
	}
	if spec.MinimumConfidence != "verified" || spec.UnknownScope != "exclude" {
		t.Fatalf("wrong filters: %+v", spec)
	}
	cfg, err := LoadManagement(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.FilterSpec(), spec) || cfg.StatePath != filepath.Join(filepath.Dir(path), "history.db") {
		t.Fatal("management resolution differs")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(original) {
		t.Fatal("preview changed configuration")
	}
	if _, err := Load(path, nil); err == nil {
		t.Fatal("strict loading must still reject missing credentials")
	}
	spec.MinimumConfidence = "not-a-level"
	if spec.Validate() == nil {
		t.Fatal("invalid filter accepted")
	}
}

func TestTemplateValidationCoversCorrectionBranches(t *testing.T) {
	if err := ValidateTemplate(`{{if eq .Kind "correction"}}{{.MissingAmendmentField}}{{end}}`); err == nil {
		t.Fatal("invalid amendment branch escaped validation")
	}
	if err := ValidateTemplate(`{{.Kind}}{{with .PreviousEvent}}{{.Status}}{{end}}{{range .Changes}}{{.Field}}: {{.Before}} -> {{.After}}{{end}}`); err != nil {
		t.Fatal("guarded amendment template rejected", err)
	}
}
