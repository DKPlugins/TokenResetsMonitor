package logging

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
)

func TestSecretsNeverReachConsoleOrFile(t *testing.T) {
	cfg := config.Defaults()
	cfg.Logging.Directory = t.TempDir()
	cfg.Logging.FileEnabled = true
	cfg.Logging.Level = "debug"
	cfg.Logging.Format = "json"
	cfg.Webhook.URL = "https://example.test/private?key=SECRETQUERY"
	cfg.Webhook.Headers = map[string]string{"Authorization": "Bearer SUPERSECRET"}
	cfg.Telegram.BotToken = "123456:ANOTHERSUPERSECRETTOKEN"
	var output bytes.Buffer
	logger, closeFn, e := New(cfg, &output)
	if e != nil {
		t.Fatal(e)
	}
	logger.Log(context.Background(), slog.LevelDebug, "request failed https://example.test/PRIVATEPATH", "error", "Bearer SUPERSECRET", "body", "private body", "token", cfg.Telegram.BotToken)
	if e = closeFn(); e != nil {
		t.Fatal(e)
	}
	file, e := os.ReadFile(filepath.Join(cfg.Logging.Directory, "tokenresetsmonitor.jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	for _, text := range []string{output.String(), string(file)} {
		for _, secret := range []string{"PRIVATEPATH", "SUPERSECRET", "ANOTHERSUPERSECRETTOKEN", "private body"} {
			if strings.Contains(text, secret) {
				t.Fatalf("secret leaked: %s", secret)
			}
		}
	}
}

func TestLazyFileSinkDoesNotWriteBeforeStateLock(t *testing.T) {
	cfg := config.Defaults()
	cfg.Logging.Directory = filepath.Join(t.TempDir(), "logs")
	cfg.Logging.FileEnabled = true
	_, closeFn, e := New(cfg, io.Discard)
	if e != nil {
		t.Fatal(e)
	}
	_ = closeFn()
	if _, e = os.Stat(cfg.Logging.Directory); !os.IsNotExist(e) {
		t.Fatal("unused logger touched daemon files")
	}
}

func TestRotationBoundsAndExportDuringWrites(t *testing.T) {
	cfg := config.Defaults()
	cfg.Logging.Directory = t.TempDir()
	cfg.Logging.FileEnabled = true
	cfg.Logging.MaxSizeMB = 1
	cfg.Logging.MaxBackups = 2
	r, e := NewRotator(cfg.Logging)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	line := `{"time":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","level":"INFO","msg":"` + strings.Repeat("a", 600000) + `","body":"SECRET_BODY"}` + "\n"
	for i := 0; i < 6; i++ {
		if _, e = r.Write([]byte(line)); e != nil {
			t.Fatal(e)
		}
	}
	archives, _ := filepath.Glob(filepath.Join(cfg.Logging.Directory, "*.gz"))
	if len(archives) != 2 {
		t.Fatalf("rotation bound: %d", len(archives))
	}
	archive := filepath.Join(t.TempDir(), "diagnostics.zip")
	if e = Export(cfg, ExportOptions{Since: 24 * time.Hour, Output: archive}); e != nil {
		t.Fatal(e)
	}
	z, e := zip.OpenReader(archive)
	if e != nil {
		t.Fatal(e)
	}
	defer z.Close()
	found := false
	for _, f := range z.File {
		in, _ := f.Open()
		data, _ := io.ReadAll(in)
		in.Close()
		if f.Name == "logs.jsonl" {
			found = true
			if bytes.Contains(data, []byte("SECRET_BODY")) {
				t.Fatal("body leaked in export")
			}
		}
	}
	if !found {
		t.Fatal("logs missing")
	}
}

func TestExportAllowsOnlyStructuredRecordsAndNoOverwrite(t *testing.T) {
	cfg := config.Defaults()
	archive := filepath.Join(t.TempDir(), "diagnostics.zip")
	input := `{"time":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","level":"INFO","msg":"hello","authorization":"SECRET","unknown":"SECRET"}` + "\nplain text secret\n"
	e := Export(cfg, ExportOptions{Since: time.Hour, Output: archive, Input: "-", Stdin: strings.NewReader(input)})
	if e != nil {
		t.Fatal(e)
	}
	if e = Export(cfg, ExportOptions{Since: time.Hour, Output: archive}); e == nil {
		t.Fatal("existing export overwritten")
	}
	z, _ := zip.OpenReader(archive)
	defer z.Close()
	for _, f := range z.File {
		r, _ := f.Open()
		b, _ := io.ReadAll(r)
		r.Close()
		if bytes.Contains(b, []byte("SECRET")) || bytes.Contains(b, []byte("plain text secret")) {
			t.Fatal("unstructured secret exported")
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("disk unavailable /private/secret-path")
}

func TestLogWriteFailuresAreReportedAndReturnedAtShutdown(t *testing.T) {
	cfg := config.Defaults()
	cfg.Logging.FileEnabled = true
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.Logging.Directory = filepath.Join(blocker, "logs")
	var output, diagnostics bytes.Buffer
	logger, closeFn, err := New(cfg, &output, &diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	logger.With("provider", "openai-codex").Info("first record")
	logger.Info("second record")
	if err := closeFn(); err == nil {
		t.Fatal("file sink failure was silently lost")
	}
	if strings.Count(diagnostics.String(), "log output failed") != 1 || strings.Contains(diagnostics.String(), blocker) {
		t.Fatalf("unsafe or repeated diagnostics: %s", diagnostics.String())
	}
	if !strings.Contains(output.String(), "second record") {
		t.Fatal("file failure stopped console logging")
	}
}

func TestOversizedLogRecordCannotBreakRetentionBound(t *testing.T) {
	cfg := config.Defaults()
	cfg.Logging.Directory = t.TempDir()
	cfg.Logging.MaxSizeMB = 1
	r, err := NewRotator(cfg.Logging)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if n, err := r.Write(bytes.Repeat([]byte("x"), maxLogRecordBytes+1)); err == nil || n != 0 {
		t.Fatal("oversized record was written")
	}
	if info, err := os.Stat(r.path()); err != nil || info.Size() != 0 {
		t.Fatalf("oversized record changed file size: %v %v", info, err)
	}
}

func TestExportWriteFailureIsNotReportedAsPartialSuccess(t *testing.T) {
	now := time.Now().UTC()
	input := `{"time":"` + now.Format(time.RFC3339Nano) + `","msg":"record"}` + "\n"
	manifest := exportManifest{RequestedSince: now.Add(-time.Hour)}
	err := scanRecords(strings.NewReader(input), failingWriter{}, NewRedactor(config.Defaults()), &manifest)
	if err == nil || manifest.Records != 0 || strings.Contains(err.Error(), "secret-path") {
		t.Fatalf("write failure not propagated safely: %v %+v", err, manifest)
	}
}

func TestSensitiveGroupsAreRedactedAndTimestampsRemainValid(t *testing.T) {
	cfg := config.Defaults()
	cfg.Logging.Format = "json"
	cfg.Telegram.ChatID = "1"
	var output bytes.Buffer
	logger, closeFn, err := New(cfg, &output)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	logger.WithGroup("request").Info("record", "arbitrary_payload", "PRIVATE_BODY")
	if strings.Contains(output.String(), "PRIVATE_BODY") {
		t.Fatal("sensitive slog group leaked")
	}
	input := `{"time":"2026-09-08T01:00:00Z","msg":"hello"}` + "\n"
	manifest := exportManifest{RequestedSince: time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)}
	var collected bytes.Buffer
	if err := scanRecords(strings.NewReader(input), &collected, NewRedactor(cfg), &manifest); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(collected.String(), "2026-09-08T01:00:00Z") {
		t.Fatal("short chat ID corrupted structured timestamp")
	}
}

func TestExportSkipsOversizedLineAndKeepsFollowingRecords(t *testing.T) {
	now := time.Now().UTC()
	input := strings.Repeat("x", maxLogRecordBytes+100) + "\n" + `{"time":"` + now.Format(time.RFC3339Nano) + `","msg":"later record"}` + "\n"
	manifest := exportManifest{RequestedSince: now.Add(-time.Hour)}
	var collected bytes.Buffer
	if err := scanRecords(strings.NewReader(input), &collected, NewRedactor(config.Defaults()), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Skipped != 1 || manifest.Records != 1 || !strings.Contains(collected.String(), "later record") {
		t.Fatalf("oversized record lost later logs: %+v", manifest)
	}
}

func TestExportFileSnapshotDoesNotFollowNewAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "growing.jsonl")
	if err := os.WriteFile(path, []byte("existing\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	snapshot := fileSnapshot(f)
	appender, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	appender.WriteString("later\n")
	appender.Close()
	data, err := io.ReadAll(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "existing\n" {
		t.Fatalf("export followed a live file: %q", data)
	}
}
