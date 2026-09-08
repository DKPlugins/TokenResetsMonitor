package monitor_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
)

// TestBinaryUpgradeRollbackAcceptance is an opt-in acceptance test against two
// actual release executables. It never installs or changes an OS service.
//
// Build the earlier binary from its immutable tag and the candidate from the
// source under review, then set TRM_ACCEPTANCE_OLD_BINARY and
// TRM_ACCEPTANCE_NEW_BINARY to their absolute paths and run:
//
//	go test -count=1 -v -timeout 2m ./internal/monitor -run '^TestBinaryUpgradeRollbackAcceptance$'
//
// Normal unit-test runs skip this check when neither binary is supplied.
func TestBinaryUpgradeRollbackAcceptance(t *testing.T) {
	oldPath := os.Getenv("TRM_ACCEPTANCE_OLD_BINARY")
	newPath := os.Getenv("TRM_ACCEPTANCE_NEW_BINARY")
	if oldPath == "" && newPath == "" {
		t.Skip("set TRM_ACCEPTANCE_OLD_BINARY and TRM_ACCEPTANCE_NEW_BINARY to run cross-version binary acceptance")
	}
	if oldPath == "" || newPath == "" {
		t.Fatal("both acceptance binary paths must be supplied")
	}
	for _, path := range []string{oldPath, newPath} {
		if !filepath.IsAbs(path) {
			t.Fatal("acceptance binary paths must be absolute")
		}
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			t.Fatalf("acceptance binary is unavailable: %s", path)
		}
	}
	workspace := t.TempDir()
	// Never allow the developer's runtime overrides or secrets to redirect these
	// child processes away from the isolated fixture and state directory.
	var environment []string
	for _, value := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(value), "TRM_") {
			environment = append(environment, value)
		}
	}
	run := func(binary string, wantExit int, arguments ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary, arguments...)
		command.Env = environment
		command.Dir = workspace
		output, err := command.CombinedOutput()
		if ctx.Err() != nil {
			t.Fatalf("binary command did not finish: %s: %v", arguments[0], ctx.Err())
		}
		code := 0
		if err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				code = exit.ExitCode()
			} else {
				t.Fatalf("cannot execute acceptance binary: %v", err)
			}
		}
		if code != wantExit {
			t.Fatalf("%s returned %d, want %d:\n%s", arguments[0], code, wantExit, output)
		}
		return output
	}
	version := func(binary string) map[string]string {
		t.Helper()
		var result map[string]string
		if err := json.Unmarshal(run(binary, 0, "version", "--json"), &result); err != nil {
			t.Fatal("invalid binary version output", err)
		}
		if result["version"] == "" || result["commit"] == "" || result["commit"] == "development" {
			t.Fatal("acceptance binaries must contain version and source commit metadata")
		}
		return result
	}
	oldVersion, newVersion := version(oldPath), version(newPath)
	if oldVersion["version"] == newVersion["version"] {
		t.Fatal("cross-version acceptance requires different application versions")
	}
	checksum := func(path string) string {
		t.Helper()
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		hash := sha256.New()
		if _, err := io.Copy(hash, file); err != nil {
			t.Fatal(err)
		}
		return hex.EncodeToString(hash.Sum(nil))
	}
	t.Logf("old version=%s commit=%s sha256=%s", oldVersion["version"], oldVersion["commit"], checksum(oldPath))
	t.Logf("new version=%s commit=%s sha256=%s", newVersion["version"], newVersion["commit"], checksum(newPath))

	fixtureEvent := func(id string) model.Event {
		return model.Event{ID: id, Slug: id, Provider: model.Provider{Slug: "openai-codex", Name: "Codex"},
			Status: "published", EventType: "hard_reset", Revision: 1, Title: "Acceptance fixture",
			AnnouncedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), Confidence: model.Confidence{Label: "reported"}}
	}
	var mu sync.Mutex
	events := []model.Event{fixtureEvent("historical-baseline")}
	var notifications []model.Notification
	var idempotencyKeys []string
	failDelivery := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/providers/openai-codex/events":
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": events,
				"pagination": map[string]any{"has_more": false}, "meta": map[string]string{"schema_version": "1.2"}})
		case "/webhook":
			var notification model.Notification
			if err := json.NewDecoder(r.Body).Decode(&notification); err != nil {
				t.Error("invalid notification JSON", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			notifications = append(notifications, notification)
			idempotencyKeys = append(idempotencyKeys, r.Header.Get("Idempotency-Key"))
			if failDelivery {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected acceptance request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	cfg := config.Defaults()
	cfg.APIBaseURL = server.URL + "/api/v1"
	cfg.StatePath = filepath.Join(workspace, "state.db")
	cfg.Providers = []config.ProviderFilter{{Slug: "openai-codex"}}
	cfg.Webhook.Enabled = true
	cfg.Webhook.URL = server.URL + "/webhook"
	cfg.Webhook.Timeout = "5s"
	cfg.Telegram.Enabled = false
	cfg.Logging.FileEnabled = false
	configPath := filepath.Join(workspace, "config.yaml")
	if err := config.WriteNew(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	status := func(binary string) model.Status {
		t.Helper()
		var result model.Status
		if err := json.Unmarshal(run(binary, 0, "status", "--json", "--config", configPath), &result); err != nil {
			t.Fatal("invalid status output", err)
		}
		return result
	}
	assertStopped := func(result model.Status, pending int) {
		t.Helper()
		if result.Running || result.Pending != pending || result.Failed != 0 || !result.Providers["openai-codex"].Ready {
			t.Fatalf("unexpected persisted monitor status: %+v", result)
		}
	}
	run(oldPath, 0, "run", "--once", "--config", configPath)
	assertStopped(status(oldPath), 0)
	mu.Lock()
	if len(notifications) != 0 {
		mu.Unlock()
		t.Fatal("old binary replayed baseline history")
	}
	events = append(events, fixtureEvent("pending-upgrade-event"))
	mu.Unlock()
	run(oldPath, 1, "run", "--once", "--config", configPath)
	assertStopped(status(oldPath), 1)
	mu.Lock()
	if len(notifications) != 1 || notifications[0].Event.ID != "pending-upgrade-event" || notifications[0].Test || idempotencyKeys[0] == "" || idempotencyKeys[0] != notifications[0].ID {
		mu.Unlock()
		t.Fatal("old binary did not persist the intended production delivery")
	}
	firstID := idempotencyKeys[0]
	failDelivery = false
	mu.Unlock()
	// The child exited before copying. These are real stopped-process backups;
	// no test code opens, migrates, or rewrites bbolt records directly.
	backup := make(map[string][]byte)
	for _, path := range []string{cfg.StatePath, cfg.StatePath + ".status.json", configPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		backup[path] = data
	}
	assertStopped(status(newPath), 1)
	// Retry timing belongs to the binaries. Repeated one-shot invocations use
	// their persisted deadline; this test does not shorten the queue backoff.
	deadline := time.Now().Add(45 * time.Second)
	for {
		run(newPath, 0, "run", "--once", "--config", configPath)
		current := status(newPath)
		if current.Pending == 0 {
			assertStopped(current, 0)
			break
		}
		assertStopped(current, 1)
		if time.Now().After(deadline) {
			t.Fatal("candidate did not resume the old pending delivery within the acceptance deadline")
		}
		select {
		case <-t.Context().Done():
			t.Fatal("acceptance interrupted")
		case <-time.After(time.Second):
		}
	}
	mu.Lock()
	if len(notifications) != 2 || idempotencyKeys[1] != firstID || notifications[1].DetectedAt != notifications[0].DetectedAt {
		mu.Unlock()
		t.Fatal("upgrade lost, duplicated, or changed the pending notification identity")
	}
	mu.Unlock()
	if data, err := os.ReadFile(configPath); err != nil || !bytes.Equal(data, backup[configPath]) {
		t.Fatal("upgrade unexpectedly changed the compatible configuration")
	}
	// A rollback restores the database matching the old binary. The restored
	// pending message is intentionally resent, proving why receivers must dedupe
	// Idempotency-Key even after an operator restores a backup.
	for path, data := range backup {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	assertStopped(status(oldPath), 1)
	run(oldPath, 0, "run", "--once", "--config", configPath)
	assertStopped(status(oldPath), 0)
	run(oldPath, 0, "run", "--once", "--config", configPath)
	mu.Lock()
	defer mu.Unlock()
	if len(notifications) != 3 || idempotencyKeys[2] != firstID || notifications[2].DetectedAt != notifications[0].DetectedAt {
		t.Fatalf("rollback did not preserve deduplication identity: requests=%d", len(notifications))
	}
	t.Log(fmt.Sprintf("ACCEPTED upgrade %s -> %s and restored-backup rollback: pending=1 -> 0 -> restored 1 -> 0; requests=3; stable Idempotency-Key=%s; baseline was never replayed", oldVersion["version"], newVersion["version"], firstID))
}
