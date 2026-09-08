package monitor

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/api"
	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
	"github.com/DKPlugins/TokenResetsMonitor/internal/state"
)

type callbackSender struct {
	send func(model.Notification) model.DeliveryResult
}

func (s callbackSender) Send(_ context.Context, _ string, n model.Notification) model.DeliveryResult {
	return s.send(n)
}

func TestCorrectionUsesActualInFlightSnapshotAndTracksRemovedProvider(t *testing.T) {
	cfg := testConfig(t)
	cfg.Webhook.Enabled, cfg.Webhook.NotifyChanges, cfg.Webhook.URL = true, true, "https://example.invalid/hook"
	policy := makePolicy(cfg)
	db, err := state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Reconcile(policy); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitScan("openai-codex", nil, policy, time.Now()); err != nil {
		t.Fatal(err)
	}
	first := testEvent("changing-event")
	first.Scope.Plans = []string{"pro"}
	if _, err := db.CommitScan("openai-codex", []model.Event{first}, policy, time.Now()); err != nil {
		t.Fatal(err)
	}
	revised := first
	revised.Revision = 2
	revised.Scope.Plans = []string{"plus", "pro"}
	var sent []model.Notification
	sender := callbackSender{send: func(n model.Notification) model.DeliveryResult {
		sent = append(sent, n)
		if len(sent) == 1 {
			if _, err := db.CommitScan("openai-codex", []model.Event{revised}, policy, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		return model.DeliveryResult{Success: true, StatusCode: 204}
	}}
	m := &monitor{store: db, policy: policy, sender: sender, logger: quiet()}
	if err := m.deliver(context.Background(), "webhook", true); err != nil {
		t.Fatal(err)
	}
	if err := m.deliver(context.Background(), "webhook", true); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[0].Event.Revision != 1 || sent[1].Kind != "correction" || sent[1].PreviousEvent == nil || sent[1].PreviousEvent.Revision != 1 || sent[1].Event.Revision != 2 {
		t.Fatalf("wrong actual delivery snapshots: %+v", sent)
	}
	if sent[0].ID == sent[1].ID || sent[1].SchemaVersion != 1 {
		t.Fatal("amendment identity or schema changed")
	}

	// Provider and confidence filters no longer select this event. Its original
	// recipient still needs the subsequent retraction.
	cfg.Providers = []config.ProviderFilter{{Slug: "anthropic-claude"}}
	cfg.MinimumConfidence = "official"
	m.policy = makePolicy(cfg)
	if err := db.Reconcile(m.policy); err != nil {
		t.Fatal(err)
	}
	retracted := revised
	retracted.Revision, retracted.Status = 3, "retracted"
	source := &stubSource{detail: retracted}
	m.source = source
	if err := m.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.deliver(context.Background(), "webhook", true); err != nil {
		t.Fatal(err)
	}
	if source.detailCalls != 1 || len(sent) != 3 || sent[2].Kind != "retraction" || sent[2].PreviousEvent.Revision != 2 {
		t.Fatalf("removed provider withdrawal lost: calls=%d sent=%+v", source.detailCalls, sent)
	}
	if err := m.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.deliver(context.Background(), "webhook", true); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 3 {
		t.Fatal("unchanged withdrawal resent")
	}
}

func TestSuccessfulSendRacingWithRetractionQueuesWithdrawal(t *testing.T) {
	cfg := testConfig(t)
	cfg.Webhook.Enabled, cfg.Webhook.NotifyChanges, cfg.Webhook.URL = true, true, "https://example.invalid/hook"
	p := makePolicy(cfg)
	db, err := state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Reconcile(p); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitScan("openai-codex", nil, p, time.Now()); err != nil {
		t.Fatal(err)
	}
	e := testEvent("withdraw-during-send")
	if _, err := db.CommitScan("openai-codex", []model.Event{e}, p, time.Now()); err != nil {
		t.Fatal(err)
	}
	var sent []model.Notification
	m := &monitor{store: db, policy: p, logger: quiet()}
	m.sender = callbackSender{send: func(n model.Notification) model.DeliveryResult {
		sent = append(sent, n)
		if len(sent) == 1 {
			e.Revision, e.Status = 2, "retracted"
			if _, err := db.CommitScan("openai-codex", []model.Event{e}, p, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		return model.DeliveryResult{Success: true}
	}}
	for i := 0; i < 2; i++ {
		if err := m.deliver(context.Background(), "webhook", true); err != nil {
			t.Fatal(err)
		}
	}
	if len(sent) != 2 || sent[1].Kind != "retraction" || sent[1].PreviousEvent.Status != "published" {
		t.Fatalf("acknowledgement lost during cancellation: %+v", sent)
	}
}

type missingHistorySource struct{ calls []string }

func (s *missingHistorySource) Events(context.Context, string) ([]model.Event, error) {
	return nil, nil
}
func (s *missingHistorySource) Event(_ context.Context, id string) (model.Event, error) {
	s.calls = append(s.calls, id)
	return model.Event{}, &api.Error{StatusCode: 404, Message: "not found"}
}

func TestMissingHistoryVerificationIsBoundedAndRotatesAcrossRestart(t *testing.T) {
	cfg := testConfig(t)
	cfg.Webhook.Enabled, cfg.Webhook.URL = true, "https://example.invalid/hook"
	p := makePolicy(cfg)
	db, err := state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Reconcile(p); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitScan("openai-codex", nil, p, time.Now()); err != nil {
		t.Fatal(err)
	}
	var events []model.Event
	for i := 0; i < 40; i++ {
		events = append(events, testEvent(fmt.Sprintf("missing-%02d", i)))
	}
	if _, err := db.CommitScan("openai-codex", events, p, time.Now()); err != nil {
		t.Fatal(err)
	}
	source := &missingHistorySource{}
	m := &monitor{store: db, policy: p, source: source, logger: quiet()}
	if err := m.poll(context.Background()); err != nil && !recoverablePoll(err) {
		t.Fatal(err)
	}
	if len(source.calls) != 32 {
		t.Fatalf("wrong detail budget: %d", len(source.calls))
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m.store = db
	if err := db.Reconcile(p); err != nil {
		t.Fatal(err)
	}
	if err := m.poll(context.Background()); err != nil && !recoverablePoll(err) {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, id := range source.calls {
		seen[id] = true
	}
	if len(seen) != 40 {
		t.Fatalf("missing events starved after restart: %d", len(seen))
	}
	status, err := db.Status(false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if status.Pending != 40 {
		t.Fatal("404 canceled pending events")
	}
	if len(source.calls) != 64 {
		t.Fatal(errors.New("detail budget changed after restart"))
	}
}
