package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/buildinfo"
	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
)

func TestDoctorOfflineDoesNotOpenOrMigrateDatabase(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.StatePath = filepath.Join(dir, "state.db")
	original := []byte("this is intentionally not a bbolt database")
	if err := os.WriteFile(cfg.StatePath, original, 0600); err != nil {
		t.Fatal(err)
	}
	snapshot := map[string]any{"running": true, "updated_at": time.Now().UTC(), "providers": map[string]any{}, "runtime": map[string]any{"config_version": 2, "state_schema_version": 2}}
	raw, _ := json.Marshal(snapshot)
	if err := os.WriteFile(cfg.StatePath+".status.json", raw, 0600); err != nil {
		t.Fatal(err)
	}
	// An unreachable URL makes an accidental online check fail.
	cfg.APIBaseURL = "http://127.0.0.1:1/api/v1"
	var out, errOut bytes.Buffer
	if code := doctor(context.Background(), cfg, true, false, &out, &errOut); code != 0 {
		t.Fatalf("doctor exit %d: %s %s", code, out.String(), errOut.String())
	}
	after, err := os.ReadFile(cfg.StatePath)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatal("doctor touched the database")
	}
	var report doctorReport
	if json.Unmarshal(out.Bytes(), &report) != nil {
		t.Fatal("invalid JSON report")
	}
	found := false
	for _, check := range report.Checks {
		if check.Name == "state_schema" && check.Status == "ok" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no snapshot-based schema result: %s", out.String())
	}
}

func TestDoctorReportsConfigurationErrorWithoutEchoingSecret(t *testing.T) {
	cfg := config.Defaults()
	cfg.Telegram.BotToken = "123456789:SECRET_TEST_TOKEN_abcdefghijklmnopqrstuvwxyz"
	var out, errOut bytes.Buffer
	code := doctor(context.Background(), cfg, true, false, &out, &errOut, errors.New("invalid token "+cfg.Telegram.BotToken))
	if code != 1 || bytes.Contains(out.Bytes(), []byte(cfg.Telegram.BotToken)) {
		t.Fatalf("unsafe report: %s", out.String())
	}
}

func TestDoctorDoesNotCallStaleSnapshotCompatible(t *testing.T) {
	for _, schema := range []int{0, 2, 99} {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.db")
		raw, _ := json.Marshal(map[string]any{"running": true, "updated_at": time.Now().Add(-time.Hour), "runtime": map[string]any{"state_schema_version": schema}})
		if err := os.WriteFile(path+".status.json", raw, 0600); err != nil {
			t.Fatal(err)
		}
		var checks []doctorCheck
		inspectSnapshot(path, buildinfo.CurrentManifest(), func(name, status, message string) {
			checks = append(checks, doctorCheck{Name: name, Status: status, Message: message})
		})
		for _, check := range checks {
			if check.Name == "state_schema" && check.Status == "ok" {
				t.Fatal("stale snapshot was reported as confirmed compatible")
			}
		}
	}
}
