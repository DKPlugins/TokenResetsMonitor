package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/api"
	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
	"github.com/DKPlugins/TokenResetsMonitor/internal/state"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Defaults()
	cfg.Updates.Enabled = false
	cfg.StatePath = filepath.Join(t.TempDir(), "state.db")
	cfg.Providers = []config.ProviderFilter{{Slug: "openai-codex"}}
	return cfg
}

func testEvent(id string) model.Event {
	return model.Event{ID: id, Slug: id, Provider: model.Provider{Slug: "openai-codex", Name: "Codex"}, Status: "published", EventType: "hard_reset", Revision: 1, AnnouncedAt: time.Now().UTC().Add(-24 * time.Hour), Confidence: model.Confidence{Label: "reported"}}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRunOnceBaselineAndIndependentChannels(t *testing.T) {
	var mu sync.Mutex
	var events []model.Event
	webhookStarted := make(chan struct{}, 1)
	telegramSent := make(chan struct{}, 1)
	releaseWebhook := make(chan struct{})
	var closeRelease sync.Once
	release := func() { closeRelease.Do(func() { close(releaseWebhook) }) }
	defer release()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/providers/openai-codex/events":
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": events, "pagination": map[string]any{"has_more": false}, "meta": map[string]string{"schema_version": "1.2"}})
		case "/hook":
			var n model.Notification
			if err := json.NewDecoder(r.Body).Decode(&n); err != nil || n.Test || n.Event.ID != "late" || r.Header.Get("Idempotency-Key") != n.ID {
				t.Error("invalid production notification", err)
			}
			webhookStarted <- struct{}{}
			select {
			case <-releaseWebhook:
			case <-r.Context().Done():
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/bot123:secret/sendMessage":
			telegramSent <- struct{}{}
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
		default:
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer func() { release(); server.Close() }()
	cfg := testConfig(t)
	cfg.APIBaseURL = server.URL
	cfg.Webhook.Enabled = true
	cfg.Webhook.URL = server.URL + "/hook"
	cfg.Webhook.Timeout = "5s"
	cfg.Telegram.Enabled = true
	cfg.Telegram.APIBaseURL = server.URL
	cfg.Telegram.BotToken = "123:secret"
	cfg.Telegram.ChatID = "42"
	// An empty source is a valid baseline; neither delivery channel is contacted.
	mu.Lock()
	events = []model.Event{}
	mu.Unlock()
	if err := Run(context.Background(), cfg, quiet(), true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-webhookStarted:
		t.Fatal("baseline sent a notification")
	default:
	}
	mu.Lock()
	events = []model.Event{testEvent("late")}
	mu.Unlock()
	finished := make(chan error, 1)
	go func() { finished <- Run(context.Background(), cfg, quiet(), true) }()
	select {
	case <-webhookStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("webhook did not start")
	}
	select {
	case <-telegramSent:
	case <-time.After(3 * time.Second):
		t.Fatal("Telegram was blocked by webhook")
	}
	release()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("run once must report failed delivery")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run once did not stop")
	}
	status, err := state.ReadStatus(cfg.StatePath)
	if err != nil || status.Running || status.Pending != 1 || status.Failed != 0 || !status.Providers["openai-codex"].Ready {
		t.Fatal("durable result status incorrect", err, status)
	}
	// Restart before backoff expiry must not replay either the source event or
	// the already delivered Telegram notification.
	if err := Run(context.Background(), cfg, quiet(), true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-telegramSent:
		t.Fatal("restart duplicated Telegram")
	default:
	}
	select {
	case <-webhookStarted:
		t.Fatal("retry ignored backoff")
	default:
	}
}

type stubSource struct {
	events      []model.Event
	err         error
	detail      model.Event
	detailErr   error
	detailCalls int
}

func (s *stubSource) Events(context.Context, string) ([]model.Event, error) { return s.events, s.err }
func (s *stubSource) Event(context.Context, string) (model.Event, error) {
	s.detailCalls++
	return s.detail, s.detailErr
}

type rateLimitedSource struct {
	calls   map[string]int
	limited bool
}

func (s *rateLimitedSource) Events(_ context.Context, slug string) ([]model.Event, error) {
	s.calls[slug]++
	if slug == "openai-codex" && s.limited {
		return nil, &api.Error{StatusCode: 429, Retryable: true, RetryAfter: time.Hour, Message: "API returned HTTP 429"}
	}
	return []model.Event{}, nil
}
func (s *rateLimitedSource) Event(context.Context, string) (model.Event, error) {
	return model.Event{}, errors.New("unexpected detail request")
}

func TestProviderRetryAfterSurvivesRestartAndDoesNotDelayOtherProviders(t *testing.T) {
	cfg := testConfig(t)
	cfg.Providers = append(cfg.Providers, config.ProviderFilter{Slug: "anthropic-claude"})
	p := makePolicy(cfg)
	s, err := state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Reconcile(p); err != nil {
		t.Fatal(err)
	}
	source := &rateLimitedSource{calls: map[string]int{}, limited: true}
	var logs bytes.Buffer
	current := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	m := &monitor{store: s, source: source, policy: p, logger: slog.New(slog.NewJSONHandler(&logs, nil)), clock: func() time.Time { return current }}
	if err := m.poll(context.Background()); err == nil {
		t.Fatal("rate limit was not reported")
	}
	if source.calls["openai-codex"] != 1 || source.calls["anthropic-claude"] != 1 {
		t.Fatal("other provider was blocked by rate limit", source.calls)
	}
	// Reopening the database models a daemon restart; it must not discard the
	// upstream deadline even though the configured poll interval is shorter.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	m.store = s
	if err := s.Reconcile(p); err != nil {
		t.Fatal(err)
	}
	current = current.Add(time.Minute)
	if err := m.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if source.calls["openai-codex"] != 1 || source.calls["anthropic-claude"] != 2 {
		t.Fatal("provider cooldown did not isolate upstream calls", source.calls)
	}
	current = current.Add(59 * time.Minute)
	source.limited = false
	if err := m.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if source.calls["openai-codex"] != 2 || source.calls["anthropic-claude"] != 3 {
		t.Fatal("polling did not resume at the deadline", source.calls)
	}
	notBefore, recovering, err := s.ProviderPollState("openai-codex")
	if err != nil || !notBefore.IsZero() || recovering {
		t.Fatal("successful recovery retained cooldown or error", err)
	}
	if !strings.Contains(logs.String(), `"recovered":true`) || !strings.Contains(logs.String(), `"retry_at"`) {
		t.Fatal("cooldown and recovery were not logged", logs.String())
	}
}

func TestDetailRetryAfterStopsFurtherVerificationAndDefersProvider(t *testing.T) {
	cfg := testConfig(t)
	cfg.Webhook.Enabled = true
	cfg.Webhook.URL = "https://example.test/hook"
	p := makePolicy(cfg)
	s, err := state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Reconcile(p); err != nil {
		t.Fatal(err)
	}
	current := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	if _, err := s.CommitScan("openai-codex", nil, p, current); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitScan("openai-codex", []model.Event{testEvent("first"), testEvent("second")}, p, current); err != nil {
		t.Fatal(err)
	}
	source := &stubSource{detailErr: &api.Error{StatusCode: 429, RetryAfter: time.Hour, Message: "API returned HTTP 429"}}
	m := &monitor{store: s, source: source, policy: p, logger: quiet(), clock: func() time.Time { return current }}
	if err := m.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if source.detailCalls != 1 {
		t.Fatal("more detail requests were sent after Retry-After", source.detailCalls)
	}
	current = current.Add(time.Minute)
	if err := m.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if source.detailCalls != 1 {
		t.Fatal("cooldown did not defer the next provider scan")
	}
	due, err := s.Due("webhook", current, 0)
	if err != nil || len(due) != 2 {
		t.Fatal("unverified disappearance canceled deliveries", err)
	}
}

func TestFailedScanDoesNotInitializeAndOnlyExplicitRetractionCancels(t *testing.T) {
	cfg := testConfig(t)
	cfg.Webhook.Enabled = true
	cfg.Webhook.URL = "https://example.test/hook"
	s, err := state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := makePolicy(cfg)
	if err := s.Reconcile(p); err != nil {
		t.Fatal(err)
	}
	source := &stubSource{events: []model.Event{testEvent("partial")}, err: errors.New("later page failed")}
	m := &monitor{store: s, source: source, policy: p, logger: quiet()}
	if err := m.poll(context.Background()); err == nil {
		t.Fatal("failed scan did not report failure")
	}
	status, err := s.Status(true, time.Now())
	if err != nil || status.Providers["openai-codex"].Ready {
		t.Fatal("partial response initialized provider")
	}
	source.events = []model.Event{testEvent("baseline")}
	source.err = nil
	if err := m.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	newEvent := testEvent("new")
	source.events = append(source.events, newEvent)
	if err := m.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	source.events = source.events[:1]
	source.detailErr = &api.Error{StatusCode: 404, Message: "API returned HTTP 404"}
	if err := m.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	due, err := s.Due("webhook", time.Now(), 0)
	if err != nil || len(due) != 1 {
		t.Fatal("404 incorrectly canceled missing event", err)
	}
	source.detailErr = nil
	source.detail = newEvent
	source.detail.Status = "retracted"
	source.detail.Revision++
	if err := m.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	due, err = s.Due("webhook", time.Now(), 0)
	if err != nil || len(due) != 0 {
		t.Fatal("explicit retraction did not cancel", err)
	}
}

func TestCredentialRotationPreservesRecipientIdentity(t *testing.T) {
	cfg := testConfig(t)
	cfg.Webhook.Enabled = true
	cfg.Webhook.URL = "https://example.test/hook?token=old&workspace=first"
	cfg.Webhook.Headers = map[string]string{"Authorization": "Bearer old"}
	cfg.Telegram.Enabled = true
	cfg.Telegram.BotToken = "old"
	cfg.Telegram.ChatID = "123"
	before := makePolicy(cfg)
	cfg.Webhook.URL = "https://example.test/hook?token=new&workspace=first"
	cfg.Webhook.Headers["Authorization"] = "Bearer new"
	cfg.Telegram.BotToken = "new"
	after := makePolicy(cfg)
	for _, channel := range []string{"webhook", "telegram"} {
		if before.Channels[channel] != after.Channels[channel] {
			t.Fatalf("%s credential rotation changed recipient", channel)
		}
	}
	cfg.Webhook.URL = "https://example.test/hook?token=new&workspace=second"
	cfg.Telegram.ChatID = "456"
	changed := makePolicy(cfg)
	for _, channel := range []string{"webhook", "telegram"} {
		if before.Channels[channel] == changed.Channels[channel] {
			t.Fatalf("%s destination change was ignored", channel)
		}
	}
}

func TestContinuousRunCancelsInFlightSendAndKeepsPending(t *testing.T) {
	var mu sync.Mutex
	events := []model.Event{}
	started := make(chan struct{}, 1)
	unblock := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hook" {
			_, _ = io.Copy(io.Discard, r.Body)
			started <- struct{}{}
			select {
			case <-r.Context().Done():
			case <-unblock:
			}
			return
		}
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": events, "pagination": map[string]any{"has_more": false}, "meta": map[string]string{"schema_version": "1.2"}})
	}))
	defer func() { close(unblock); server.Close() }()
	cfg := testConfig(t)
	cfg.APIBaseURL = server.URL
	cfg.Webhook.Enabled = true
	cfg.Webhook.URL = server.URL + "/hook"
	if err := Run(context.Background(), cfg, quiet(), true); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	events = []model.Event{testEvent("new")}
	mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- Run(ctx, cfg, quiet(), false) }()
	select {
	case <-started:
	case <-time.After(4 * time.Second):
		t.Fatal("delivery did not start")
	}
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("monitor did not shut down promptly")
	}
	status, err := state.ReadStatus(cfg.StatePath)
	if err != nil || status.Running || status.Pending != 1 || status.Failed != 0 {
		t.Fatal("shutdown did not preserve pending work", err, status)
	}
}

func TestDetailBudgetFailureDoesNotCommitPartialScan(t *testing.T) {
	cfg := testConfig(t)
	cfg.Webhook.Enabled = true
	cfg.Webhook.URL = "https://example.test/hook"
	db, err := state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	policy := makePolicy(cfg)
	if err := db.Reconcile(policy); err != nil {
		t.Fatal(err)
	}
	for _, events := range [][]model.Event{nil, {testEvent("pending")}} {
		if _, err := db.CommitScan("openai-codex", events, policy, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	source := &stubSource{events: []model.Event{testEvent("must-not-commit")}, detailErr: api.ErrScanBudget}
	monitor := &monitor{store: db, source: source, policy: policy, logger: quiet()}
	if err := monitor.poll(context.Background()); err == nil {
		t.Fatal("oversized scan reported success")
	}
	due, err := db.Due("webhook", time.Now(), 0)
	if err != nil || len(due) != 1 || due[0].Notification.Event.ID != "pending" {
		t.Fatalf("oversized detail scan changed the durable queue: %+v %v", due, err)
	}
}
