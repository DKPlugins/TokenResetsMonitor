package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
)

func TestNotificationUsesRealTransportWithoutState(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("X-TokenResetsMonitor-Test") != "true" {
			t.Error("test header missing")
		}
		var n model.Notification
		if json.NewDecoder(r.Body).Decode(&n) != nil || !n.Test {
			t.Error("test payload missing")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.StatePath = filepath.Join(dir, "state.db")
	cfg.Webhook.URL = srv.URL
	cfg.Webhook.Enabled = false
	path := filepath.Join(dir, "config.yaml")
	if e := config.WriteNew(path, cfg); e != nil {
		t.Fatal(e)
	}
	var out, errOut bytes.Buffer
	args := []string{"test-notification", "webhook", "--config", path, "--dry-run"}
	if code := Execute(context.Background(), args, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("dry run=%d %s", code, errOut.String())
	}
	if calls.Load() != 0 {
		t.Fatal("dry run sent network request")
	}
	args = args[:len(args)-1]
	if code := Execute(context.Background(), args, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("send=%d %s", code, errOut.String())
	}
	if calls.Load() != 1 {
		t.Fatal("test did not make exactly one request")
	}
	if _, e := os.Stat(cfg.StatePath); !os.IsNotExist(e) {
		t.Fatal("test notification touched state")
	}
}

func TestAllChannelsReportIndependentResults(t *testing.T) {
	var tgCalls atomic.Int32
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer webhook.Close()
	telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tgCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer telegram.Close()
	cfg := config.Defaults()
	cfg.Webhook.Enabled = true
	cfg.Webhook.URL = webhook.URL
	cfg.Telegram.Enabled = true
	cfg.Telegram.APIBaseURL = telegram.URL
	cfg.Telegram.BotToken = "123456:TESTSECRETTOKEN"
	cfg.Telegram.ChatID = "1234"
	path := filepath.Join(t.TempDir(), "config.yaml")
	_ = config.WriteNew(path, cfg)
	var out, errOut bytes.Buffer
	code := Execute(context.Background(), []string{"test-notification", "all", "--config", path}, strings.NewReader(""), &out, &errOut)
	if code != 1 || tgCalls.Load() != 1 || !strings.Contains(out.String(), "telegram: TEST delivered") {
		t.Fatalf("independence failed %d %s %s", code, out.String(), errOut.String())
	}
	if strings.Contains(errOut.String(), cfg.Telegram.BotToken) {
		t.Fatal("token leaked")
	}
}

func TestInvalidArgumentsAndInitNoOverwrite(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Execute(context.Background(), []string{"test-notification"}, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Fatal("missing channel not usage error")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	args := []string{"init", "--defaults", "--config", path}
	if code := Execute(context.Background(), args, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("init failed %s", errOut.String())
	}
	original, _ := os.ReadFile(path)
	if code := Execute(context.Background(), args, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Fatal("init overwrote existing file")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(original, after) {
		t.Fatal("config changed")
	}
}

func TestInitDoesNotPersistEnvironmentCredentials(t *testing.T) {
	t.Setenv("TRM_WEBHOOK_URL", "https://example.invalid/secret-url-token")
	t.Setenv("TRM_WEBHOOK_HEADERS", `{"Authorization":"Bearer secret-header-value"}`)
	t.Setenv("TRM_TELEGRAM_BOT_TOKEN", "123456:secret-bot-token")
	t.Setenv("TRM_TELEGRAM_CHAT_ID", "secret-chat-id")
	path := filepath.Join(t.TempDir(), "config.yaml")
	var out, errOut bytes.Buffer
	if code := Execute(context.Background(), []string{"init", "--defaults", "--config", path}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("init %d: %s", code, errOut.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-url-token", "secret-header-value", "secret-bot-token", "secret-chat-id"} {
		if bytes.Contains(data, []byte(secret)) || strings.Contains(out.String(), secret) {
			t.Fatalf("init persisted/exposed %s", secret)
		}
	}
	if !bytes.Contains(data, []byte("${TRM_WEBHOOK_URL}")) || !bytes.Contains(data, []byte("${TRM_TELEGRAM_BOT_TOKEN}")) {
		t.Fatal("environment references not preserved")
	}
	loaded, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Webhook.Headers["Authorization"] != "Bearer secret-header-value" {
		t.Fatal("runtime header override lost")
	}
}

func TestTemplateRenderingErrorIsUsageErrorAndOtherChannelStillTests(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.Write([]byte(`{"ok":true}`)) }))
	defer server.Close()
	cfg := config.Defaults()
	cfg.Webhook.Enabled = true
	cfg.Webhook.URL = server.URL
	cfg.Webhook.BodyTemplate = `{{.MissingField}}`
	cfg.Telegram.Enabled = true
	cfg.Telegram.APIBaseURL = server.URL
	cfg.Telegram.BotToken = "123:token"
	cfg.Telegram.ChatID = "456"
	cfg.StatePath = filepath.Join(t.TempDir(), "absent-state.db")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.WriteNew(path, cfg); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := Execute(context.Background(), []string{"test-notification", "all", "--config", path}, strings.NewReader(""), &out, &errOut)
	if code != 2 || requests.Load() != 1 || !strings.Contains(out.String(), "telegram: TEST delivered") {
		t.Fatalf("channel independence/render code: %d %d %s %s", code, requests.Load(), out.String(), errOut.String())
	}
	if _, err := os.Stat(cfg.StatePath); !os.IsNotExist(err) {
		t.Fatal("test touched state")
	}
}

func TestCommandHelpAndUnsupportedVersionFlags(t *testing.T) {
	for _, args := range [][]string{{"run", "--help"}, {"test-notification", "webhook", "--help"}, {"config", "--help"}} {
		var out, errOut bytes.Buffer
		if code := Execute(context.Background(), args, strings.NewReader(""), &out, &errOut); code != 0 || !strings.Contains(out.String(), "Usage:") {
			t.Fatalf("help %v returned %d", args, code)
		}
	}
	var out, errOut bytes.Buffer
	if code := Execute(context.Background(), []string{"version", "--bad"}, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Fatal("invalid version arguments silently accepted")
	}
}
