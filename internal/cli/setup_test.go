package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
)

func TestSlackSetupPreservesReferenceAndDoesNotSendByDefault(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; fmt.Fprint(w, "ok") }))
	defer server.Close()
	t.Setenv("FIXTURE_SLACK_URL", server.URL+"/private-hook")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.WriteNew(path, config.Defaults()); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	input := strings.NewReader("${FIXTURE_SLACK_URL}\nyes\nno\n")
	if err := setupChannel(context.Background(), options{configPath: path}, "slack", input, &out); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if requests != 0 || !bytes.Contains(data, []byte("${FIXTURE_SLACK_URL}")) || bytes.Contains(data, []byte("private-hook")) || strings.Contains(out.String(), "private-hook") {
		t.Fatal("setup sent unexpectedly or materialized/exposed the secret")
	}
	cfg, err := config.Load(path, nil)
	if err != nil || !cfg.Slack.Enabled {
		t.Fatal("Slack was not enabled")
	}
}

func TestTelegramSetupKeepsWebhookAndManualTopic(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		methods = append(methods, method)
		switch method {
		case "getMe":
			fmt.Fprint(w, `{"ok":true,"result":{"id":1,"username":"fixture_bot"}}`)
		case "getWebhookInfo":
			fmt.Fprint(w, `{"ok":true,"result":{"url":"https://private-hook.invalid"}}`)
		default:
			t.Errorf("unexpected method %s", method)
			http.Error(w, "bad", 400)
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := config.Defaults()
	cfg.Telegram.APIBaseURL = server.URL
	if err := config.WriteNew(path, cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIXTURE_BOT_TOKEN", "123:private-token")
	var out bytes.Buffer
	input := strings.NewReader("${FIXTURE_BOT_TOKEN}\n-123456\n42\nyes\nno\n")
	if err := setupChannel(context.Background(), options{configPath: path}, "telegram", input, &out); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path, nil)
	if err != nil || !loaded.Telegram.Enabled || loaded.Telegram.ChatID != "-123456" || loaded.Telegram.MessageThreadID != 42 {
		t.Fatalf("bad setup: %v", err)
	}
	if strings.Contains(out.String(), "private-token") || strings.Contains(out.String(), "private-hook") {
		t.Fatal("setup exposed credentials")
	}
	if len(methods) != 2 {
		t.Fatal("setup altered bot updates or sent without permission")
	}
	data, _ := os.ReadFile(path)
	if !bytes.Contains(data, []byte("${FIXTURE_BOT_TOKEN}")) || bytes.Contains(data, []byte("private-token")) {
		t.Fatal("reference was materialized")
	}
}

func TestSecretPromptNeverDisplaysExistingValue(t *testing.T) {
	var out bytes.Buffer
	value, err := newSetupPrompter(strings.NewReader("\n"), &out).secret("Secret", "do-not-print")
	if err != nil || value != "do-not-print" || strings.Contains(out.String(), value) {
		t.Fatalf("unsafe secret prompt %q %v", out.String(), err)
	}
}
