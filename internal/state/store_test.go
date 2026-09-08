package state_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
	"github.com/DKPlugins/TokenResetsMonitor/internal/state"
	bolt "go.etcd.io/bbolt"
)

var now = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

func event(id, confidence string) model.Event {
	return model.Event{ID: id, Provider: model.Provider{Slug: "codex"}, Revision: 1, Status: "published", EventType: "hard_reset", AnnouncedAt: now.Add(-24 * time.Hour), Confidence: model.Confidence{Label: confidence}}
}

func policy(channels map[string]string) state.Policy {
	return state.Policy{Providers: []string{"codex"}, Channels: channels, Match: func(e model.Event) (bool, string) {
		return e.Status == "published" && e.Confidence.Label == "reported", "confidence or status"
	}}
}

func openStore(t *testing.T) (*state.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func scan(t *testing.T, s *state.Store, p state.Policy, events ...model.Event) {
	t.Helper()
	if _, err := s.CommitScan("codex", events, p, now); err != nil {
		t.Fatal(err)
	}
}

func reconcile(t *testing.T, s *state.Store, p state.Policy) {
	t.Helper()
	if err := s.Reconcile(p); err != nil {
		t.Fatal(err)
	}
}

func due(t *testing.T, s *state.Store, channel string) []state.Delivery {
	t.Helper()
	values, err := s.Due(channel, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	return values
}

func TestBaselineLatePublicationAndRevision(t *testing.T) {
	s, _ := openStore(t)
	p := policy(map[string]string{"webhook": "destination"})
	reconcile(t, s, p)
	old := event("baseline", "reported")
	scan(t, s, p, old)
	if values := due(t, s, "webhook"); len(values) != 0 {
		t.Fatal("baseline must not notify")
	}
	late := event("late", "probable")
	late.AnnouncedAt = now.Add(-365 * 24 * time.Hour)
	scan(t, s, p, old, late)
	if values := due(t, s, "webhook"); len(values) != 0 {
		t.Fatal("low confidence event must not notify")
	}
	old.Revision++
	late.Revision++
	late.Confidence.Label = "reported"
	scan(t, s, p, old, late)
	values := due(t, s, "webhook")
	if len(values) != 1 || values[0].Notification.Event.ID != "late" {
		t.Fatalf("late revised event should notify once: %+v", values)
	}
	id := values[0].Notification.ID
	if err := s.Complete(id, model.DeliveryResult{Success: true}, now); err != nil {
		t.Fatal(err)
	}
	late.Revision++
	scan(t, s, p, old, late)
	if values := due(t, s, "webhook"); len(values) != 0 {
		t.Fatal("delivered event revision must not resend")
	}
}

func TestIncompleteScanRollsBackBaselineAndOutbox(t *testing.T) {
	s, _ := openStore(t)
	p := policy(map[string]string{"webhook": "destination"})
	reconcile(t, s, p)
	valid := event("first", "reported")
	bad := event("bad", "reported")
	bad.Provider.Slug = "other"
	if _, err := s.CommitScan("codex", []model.Event{valid, bad}, p, now); err == nil {
		t.Fatal("expected invalid scan rejection")
	}
	status, err := s.Status(true, now)
	if err != nil {
		t.Fatal(err)
	}
	if status.Providers["codex"].Ready || status.Pending != 0 {
		t.Fatalf("partial scan leaked: %+v", status)
	}
	scan(t, s, p, valid)
	if values := due(t, s, "webhook"); len(values) != 0 {
		t.Fatal("first complete scan must still be baseline")
	}
	newEvent := event("new", "reported")
	if _, err := s.CommitScan("codex", []model.Event{valid, newEvent, bad}, p, now); err == nil {
		t.Fatal("expected rejection")
	}
	if values := due(t, s, "webhook"); len(values) != 0 {
		t.Fatal("partial outbox commit")
	}
	scan(t, s, p, valid, newEvent)
	if values := due(t, s, "webhook"); len(values) != 1 {
		t.Fatal("rolled-back new event was lost")
	}
}

func TestChannelEnableDestinationAndFilterChangesDoNotReplay(t *testing.T) {
	s, _ := openStore(t)
	p := policy(map[string]string{"webhook": "first"})
	reconcile(t, s, p)
	scan(t, s, p)
	e := event("new", "reported")
	scan(t, s, p, e)
	p.Channels = map[string]string{"webhook": "first", "telegram": "chat"}
	reconcile(t, s, p)
	e.Revision++
	scan(t, s, p, e)
	if len(due(t, s, "telegram")) != 0 || len(due(t, s, "webhook")) != 1 {
		t.Fatal("new channel replayed an existing event")
	}
	p.Channels["webhook"] = "second"
	reconcile(t, s, p)
	e.Revision++
	scan(t, s, p, e)
	if len(due(t, s, "webhook")) != 0 {
		t.Fatal("new recipient retained or replayed old queue")
	}
	newEvent := event("later", "reported")
	scan(t, s, p, e, newEvent)
	if len(due(t, s, "webhook")) != 1 || len(due(t, s, "telegram")) != 1 {
		t.Fatal("new event did not reach both current recipients")
	}
	p.Match = func(model.Event) (bool, string) { return false, "narrowed" }
	reconcile(t, s, p)
	if len(due(t, s, "webhook")) != 0 || len(due(t, s, "telegram")) != 0 {
		t.Fatal("filter narrowing must cancel pending")
	}
	p.Match = func(model.Event) (bool, string) { return true, "" }
	reconcile(t, s, p)
	scan(t, s, p, e, newEvent)
	if len(due(t, s, "webhook")) != 0 {
		t.Fatal("widening replayed history")
	}
}

func TestWideningDoesNotNotifyUnchangedFilteredEvent(t *testing.T) {
	s, _ := openStore(t)
	p := policy(map[string]string{"webhook": "destination"})
	reconcile(t, s, p)
	scan(t, s, p)
	e := event("probable", "probable")
	scan(t, s, p, e)
	p.Match = func(model.Event) (bool, string) { return true, "" }
	reconcile(t, s, p)
	scan(t, s, p, e)
	if len(due(t, s, "webhook")) != 0 {
		t.Fatal("filter widening must not replay unchanged events")
	}
	e.Revision++
	scan(t, s, p, e)
	if len(due(t, s, "webhook")) != 1 {
		t.Fatal("subsequent source revision should be re-evaluated")
	}
}

func TestRemovedProviderGetsFreshBaselineWhenReenabled(t *testing.T) {
	s, _ := openStore(t)
	p := policy(map[string]string{"webhook": "destination"})
	reconcile(t, s, p)
	scan(t, s, p)
	a := event("old", "reported")
	scan(t, s, p, a)
	disabled := p
	disabled.Providers = nil
	reconcile(t, s, disabled)
	if len(due(t, s, "webhook")) != 0 {
		t.Fatal("removed provider retained pending work")
	}
	reconcile(t, s, p)
	b := event("while-disabled", "reported")
	scan(t, s, p, a, b)
	if len(due(t, s, "webhook")) != 0 {
		t.Fatal("reenabling provider replayed inactive history")
	}
	c := event("after-reenable", "reported")
	scan(t, s, p, a, b, c)
	if len(due(t, s, "webhook")) != 1 {
		t.Fatal("new active event lost")
	}
}

func TestDurableRetryIndependentChannelsAndRetryAfter(t *testing.T) {
	s, path := openStore(t)
	p := policy(map[string]string{"webhook": "endpoint", "telegram": "chat"})
	reconcile(t, s, p)
	scan(t, s, p)
	e := event("new", "reported")
	scan(t, s, p, e)
	webhook, telegram := due(t, s, "webhook")[0], due(t, s, "telegram")[0]
	if err := s.Complete(webhook.Notification.ID, model.DeliveryResult{Retryable: true, RetryAfter: time.Hour, Error: "rate limited"}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(telegram.Notification.ID, model.DeliveryResult{Success: true}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	soon, err := reopened.Due("webhook", now.Add(59*time.Minute), 0)
	if err != nil || len(soon) != 0 {
		t.Fatal("Retry-After not honored", err)
	}
	later, err := reopened.Due("webhook", now.Add(time.Hour), 0)
	if err != nil || len(later) != 1 || later[0].Notification.ID != webhook.Notification.ID || later[0].Attempts != 1 {
		t.Fatal("retry did not survive restart", err, later)
	}
	if len(due(t, reopened, "telegram")) != 0 {
		t.Fatal("successful channel was retried")
	}
	if state.RetryDelay(100000) != 30*time.Minute {
		t.Fatal("backoff not capped")
	}
}

func TestPermanentFailureRetryAndRetractionCancel(t *testing.T) {
	s, _ := openStore(t)
	p := policy(map[string]string{"webhook": "destination"})
	reconcile(t, s, p)
	scan(t, s, p)
	e := event("new", "reported")
	scan(t, s, p, e)
	d := due(t, s, "webhook")[0]
	if err := s.Complete(d.Notification.ID, model.DeliveryResult{Error: "forbidden"}, now); err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(true, now)
	if err != nil || status.Failed != 1 {
		t.Fatal("failed delivery not retained", err)
	}
	count, err := s.RetryFailed(now)
	if err != nil || count != 1 {
		t.Fatal("manual retry failed", err)
	}
	missing, err := s.MissingPending("codex", nil)
	if err != nil || len(missing) != 1 {
		t.Fatal("pending disappearance not identified", err)
	}
	scan(t, s, p) // A disappearance itself is not a retraction.
	if len(due(t, s, "webhook")) != 1 {
		t.Fatal("missing list entry canceled delivery")
	}
	e.Status = "retracted"
	e.Revision++
	scan(t, s, p, e)
	if len(due(t, s, "webhook")) != 0 {
		t.Fatal("verified retraction not canceled")
	}
	count, err = s.RetryFailed(now)
	if err != nil || count != 0 {
		t.Fatal("manual retry resurrected cancellation")
	}
}

func TestLockAndLiveStatusSnapshot(t *testing.T) {
	s, path := openStore(t)
	p := policy(map[string]string{})
	reconcile(t, s, p)
	scan(t, s, p)
	if err := s.PublishStatus(true, now); err != nil {
		t.Fatal(err)
	}
	status, err := state.ReadStatus(path)
	if err != nil || !status.Running || !status.Providers["codex"].Ready {
		t.Fatal("live status unreadable", err, status)
	}
	if other, err := state.Open(path); !errors.Is(err, state.ErrLocked) {
		if other != nil {
			_ = other.Close()
		}
		t.Fatalf("expected bounded lock error, got %v", err)
	}
	if err := s.PublishStatus(false, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	status, err = state.ReadStatus(path)
	if err != nil || status.Running {
		t.Fatal("status replacement failed", err)
	}
}

func TestLegacyMigrationBackupAndFutureSchemaRefusal(t *testing.T) {
	for _, version := range []string{"0", "2"} {
		t.Run(version, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			db, err := bolt.Open(path, 0600, nil)
			if err != nil {
				t.Fatal(err)
			}
			err = db.Update(func(tx *bolt.Tx) error {
				meta, err := tx.CreateBucket([]byte("meta"))
				if err != nil {
					return err
				}
				if err := meta.Put([]byte("state_schema_version"), []byte(version)); err != nil {
					return err
				}
				return meta.Put([]byte("canary"), []byte("preserve me"))
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			s, err := state.Open(path)
			if version == "2" {
				if err == nil {
					_ = s.Close()
					t.Fatal("future schema accepted")
				}
				after, readErr := os.ReadFile(path)
				if readErr != nil || !bytes.Equal(before, after) {
					t.Fatal("future schema database was modified")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			backups, err := filepath.Glob(path + ".backup-*")
			if err != nil || len(backups) != 1 {
				t.Fatal("migration backup missing", err)
			}
			for _, checkPath := range []string{path, backups[0]} {
				check, err := bolt.Open(checkPath, 0600, &bolt.Options{ReadOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				err = check.View(func(tx *bolt.Tx) error {
					meta := tx.Bucket([]byte("meta"))
					if string(meta.Get([]byte("canary"))) != "preserve me" {
						t.Fatal("migration lost existing data")
					}
					want := "1"
					if checkPath == backups[0] {
						want = "0"
					}
					if string(meta.Get([]byte("state_schema_version"))) != want {
						t.Fatal("schema version mismatch")
					}
					return nil
				})
				_ = check.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestCachePersistsBodyWithETag(t *testing.T) {
	s, path := openStore(t)
	want := model.CachedResponse{ETag: "version-1", Body: []byte(`{"events":[]}`)}
	if err := s.PutCache("https://example.test/page/2", want); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, found, err := s.GetCache("https://example.test/page/2")
	if err != nil || !found || got.ETag != want.ETag || !bytes.Equal(got.Body, want.Body) {
		t.Fatal("cache not durable", err, got)
	}
}
