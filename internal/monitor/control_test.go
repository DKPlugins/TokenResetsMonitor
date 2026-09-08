package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/control"
	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
	"github.com/DKPlugins/TokenResetsMonitor/internal/state"
)

func TestLiveHistoryPreviewAndTargetedRetry(t *testing.T) {
	event := testEvent("live-failed")
	var sourceCalls, sendCalls atomic.Int32
	ids := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/providers/openai-codex/events":
			sourceCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []model.Event{event}, "pagination": map[string]bool{"has_more": false}, "meta": map[string]string{"schema_version": "1.0"}})
		case "/hook":
			ids <- r.Header.Get("Idempotency-Key")
			if sendCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusBadRequest)
			} else {
				w.WriteHeader(http.StatusNoContent)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	cfg := testConfig(t)
	cfg.APIBaseURL = server.URL
	cfg.Webhook.Enabled, cfg.Webhook.URL = true, server.URL+"/hook"
	path := filepath.Join(filepath.Dir(cfg.StatePath), "config.yaml")
	writeRuntimeConfig(t, path, cfg)
	// Production starts from a decoded snapshot, including normalized empty
	// collections. Use the same snapshot for the watcher comparison.
	var loadErr error
	cfg, loadErr = config.Load(path, nil)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	db, err := state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	p := makePolicy(cfg)
	if err := db.Reconcile(p); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitScan("openai-codex", nil, p, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunWithOptions(ctx, cfg, quiet(), false, RunOptions{ConfigPath: path, WatchInterval: 20 * time.Millisecond})
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(6 * time.Second):
			t.Error("daemon did not stop")
		}
	}()
	waitRuntimeStatus(t, cfg.StatePath, func(s model.Status) bool { return s.Failed == 1 })
	var result control.Result
	if err := control.Call(context.Background(), cfg.StatePath, control.Request{Operation: "deliveries.list", Query: state.Query{Status: "failed"}}, &result); err != nil {
		t.Fatal(err)
	}
	var page state.DeliveryPage
	if err := json.Unmarshal(result.Data, &page); err != nil {
		t.Fatal(err)
	}
	if result.ConfigurationSource != "running" || result.ConfigGeneration != 1 || len(page.Items) != 1 {
		t.Fatalf("bad live view: %+v %+v", result, page)
	}
	id := page.Items[0].Notification.ID
	beforeSource, beforeSend := sourceCalls.Load(), sendCalls.Load()
	candidate := cfg.FilterSpec()
	candidate.MinimumConfidence = "official"
	if err := control.Call(context.Background(), cfg.StatePath, control.Request{Operation: "filters.preview", Candidate: &candidate}, &result); err != nil {
		t.Fatal(err)
	}
	var preview control.PreviewPage
	if err := json.Unmarshal(result.Data, &preview); err != nil {
		t.Fatal(err)
	}
	if len(preview.Items) != 1 || !preview.Items[0].ActiveMatched || preview.Items[0].CandidateMatched {
		t.Fatalf("wrong preview: %+v", preview)
	}
	if sourceCalls.Load() != beforeSource || sendCalls.Load() != beforeSend {
		t.Fatal("preview performed external I/O")
	}
	if err := control.Call(context.Background(), cfg.StatePath, control.Request{Operation: "deliveries.retry", ID: id}, &result); err != nil {
		t.Fatal(err)
	}
	waitRuntimeStatus(t, cfg.StatePath, func(s model.Status) bool { return s.Channels["webhook"].Delivered == 1 && s.Failed == 0 })
	if <-ids != id || <-ids != id || sendCalls.Load() != 2 {
		t.Fatal("targeted retry changed identity or sent extra requests")
	}
	if err := control.Call(context.Background(), cfg.StatePath, control.Request{Operation: "deliveries.show", ID: id}, &result); err != nil {
		t.Fatal(err)
	}
	var detail state.DeliveryView
	if err := json.Unmarshal(result.Data, &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.AttemptHistory) != 2 || detail.Status != "delivered" {
		t.Fatalf("attempt history missing: %+v", detail)
	}
	// Rejected saved configuration must not replace the filters exposed by the
	// running generation or prevent authenticated direct inspection.
	if err := os.WriteFile(path, []byte("config_version: 3\nproviders: ["), 0600); err != nil {
		t.Fatal(err)
	}
	waitRuntimeStatus(t, cfg.StatePath, func(s model.Status) bool { return s.Runtime != nil && !s.Runtime.LastReloadSuccessful })
	if err := control.Call(context.Background(), cfg.StatePath, control.Request{Operation: "filters.preview", Candidate: &candidate}, &result); err != nil {
		t.Fatal(err)
	}
	if result.ConfigGeneration != 1 {
		t.Fatal("rejected config became active")
	}
}

func TestLiveRetryIsRefusedWhileReloadDrains(t *testing.T) {
	f := newReloadFixture(t)
	_ = nextRuntimeDelivery(t, f.deliveries)
	next := f.cfg
	next.Webhook.Headers = map[string]string{"Authorization": "Bearer replaced"}
	writeRuntimeConfig(t, f.path, next)
	waitRuntimeStatus(t, f.cfg.StatePath, func(s model.Status) bool { return s.Runtime != nil && s.Runtime.ReloadPending })
	var result control.Result
	err := control.Call(context.Background(), f.cfg.StatePath, control.Request{Operation: "deliveries.retry", ID: "does-not-matter-while-reloading"}, &result)
	if !errors.Is(err, control.ErrBusy) {
		t.Fatalf("retry should report draining configuration: %v", err)
	}
	f.release()
}
