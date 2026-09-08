package state_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
	"github.com/DKPlugins/TokenResetsMonitor/internal/state"
	bolt "go.etcd.io/bbolt"
)

func TestStaleRevisionCannotCancelOrReplacePendingDelivery(t *testing.T) {
	s, _ := openStore(t)
	p := policy(map[string]string{"webhook": "recipient"})
	reconcile(t, s, p)
	scan(t, s, p)
	current := event("revised", "reported")
	current.Revision = 2
	scan(t, s, p, current)
	first := due(t, s, "webhook")[0]
	stale := current
	stale.Revision = 1
	stale.Status = "retracted"
	stale.Confidence.Label = "unverified"
	detections, err := s.CommitScan("codex", []model.Event{stale}, p, now)
	if err != nil || len(detections) != 1 || detections[0].Reason != "stale_revision" {
		t.Fatalf("stale revision was not diagnosed: %+v %v", detections, err)
	}
	scan(t, s, p, current)
	pending := due(t, s, "webhook")
	if len(pending) != 1 || pending[0].Notification.ID != first.Notification.ID || pending[0].Notification.Event.Revision != 2 || pending[0].Notification.Event.Status != "published" {
		t.Fatalf("stale revision altered the latest pending delivery: %+v", pending)
	}

	disabled := p
	disabled.Providers = nil
	reconcile(t, s, disabled)
	reconcile(t, s, p)
	scan(t, s, p, stale)
	scan(t, s, p, current)
	if len(due(t, s, "webhook")) != 0 {
		t.Fatal("reenabling with a stale revision replayed the baseline")
	}
}

