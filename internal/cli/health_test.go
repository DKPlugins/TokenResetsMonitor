package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
)

func TestHealthAndStatusWorkWithBrokenConfigurationAndNoDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	configPath := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(configPath, []byte("invalid: ["), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	s := model.Status{Running: true, UpdatedAt: now, Runtime: &model.RuntimeStatus{PollIntervalSeconds: 60}, Providers: map[string]model.ProviderStatus{"p": {Ready: true, LastSuccess: &now}}}
	save := func() {
		data, _ := json.Marshal(s)
		if err := os.WriteFile(path+".status.json", data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	save()
	for _, command := range []string{"status", "healthcheck"} {
		var out, errOut bytes.Buffer
		code := Execute(context.Background(), []string{command, "--config", configPath, "--state-path", path, "--json"}, strings.NewReader(""), &out, &errOut)
		if code != 0 || !json.Valid(out.Bytes()) {
			t.Fatalf("%s code=%d out=%s err=%s", command, code, &out, &errOut)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("diagnostics created or opened a state database")
	}
	for _, state := range []string{"stopped", "stale", "future"} {
		s.Running = true
		s.UpdatedAt = now
		switch state {
		case "stopped":
			s.Running = false
		case "stale":
			s.UpdatedAt = now.Add(-61 * time.Second)
		case "future":
			s.UpdatedAt = now.Add(time.Minute)
		}
		save()
		var out, errOut bytes.Buffer
		if code := Execute(context.Background(), []string{"healthcheck", "--state-path", path}, strings.NewReader(""), &out, &errOut); code != 1 {
			t.Fatalf("%s accepted as healthy: %d", state, code)
		}
	}
}

func TestStatusDoesNotResolveChannelSecrets(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.StatePath = filepath.Join(dir, "state.db")
	cfg.Telegram.Enabled = true
	cfg.Telegram.BotToken = "${TRM_INTENTIONALLY_ABSENT_HEALTH_SECRET}"
	cfg.Telegram.ChatID = "123"
	path := filepath.Join(dir, "config.yaml")
	if err := config.WriteNew(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(model.Status{Running: true, UpdatedAt: time.Now()})
	if err := os.WriteFile(cfg.StatePath+".status.json", raw, 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Execute(context.Background(), []string{"status", "--config", path}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("status required secret: %s", &errOut)
	}
}

func TestCheckUpdateDoesNotRequireChannelSecrets(t *testing.T) {
	cfg := config.Defaults()
	cfg.Telegram.Enabled = true
	cfg.Telegram.BotToken = "${TRM_INTENTIONALLY_ABSENT_UPDATE_SECRET}"
	cfg.Telegram.ChatID = "123"
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.WriteNew(path, cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // No network: reaching the checker must report unavailable, not invalid credentials.
	var out, errOut bytes.Buffer
	code := Execute(ctx, []string{"check-update", "--config", path, "--json"}, strings.NewReader(""), &out, &errOut)
	var result struct {
		Error string `json:"error"`
	}
	if code != 1 || json.Unmarshal(out.Bytes(), &result) != nil || result.Error == "" {
		t.Fatalf("update check required channel credentials: code=%d out=%s err=%s", code, &out, &errOut)
	}
	if strings.Contains(out.String()+errOut.String(), "TRM_INTENTIONALLY_ABSENT_UPDATE_SECRET") {
		t.Fatal("notification secret name escaped into unrelated diagnostics")
	}
}
