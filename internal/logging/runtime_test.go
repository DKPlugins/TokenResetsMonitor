package logging

import (
	"bytes"
	"strings"
	"testing"

	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
)

func TestRuntimeLevelAndSecretRotation(t *testing.T) {
	cfg := config.Defaults()
	cfg.Telegram.BotToken = "old-private-credential"
	var out bytes.Buffer
	logger, control, closeFn, err := NewRuntime(cfg, &out)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	logger.Debug("before debug change")
	next := cfg
	next.Logging.Level = "debug"
	next.Telegram.BotToken = "new-private-credential"
	next.Slack.WebhookURL = "https://hooks.slack.invalid/secret"
	if err := control.Apply(next); err != nil {
		t.Fatal(err)
	}
	logger.Debug("after debug change", "detail", cfg.Telegram.BotToken+" "+next.Telegram.BotToken+" "+next.Slack.WebhookURL)
	result := out.String()
	if strings.Contains(result, "before debug change") || !strings.Contains(result, "after debug change") {
		t.Fatal("live level change failed")
	}
	for _, secret := range []string{cfg.Telegram.BotToken, next.Telegram.BotToken, next.Slack.WebhookURL} {
		if strings.Contains(result, secret) {
			t.Fatal("secret rotation leaked credential")
		}
	}
	next.Logging.Directory = "other-output"
	if err := control.Apply(next); err == nil {
		t.Fatal("output change was silently accepted")
	}
}
