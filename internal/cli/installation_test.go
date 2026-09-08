package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
)

func TestStructuralValidationDefersSecretsButRunStillRejectsThem(t *testing.T) {
	cfg := config.Defaults()
	cfg.Telegram.Enabled = true
	cfg.Telegram.BotToken = "${TRM_ABSENT_STRUCTURAL_TEST_SECRET}"
	cfg.Telegram.ChatID = "123"
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.WriteNew(path, cfg); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"config", "validate", "--structural", "--config", path}, 0},
		{[]string{"config", "validate", "--config", path}, 2},
		{[]string{"run", "--once", "--config", path}, 2},
		{[]string{"config", "validate", "--structural", "--config", path, "--poll-interval", "1s"}, 2},
	} {
		var out, errOut bytes.Buffer
		got := Execute(context.Background(), tc.args, strings.NewReader(""), &out, &errOut)
		if got != tc.code {
			t.Fatalf("%v: got %d want %d: %s %s", tc.args, got, tc.code, &out, &errOut)
		}
		if got == 0 && !strings.Contains(out.String(), "deferred") {
			t.Fatal("missing service environment reported as fully validated")
		}
	}
}
