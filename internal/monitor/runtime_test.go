package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
	"github.com/DKPlugins/TokenResetsMonitor/internal/observability"
	"github.com/DKPlugins/TokenResetsMonitor/internal/state"
	"gopkg.in/yaml.v3"
)

type runtimeDelivery struct{ path, authorization, id, event string }
type reloadFixture struct {
	cfg        config.Config
	path       string
	deliveries chan runtimeDelivery
	metrics    *observability.Metrics
	mu         sync.Mutex
	events     []model.Event
	release    func()
	stop       func()
}

func writeRuntimeConfig(t *testing.T, path string, cfg config.Config) {
	t.Helper()
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func waitRuntimeStatus(t *testing.T, path string, match func(model.Status) bool) model.Status {
	t.Helper()
	timer := time.NewTimer(6 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var last model.Status
	for {
		snapshot, err := state.ReadStatus(path)
		if err == nil {
			last = snapshot
			if match(snapshot) {
				return snapshot
			}
		}
		select {
		case <-timer.C:
			t.Fatalf("runtime did not reach the expected state: %+v", last)
		case <-ticker.C:
		}
	}
}

func nextRuntimeDelivery(t *testing.T, deliveries <-chan runtimeDelivery) runtimeDelivery {
	t.Helper()
	select {
	case d := <-deliveries:
		return d
	case <-time.After(6 * time.Second):
		t.Fatal("notification did not arrive")
		return runtimeDelivery{}
	}
}

func newReloadFixture(t *testing.T) *reloadFixture {
	t.Helper()
	f := &reloadFixture{deliveries: make(chan runtimeDelivery, 10), metrics: observability.New(), events: []model.Event{testEvent("first"), testEvent("second")}}
	release := make(chan struct{})
	var releaseOnce sync.Once
	f.release = func() { releaseOnce.Do(func() { close(release) }) }
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/providers/openai-codex/events":
			f.mu.Lock()
			defer f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": f.events, "pagination": map[string]any{"has_more": false}, "meta": map[string]any{"schema_version": "1.0"}})
		case "/old", "/new":
			var notification model.Notification
			if err := json.NewDecoder(r.Body).Decode(&notification); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			f.deliveries <- runtimeDelivery{r.URL.Path, r.Header.Get("Authorization"), notification.ID, notification.Event.ID}
			if requests.Add(1) == 1 {
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	cfg := testConfig(t)
	cfg.APIBaseURL = server.URL
	cfg.Updates.Enabled = false
	cfg.Reload.Enabled = true
	cfg.Logging.FileEnabled = false
	cfg.Webhook.Enabled = true
	cfg.Webhook.URL = server.URL + "/old"
	cfg.Webhook.Headers = map[string]string{"Authorization": "Bearer old-secret"}
	cfg.Webhook.Timeout = "10s"
	f.path = filepath.Join(filepath.Dir(cfg.StatePath), "config.yaml")
	writeRuntimeConfig(t, f.path, cfg)
	var err error
	f.cfg, err = config.Load(f.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(f.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	p := makePolicy(f.cfg)
	if err := db.Reconcile(p); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitScan("openai-codex", nil, p, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitScan("openai-codex", f.events, p, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunWithOptions(ctx, f.cfg, quiet(), false, RunOptions{ConfigPath: f.path, WatchInterval: 20 * time.Millisecond, Metrics: f.metrics})
	}()
	var stopOnce sync.Once
	f.stop = func() {
		stopOnce.Do(func() {
			cancel()
			f.release()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(6 * time.Second):
				t.Error("runtime did not stop")
			}
		})
	}
	t.Cleanup(f.stop)
	return f
}

func TestReloadDrainsAcknowledgmentBeforeSwitchingSender(t *testing.T) {
	for _, changedRecipient := range []bool{false, true} {
		name := "credential-rotation"
		if changedRecipient {
			name = "recipient-change"
		}
		t.Run(name, func(t *testing.T) {
			f := newReloadFixture(t)
			first := nextRuntimeDelivery(t, f.deliveries)
			next := f.cfg
			next.Webhook.Headers = map[string]string{"Authorization": "Bearer new-secret"}
			if changedRecipient {
				next.Webhook.URL = f.cfg.APIBaseURL + "/new"
			}
			writeRuntimeConfig(t, f.path, next)
			waitRuntimeStatus(t, f.cfg.StatePath, func(s model.Status) bool {
				return s.Runtime != nil && s.Runtime.ReloadPending && s.Runtime.ConfigGeneration == 1
			})
			select {
			case unexpected := <-f.deliveries:
				t.Fatalf("another send started while draining: %+v", unexpected)
			default:
			}
			f.release()
			snapshot := waitRuntimeStatus(t, f.cfg.StatePath, func(s model.Status) bool {
				return s.Runtime != nil && !s.Runtime.ReloadPending && s.Runtime.ConfigGeneration == 2
			})
			if snapshot.Channels["webhook"].Delivered < 1 {
				t.Fatal("reload did not preserve the in-flight acknowledgment", snapshot)
			}
			if changedRecipient {
				if snapshot.Channels["webhook"].Canceled != 1 || snapshot.Pending != 0 {
					t.Fatal("old recipient queue was not canceled", snapshot)
				}
				f.mu.Lock()
				f.events = append(f.events, testEvent("after-reload"))
				f.mu.Unlock()
				next.PollInterval = "2m"
				writeRuntimeConfig(t, f.path, next)
				waitRuntimeStatus(t, f.cfg.StatePath, func(s model.Status) bool { return s.Runtime != nil && s.Runtime.ConfigGeneration == 3 })
				delivered := nextRuntimeDelivery(t, f.deliveries)
				if delivered.path != "/new" || delivered.authorization != "Bearer new-secret" || delivered.event != "after-reload" {
					t.Fatalf("new recipient received historical or incorrectly authenticated work: %+v", delivered)
				}
			} else {
				delivered := nextRuntimeDelivery(t, f.deliveries)
				if delivered.path != "/old" || delivered.authorization != "Bearer new-secret" || delivered.id == first.id {
					t.Fatalf("credential rotation lost the queue or repeated its acknowledgment: %+v", delivered)
				}
			}
			waitRuntimeStatus(t, f.cfg.StatePath, func(s model.Status) bool { return s.Pending == 0 && s.Channels["webhook"].Delivered == 2 })
			f.stop()
			final, err := state.ReadStatus(f.cfg.StatePath)
			if err != nil || final.Pending != 0 || final.Channels["webhook"].Delivered != 2 {
				t.Fatal("reload lost notification results", final, err)
			}
			families, err := f.metrics.Registry.Gather()
			if err != nil {
				t.Fatal(err)
			}
			attempts := 0.
			for _, family := range families {
				if family.GetName() == "tokenresetsmonitor_delivery_attempts_total" {
					for _, metric := range family.Metric {
						attempts += metric.GetCounter().GetValue()
					}
				}
			}
			if attempts != 2 {
				t.Fatal("reload reset process delivery counters", attempts)
			}
		})
	}
}

func TestLatestInvalidOrRestartCandidateKeepsOldGeneration(t *testing.T) {
	for _, reason := range []string{"invalid_config", "restart_required"} {
		t.Run(reason, func(t *testing.T) {
			f := newReloadFixture(t)
			_ = nextRuntimeDelivery(t, f.deliveries)
			next := f.cfg
			next.Webhook.Headers = map[string]string{"Authorization": "Bearer must-not-apply"}
			writeRuntimeConfig(t, f.path, next)
			waitRuntimeStatus(t, f.cfg.StatePath, func(s model.Status) bool { return s.Runtime != nil && s.Runtime.ReloadPending })
			if reason == "invalid_config" {
				if err := os.WriteFile(f.path, []byte("config_version: [invalid-secret"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				next.StatePath = filepath.Join(filepath.Dir(f.cfg.StatePath), "must-not-open.db")
				writeRuntimeConfig(t, f.path, next)
			}
			waitRuntimeStatus(t, f.cfg.StatePath, func(s model.Status) bool { return s.Runtime != nil && s.Runtime.ReloadError == reason })
			f.release()
			waitRuntimeStatus(t, f.cfg.StatePath, func(s model.Status) bool {
				return s.Runtime != nil && !s.Runtime.ReloadPending && s.Runtime.ConfigGeneration == 1 && s.Runtime.ReloadError == reason
			})
			second := nextRuntimeDelivery(t, f.deliveries)
			if second.authorization != "Bearer old-secret" || second.path != "/old" {
				t.Fatalf("a rejected candidate changed the active sender: %+v", second)
			}
			f.stop()
			if reason == "restart_required" {
				if _, err := os.Stat(next.StatePath); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("rejected immutable config opened a new database", err)
				}
			}
		})
	}
}

func TestReloadCandidateRejectsTemplateExecutionFailure(t *testing.T) {
	cfg := testConfig(t)
	cfg.Updates.Enabled = false
	cfg.Webhook.Enabled = true
	cfg.Webhook.URL = "http://127.0.0.1:1/never-contact"
	cfg.Webhook.BodyTemplate = "{{.MissingField}}"
	path := filepath.Join(filepath.Dir(cfg.StatePath), "config.yaml")
	writeRuntimeConfig(t, path, cfg)
	candidate, _ := readCandidate(path, nil)
	if candidate.err == nil {
		t.Fatal("reload accepted a template that cannot render a notification")
	}
}

type canceledScanSource struct{ started chan struct{} }

func (s canceledScanSource) Events(ctx context.Context, _ string) ([]model.Event, error) {
	close(s.started)
	<-ctx.Done()
	return []model.Event{testEvent("partial")}, nil
}
func (canceledScanSource) Event(context.Context, string) (model.Event, error) {
	return model.Event{}, errors.New("unexpected detail")
}

func TestDrainingGenerationDiscardsInterruptedScan(t *testing.T) {
	cfg := testConfig(t)
	cfg.Updates.Enabled = false
	db, err := state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := makePolicy(cfg)
	if err := db.Reconcile(p); err != nil {
		t.Fatal(err)
	}
	source := canceledScanSource{started: make(chan struct{})}
	m := &monitor{store: db, source: source, policy: p, logger: quiet()}
	g := startGeneration(context.Background(), cfg, m, make(chan error, 1))
	defer g.drain()
	select {
	case <-source.started:
	case <-time.After(time.Second):
		t.Fatal("scan did not begin")
	}
	g.drain()
	select {
	case <-g.done:
	case <-time.After(time.Second):
		t.Fatal("draining did not cancel the scan")
	}
	snapshot, err := db.Status(true, time.Now())
	if err != nil || snapshot.Pending != 0 || snapshot.Providers["openai-codex"].Ready {
		t.Fatal("canceled scan committed a partial baseline", snapshot, err)
	}
}

type limitAfterOneSender struct{ calls int }

func (s *limitAfterOneSender) Send(context.Context, string, model.Notification) model.DeliveryResult {
	s.calls++
	return model.DeliveryResult{Retryable: true, StatusCode: 429, RetryAfter: time.Hour}
}
func TestDeliveryRechecksRecipientCooldownInsideDueBatch(t *testing.T) {
	cfg := testConfig(t)
	cfg.Updates.Enabled = false
	cfg.Webhook.Enabled = true
	cfg.Webhook.URL = "http://127.0.0.1:1/hook"
	db, err := state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := makePolicy(cfg)
	if err := db.Reconcile(p); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitScan("openai-codex", nil, p, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitScan("openai-codex", []model.Event{testEvent("first"), testEvent("second")}, p, time.Now()); err != nil {
		t.Fatal(err)
	}
	sender := &limitAfterOneSender{}
	m := &monitor{store: db, sender: sender, policy: p, logger: quiet()}
	if err := m.deliver(context.Background(), "webhook", true); err == nil {
		t.Fatal("one-shot mode did not report the failed attempt")
	}
	if sender.calls != 1 {
		t.Fatal("another queued notification bypassed recipient cooldown", sender.calls)
	}
}

func TestFinalCandidateWaitsForTwoMatchingValidatedReads(t *testing.T) {
	initial := testConfig(t)
	partial := initial
	partial.Logging.Level = "debug"
	complete := partial
	complete.MinimumConfidence = "official"
	calls := 0
	latest, ok := stableCandidate(context.Background(), time.Millisecond, func() (candidate, [32]byte) {
		calls++
		switch calls {
		case 1:
			return candidate{cfg: initial}, [32]byte{1}
		case 2:
			// A parseable partial document is still changing after the drain.
			return candidate{cfg: partial}, [32]byte{2}
		default:
			return candidate{cfg: complete}, [32]byte{3}
		}
	})
	if !ok || calls != 4 || latest.cfg.MinimumConfidence != "official" || latest.cfg.Logging.Level != "debug" {
		t.Fatalf("final check applied a transient document: calls=%d ok=%v cfg=%+v", calls, ok, latest.cfg)
	}
}

func TestFinalCandidateSettlingCanBeCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstRead := make(chan struct{})
	done := make(chan bool, 1)
	go func() {
		_, ok := stableCandidate(ctx, time.Hour, func() (candidate, [32]byte) {
			close(firstRead)
			return candidate{}, [32]byte{1}
		})
		done <- ok
	}()
	<-firstRead
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("canceled settling published a candidate")
		}
	case <-time.After(time.Second):
		t.Fatal("settling blocked generation cancellation")
	}
}