func TestRecipientCooldownSurvivesRestartAndConfigurationChanges(t *testing.T) {
	s, path := openStore(t)
	p := policy(map[string]string{"webhook": "recipient", "telegram": "chat"})
	reconcile(t, s, p)
	scan(t, s, p)
	a, b := event("first", "reported"), event("second", "reported")
	scan(t, s, p, a, b)
	first := due(t, s, "webhook")[0]
	if err := s.Complete(first.Notification.ID, model.DeliveryResult{Retryable: true, StatusCode: 429, RetryAfter: time.Hour}, now); err != nil {
		t.Fatal(err)
	}
	if len(due(t, s, "webhook")) != 0 || len(due(t, s, "telegram")) != 2 {
		t.Fatal("recipient cooldown did not isolate the throttled destination")
	}
	if err := s.Complete(first.Notification.ID, model.DeliveryResult{Retryable: true, StatusCode: 429, RetryAfter: time.Second}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	deadline, err := s.RecipientCooldown("webhook", "recipient")
	if err != nil || !deadline.Equal(now.Add(time.Hour)) {
		t.Fatal("shorter response reduced recipient cooldown", deadline, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	before, err := reopened.Due("webhook", now.Add(time.Hour-time.Second), 0)
	if err != nil || len(before) != 0 {
		t.Fatal("recipient cooldown did not survive restart", before, err)
	}
	after, err := reopened.Due("webhook", now.Add(time.Hour), 0)
	if err != nil || len(after) != 2 {
		t.Fatal("cooldown expiration lost pending work", after, err)
	}

	disabled := policy(map[string]string{"telegram": "chat"})
	reconcile(t, reopened, disabled)
	reconcile(t, reopened, p)
	c := event("after-reenable", "reported")
	scan(t, reopened, p, a, b, c)
	if len(due(t, reopened, "webhook")) != 0 {
		t.Fatal("reenabling the same recipient cleared its cooldown")
	}
	p.Channels["webhook"] = "other-recipient"
	reconcile(t, reopened, p)
	d := event("after-recipient-change", "reported")
	scan(t, reopened, p, a, b, c, d)
	if len(due(t, reopened, "webhook")) != 1 {
		t.Fatal("old recipient cooldown leaked into the new recipient")
	}
}

func TestRateLimitWithoutRetryAfterUsesSharedDefaultDelay(t *testing.T) {
	for _, result := range []model.DeliveryResult{
		{Retryable: true, StatusCode: 429},
		{Retryable: true, StatusCode: 200, RateLimited: true},
	} {
		s, _ := openStore(t)
		p := policy(map[string]string{"telegram": "chat"})
		reconcile(t, s, p)
		scan(t, s, p)
		scan(t, s, p, event("first", "reported"), event("second", "reported"))
		first := due(t, s, "telegram")[0]
		if err := s.Complete(first.Notification.ID, result, now); err != nil {
			t.Fatal(err)
		}
		deadline, err := s.RecipientCooldown("telegram", "chat")
		if err != nil || !deadline.Equal(now.Add(state.RetryDelay(1))) || len(due(t, s, "telegram")) != 0 {
			t.Fatal("rate limit without a header did not pause the recipient", deadline, err)
		}
	}
}

func TestRetractionDuringSendStillPersistsRecipientCooldown(t *testing.T) {
	s, _ := openStore(t)
	p := policy(map[string]string{"webhook": "recipient"})
	reconcile(t, s, p)
	scan(t, s, p)
	a, b := event("first", "reported"), event("second", "reported")
	scan(t, s, p, a)
	first := due(t, s, "webhook")[0]
	a.Revision++
	a.Status = "retracted"
	scan(t, s, p, a, b)
	if err := s.Complete(first.Notification.ID, model.DeliveryResult{Retryable: true, StatusCode: 429, RetryAfter: time.Minute}, now); err != nil {
		t.Fatal(err)
	}
	if len(due(t, s, "webhook")) != 0 {
		t.Fatal("a cancellation during send lost the recipient cooldown")
	}
	delivery, _, err := s.Delivery(first.Notification.ID)
	if err != nil || delivery.Status != "canceled" {
		t.Fatal("late result resurrected a canceled delivery", delivery, err)
	}
}

func TestStatusHasQueueBreakdownAndKeepsImmutableRuntime(t *testing.T) {
	s, path := openStore(t)
	p := policy(map[string]string{"webhook": "secret-recipient", "telegram": "secret-chat"})
	reconcile(t, s, p)
	scan(t, s, p)
	a, b := event("first", "reported"), event("second", "reported")
	scan(t, s, p, a)
	firstWebhook := due(t, s, "webhook")[0]
	firstTelegram := due(t, s, "telegram")[0]
	if _, err := s.CommitScan("codex", []model.Event{a, b}, p, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(firstWebhook.Notification.ID, model.DeliveryResult{Success: true}, now); err != nil {
		t.Fatal(err)
	}
	secondWebhook := dueAt(t, s, "webhook", now.Add(time.Minute))[0]
	if err := s.Complete(secondWebhook.Notification.ID, model.DeliveryResult{Error: "forbidden"}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(firstTelegram.Notification.ID, model.DeliveryResult{Retryable: true, StatusCode: 429, RetryAfter: time.Hour}, now); err != nil {
		t.Fatal(err)
	}
	checked := now
	runtime := &model.RuntimeStatus{Version: "test-version", LastReloadAt: &checked, Updates: &model.UpdateStatus{LatestVersion: "2.0.0"}}
	if err := s.PublishStatusWithRuntime(true, now, runtime); err != nil {
		t.Fatal(err)
	}
	runtime.Version = "mutated"
	runtime.Updates.LatestVersion = "mutated"
	checked = now.Add(time.Hour)
	if err := s.PublishStatus(true, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	status, err := state.ReadStatus(path)
	if err != nil {
		t.Fatal(err)
	}
	if status.Pending != 2 || status.Failed != 1 || status.Channels["webhook"].Delivered != 1 || status.Channels["webhook"].Failed != 1 || status.Channels["telegram"].Pending != 2 {
		t.Fatalf("queue statistics are incorrect: %+v", status)
	}
	telegram, provider := status.Channels["telegram"], status.Providers["codex"]
	if telegram.OldestPending == nil || !telegram.OldestPending.Equal(now) || telegram.CooldownUntil == nil || !telegram.CooldownUntil.Equal(now.Add(time.Hour)) || provider.Pending != 2 || provider.Failed != 1 || provider.OldestPending == nil || !provider.OldestPending.Equal(now) {
		t.Fatalf("queue age or cooldown is incorrect: %+v %+v", telegram, provider)
	}
	if status.Runtime == nil || status.Runtime.Version != "test-version" || status.Runtime.Updates.LatestVersion != "2.0.0" || !status.Runtime.LastReloadAt.Equal(now) {
		t.Fatalf("ordinary publication lost or mutated runtime metadata: %+v", status.Runtime)
	}
	status.Runtime.Updates.LatestVersion = "changed-by-reader"
	current, err := s.Status(true, now)
	if err != nil || current.Runtime.Updates.LatestVersion != "2.0.0" {
		t.Fatal("returned status retained mutable runtime pointers", err)
	}
	encoded, err := json.Marshal(current)
	if err != nil || strings.Contains(string(encoded), "secret-recipient") || strings.Contains(string(encoded), "secret-chat") {
		t.Fatal("operational status exposed recipient identity", err)
	}
}

func dueAt(t *testing.T, s *state.Store, channel string, at time.Time) []state.Delivery {
	t.Helper()
	deliveries, err := s.Due(channel, at, 0)
	if err != nil {
		t.Fatal(err)
	}
	return deliveries
}

func TestSchemaOneMigrationPreservesPendingIdentityAndBackup(t *testing.T) {
	s, path := openStore(t)
	p := policy(map[string]string{"webhook": "recipient"})
	reconcile(t, s, p)
	scan(t, s, p)
	scan(t, s, p, event("pending", "reported"))
	original := due(t, s, "webhook")[0]
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	legacy, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Update(func(tx *bolt.Tx) error {
		if err := tx.DeleteBucket([]byte("cooldowns")); err != nil {
			return err
		}
		return tx.Bucket([]byte("meta")).Put([]byte("state_schema_version"), []byte("1"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	pending := due(t, migrated, "webhook")
	if len(pending) != 1 || pending[0].Notification.ID != original.Notification.ID || !pending[0].Notification.DetectedAt.Equal(original.Notification.DetectedAt) {
		t.Fatal("schema migration changed pending notification identity", pending)
	}
	if err := migrated.Complete(original.Notification.ID, model.DeliveryResult{Retryable: true, StatusCode: 429}, now); err != nil {
		t.Fatal("migrated state cannot persist recipient cooldown", err)
	}
	backups, err := filepath.Glob(path + ".backup-*")
	if err != nil || len(backups) != 1 {
		t.Fatal("migration backup is missing", backups, err)
	}
	backup, err := bolt.Open(backups[0], 0600, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	if err := backup.View(func(tx *bolt.Tx) error {
		if string(tx.Bucket([]byte("meta")).Get([]byte("state_schema_version"))) != "1" || tx.Bucket([]byte("cooldowns")) != nil {
			t.Fatal("backup no longer contains the original schema")
		}
		var delivery state.Delivery
		if err := json.Unmarshal(tx.Bucket([]byte("outbox")).Get([]byte(original.Notification.ID)), &delivery); err != nil {
			return err
		}
		if delivery.Status != "pending" || delivery.Attempts != 0 {
			t.Fatal("migration backup changed original pending work", delivery)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
