package state_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
	"github.com/DKPlugins/TokenResetsMonitor/internal/state"
	bolt "go.etcd.io/bbolt"
)

func changesStore(t *testing.T) (*state.Store, string, state.Policy, time.Time) {
	t.Helper()
	store, path := openStore(t)
	p := policy(map[string]string{"webhook": "recipient"})
	p.Changes = map[string]bool{"webhook": true}
	p.FilterConfig = json.RawMessage(`{"minimum_confidence":"reported"}`)
	at := time.Now().UTC().Add(-time.Hour)
	reconcile(t, store, p)
	commitAt(t, store, p, at)
	return store, path, p, at
}
func commitAt(t *testing.T, s *state.Store, p state.Policy, at time.Time, events ...model.Event) {
	t.Helper()
	if _, err := s.CommitScan("codex", events, p, at); err != nil {
		t.Fatal(err)
	}
}
func claimAt(t *testing.T, s *state.Store, id string, at time.Time) state.Delivery {
	t.Helper()
	claim, ok, err := s.BeginAttempt(id, at)
	if err != nil || !ok || claim.AttemptID == "" {
		t.Fatalf("claim failed: %v %v %+v", err, ok, claim)
	}
	return claim
}
func finishAt(t *testing.T, s *state.Store, claim state.Delivery, p state.Policy, at time.Time) {
	t.Helper()
	if err := s.CompleteAttempt(claim.AttemptID, model.DeliveryResult{Success: true, StatusCode: 200}, p, at); err != nil {
		t.Fatal(err)
	}
}

func TestImmutableClaimRecordsActualRevisionAndQueuesCorrection(t *testing.T) {
	s, _, p, at := changesStore(t)
	original := event("changed", "reported")
	original.Scope.Plans = []string{"plus"}
	commitAt(t, s, p, at, original)
	first := dueAt(t, s, "webhook", at)[0]
	claim := claimAt(t, s, first.Notification.ID, at)
	if _, ok, err := s.BeginAttempt(first.Notification.ID, at); err != nil || ok {
		t.Fatal("claim was acquired twice", err)
	}
	revised := original
	revised.Revision = 2
	revised.Scope.Plans = []string{"pro"}
	commitAt(t, s, p, at.Add(time.Minute), revised)
	canceled, _, err := s.Delivery(first.Notification.ID)
	if err != nil || canceled.Status != "canceled" || canceled.Notification.Event.Revision != 1 {
		t.Fatal("in-flight payload mutated", canceled, err)
	}
	finishAt(t, s, claim, p, at.Add(2*time.Minute))
	delivered, _, _ := s.Delivery(first.Notification.ID)
	if delivered.Status != "delivered" || delivered.Notification.Event.Revision != 1 {
		t.Fatal("acknowledgment attributed to wrong revision", delivered)
	}
	corrections := dueAt(t, s, "webhook", at.Add(2*time.Minute))
	if len(corrections) != 1 {
		t.Fatal("missing correction", corrections)
	}
	n := corrections[0].Notification
	if n.Kind != "correction" || n.SchemaVersion != 1 || n.PreviousEvent == nil || n.PreviousEvent.Revision != 1 || n.Event.Revision != 2 || n.RelatedNotificationID != first.Notification.ID {
		t.Fatal("incorrect correction", n)
	}
	detail, found, err := s.DeliveryDetails(first.Notification.ID, state.Query{})
	if err != nil || !found || len(detail.AttemptHistory) != 1 || detail.AttemptHistory[0].Notification.Event.Revision != 1 || detail.AttemptHistory[0].Outcome != "delivered" {
		t.Fatal("missing immutable attempt history", detail, err)
	}
	finishAt(t, s, claim, p, at.Add(3*time.Minute))
	page, err := s.ListDeliveries(state.Query{})
	if err != nil || len(page.Items) != 2 {
		t.Fatal("completion replay duplicated correction", page, err)
	}
	correctionClaim := claimAt(t, s, n.ID, at.Add(3*time.Minute))
	newest := revised
	newest.Revision = 3
	newest.Scope.Plans = []string{"team"}
	commitAt(t, s, p, at.Add(4*time.Minute), newest)
	finishAt(t, s, correctionClaim, p, at.Add(5*time.Minute))
	followup := dueAt(t, s, "webhook", at.Add(5*time.Minute))
	if len(followup) != 1 || followup[0].Notification.PreviousEvent.Revision != 2 || followup[0].Notification.Event.Revision != 3 {
		t.Fatal("follow-up did not compare actual acknowledgments", followup)
	}
}

func TestSuccessfulInFlightAnnouncementStillProducesRetractionWhenFiltersRemoved(t *testing.T) {
	s, _, p, at := changesStore(t)
	e := event("withdrawn", "reported")
	commitAt(t, s, p, at, e)
	first := dueAt(t, s, "webhook", at)[0]
	claim := claimAt(t, s, first.Notification.ID, at)
	p.Providers = nil
	p.Match = func(model.Event) (bool, string) { return false, "provider_not_selected" }
	reconcile(t, s, p)
	e.Revision++
	e.Status = "retracted"
	if _, err := s.CommitDetails("codex", []model.Event{e}, p, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	finishAt(t, s, claim, p, at.Add(2*time.Minute))
	queued := dueAt(t, s, "webhook", at.Add(2*time.Minute))
	if len(queued) != 1 || queued[0].Notification.Kind != "retraction" || queued[0].Notification.PreviousEvent.Status != "published" {
		t.Fatal("acknowledged withdrawal was lost", queued)
	}
	status, err := s.Status(true, at)
	if err != nil || status.Providers["codex"].Ready {
		t.Fatal("detail lookup initialized removed provider", status, err)
	}
	p.Channels = map[string]string{"webhook": "new-recipient"}
	reconcile(t, s, p)
	if pending := dueAt(t, s, "webhook", at.Add(3*time.Minute)); len(pending) != 0 {
		t.Fatal("withdrawal sent to replacement recipient", pending)
	}
}

func TestRetractionBeforeAnyAttemptCancelsWithoutFollowup(t *testing.T) {
	s, _, p, at := changesStore(t)
	e := event("unsent", "reported")
	commitAt(t, s, p, at, e)
	first := dueAt(t, s, "webhook", at)[0]
	e.Revision++
	e.Status = "retracted"
	commitAt(t, s, p, at.Add(time.Minute), e)
	all, err := s.ListDeliveries(state.Query{})
	if err != nil || len(all.Items) != 1 || all.Items[0].Status != "canceled" {
		t.Fatal("unsent event generated withdrawal", all, err)
	}
	if _, err := s.RetryDelivery(first.Notification.ID, p, at); !errors.Is(err, state.ErrNotRetryable) {
		t.Fatal("canceled delivery retried", err)
	}
}

func TestFailedSupersededDeliveryRequiresTargetedRetry(t *testing.T) {
	s, _, p, at := changesStore(t)
	e := event("failed", "reported")
	commitAt(t, s, p, at, e)
	first := dueAt(t, s, "webhook", at)[0]
	claim := claimAt(t, s, first.Notification.ID, at)
	if _, err := s.RetryDelivery(first.Notification.ID, p, at); !errors.Is(err, state.ErrInFlight) {
		t.Fatal("in-flight retry not refused", err)
	}
	if err := s.CompleteAttempt(claim.AttemptID, model.DeliveryResult{StatusCode: 403, Error: "forbidden"}, p, at); err != nil {
		t.Fatal(err)
	}
	e.Revision++
	e.Scope.Plans = []string{"pro"}
	commitAt(t, s, p, at.Add(time.Minute), e)
	if queued := dueAt(t, s, "webhook", at.Add(time.Minute)); len(queued) != 0 {
		t.Fatal("revision automatically retried permanent failure", queued)
	}
	page, err := s.ListDeliveries(state.Query{Status: "failed"})
	if err != nil || len(page.Items) != 1 {
		t.Fatal("replacement permanent failure missing", page, err)
	}
	replacement := page.Items[0]
	if replacement.Notification.ID == first.Notification.ID || replacement.Notification.Event.Revision != 2 {
		t.Fatal("failed payload was overwritten", replacement)
	}
	if _, err := s.RetryDelivery(first.Notification.ID, p, at); !errors.Is(err, state.ErrSuperseded) {
		t.Fatal("superseded retry not refused", err)
	}
	retried, err := s.RetryDelivery(replacement.Notification.ID, p, at.Add(time.Minute))
	if err != nil || retried.Status != "pending" || retried.Notification.ID != replacement.Notification.ID {
		t.Fatal("target retry failed", retried, err)
	}
	if _, err := s.RetryDelivery(replacement.Notification.ID, p, at); !errors.Is(err, state.ErrAlreadyPending) {
		t.Fatal("repeated retry was not idempotently refused", err)
	}
}

func TestRestartRecoversSupersededClaimWithoutReplayingOtherFilteredEvents(t *testing.T) {
	s, path, p, at := changesStore(t)
	e := event("interrupted", "reported")
	filtered := event("still-filtered", "probable")
	commitAt(t, s, p, at, e, filtered)
	first := dueAt(t, s, "webhook", at)[0]
	claimAt(t, s, first.Notification.ID, at)
	e.Revision++
	e.Scope.Plans = []string{"pro"}
	commitAt(t, s, p, at.Add(time.Minute), e, filtered)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	p.Match = func(e model.Event) (bool, string) { return e.Status == "published", "matched" }
	reconcile(t, reopened, p)
	pending, err := reopened.Due("webhook", time.Now().Add(time.Second), 0)
	if err != nil || len(pending) != 1 || pending[0].Notification.Event.ID != e.ID || pending[0].Notification.Event.Revision != 2 {
		t.Fatal("recovery lost replacement or replayed unrelated history", pending, err)
	}
	history, _, err := reopened.DeliveryDetails(first.Notification.ID, state.Query{})
	if err != nil || len(history.AttemptHistory) != 1 || history.AttemptHistory[0].Outcome != "interrupted" {
		t.Fatal("interrupted attempt was not recorded", history, err)
	}
}

func TestExplainKeepsHistoricalFiltersAndChannelDiscovery(t *testing.T) {
	s, _, p, at := changesStore(t)
	e := event("filtered", "probable")
	commitAt(t, s, p, at, e)
	expanded := p
	expanded.Match = func(model.Event) (bool, string) { return true, "matched" }
	expanded.Channels = map[string]string{"webhook": "recipient", "telegram": "new-chat"}
	reconcile(t, s, expanded)
	explanation, found, err := s.Explain("", e.ID, expanded)
	if err != nil || !found || !explanation.CurrentMatched || explanation.Record.Decision.Matched {
		t.Fatal("historical and current filters were conflated", explanation, err)
	}
	reasons := map[string]string{}
	for _, channel := range explanation.Channels {
		reasons[channel.Channel] = channel.Reason
	}
	if reasons["telegram"] != "channel_not_enabled_at_discovery" || reasons["webhook"] != pReason(p, e) {
		t.Fatal("channel discovery explanation missing", reasons)
	}
	expanded.Channels = map[string]string{"webhook": "changed-recipient"}
	reconcile(t, s, expanded)
	explanation, _, err = s.Explain("codex", e.ID, expanded)
	if err != nil || explanation.Record.DiscoveryChannels["webhook"] != "recipient" {
		t.Fatal("reconcile rewrote discovery facts", explanation, err)
	}
	reasons = map[string]string{}
	for _, channel := range explanation.Channels {
		reasons[channel.Channel] = channel.Reason
	}
	if reasons["webhook"] != "recipient_changed" {
		t.Fatal("recipient change was not explained", reasons)
	}
}
func pReason(p state.Policy, e model.Event) string { _, reason := p.Match(e); return reason }

func TestHistoryPaginationSortsFractionsAndKeepsRelativeSinceStable(t *testing.T) {
	s, _, p, at := changesStore(t)
	at = at.Truncate(time.Second)
	a, b, c := event("a", "reported"), event("b", "reported"), event("c", "reported")
	commitAt(t, s, p, at, a)
	commitAt(t, s, p, at.Add(100*time.Millisecond), a, b)
	commitAt(t, s, p, at.Add(900*time.Millisecond), a, b, c)
	query := state.Query{Limit: 1, SinceDuration: "24h"}
	first, err := s.ListEvents(query)
	if err != nil || len(first.Items) != 1 || first.Items[0].Event.ID != "c" || first.NextCursor == "" {
		t.Fatal("newest fractional timestamp order wrong", first, err)
	}
	query.Cursor = first.NextCursor
	second, err := s.ListEvents(query)
	if err != nil || len(second.Items) != 1 || second.Items[0].Event.ID != "b" || !first.AsOf.Equal(second.AsOf) {
		t.Fatal("relative-time pagination drifted", second, err)
	}
	query.Cursor = second.NextCursor
	third, err := s.ListEvents(query)
	if err != nil || len(third.Items) != 1 || third.Items[0].Event.ID != "a" || third.NextCursor != "" {
		t.Fatal("pagination omitted whole-second event", third, err)
	}
	query.Provider = "different"
	if _, err := s.ListEvents(query); err == nil {
		t.Fatal("cursor accepted different filter")
	}
}

func TestReadOnlyLegacyStateDoesNotMigrateAndMigrationMarksUnknownHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := event("old", "reported")
	legacyID := "old-delivery"
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{"meta", "providers", "events", "outbox", "cache", "cooldowns"} {
			if _, err := tx.CreateBucket([]byte(name)); err != nil {
				return err
			}
		}
		if err := tx.Bucket([]byte("meta")).Put([]byte("state_schema_version"), []byte("2")); err != nil {
			return err
		}
		record := state.EventRecord{Event: e, DetectedAt: time.Now().Add(-time.Hour), AllowedChannels: map[string]string{"webhook": "recipient"}, DeliveryIDs: map[string]string{"webhook": legacyID}}
		raw, _ := json.Marshal(record)
		if err := tx.Bucket([]byte("events")).Put([]byte("codex\x00old"), raw); err != nil {
			return err
		}
		delivery := state.Delivery{Channel: "webhook", Destination: "recipient", Status: "delivered", Attempts: 3, Notification: model.Notification{SchemaVersion: 1, ID: legacyID, Event: e}}
		raw, _ = json.Marshal(delivery)
		return tx.Bucket([]byte("outbox")).Put([]byte(legacyID), raw)
	})
	if closeErr := db.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := state.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	view, found, readErr := reader.Event("codex", "old")
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || !found || !view.Record.Decision.Unavailable || !view.Record.DiscoveryUnavailable {
		t.Fatal("legacy history unavailable", view, readErr, closeErr)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("read-only history modified legacy state", err)
	}
	backups, _ := filepath.Glob(path + ".backup-*")
	if len(backups) != 0 {
		t.Fatal("read-only query created migration backup")
	}
	migrated, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	view, _, err = migrated.Event("codex", "old")
	if err != nil || !view.Record.DiscoveryUnavailable || !view.Record.Acknowledged["webhook"].LegacyUnverified || view.Record.Acknowledged["webhook"].NotificationID != legacyID {
		t.Fatal("migration invented exact historical truth", view, err)
	}
	details, _, err := migrated.DeliveryDetails(legacyID, state.Query{})
	if err != nil || details.LegacyAttempts != 3 || len(details.AttemptHistory) != 0 {
		t.Fatal("migration fabricated attempt details", details, err)
	}
}

func TestRetentionPreservesAcknowledgedAndPendingPayloadsAndHonorsBatchLimit(t *testing.T) {
	s, _, p, at := changesStore(t)
	at = at.Add(-30 * 24 * time.Hour)
	e := event("retained", "reported")
	e.Scope.Plans = []string{"plus"}
	commitAt(t, s, p, at, e)
	first := dueAt(t, s, "webhook", at)[0]
	finishAt(t, s, claimAt(t, s, first.Notification.ID, at), p, at)
	e.Revision++
	e.Title = "Presentation-only edit"
	commitAt(t, s, p, at.Add(time.Hour), e)
	e.Revision++
	e.Scope.Plans = []string{"pro"}
	commitAt(t, s, p, at.Add(2*time.Hour), e)
	e.Revision++
	e.Scope.Plans = []string{"team"}
	commitAt(t, s, p, at.Add(3*time.Hour), e)
	if count, err := s.PruneHistory(time.Now(), 0, 200); err != nil || count != 0 {
		t.Fatal("default retention pruned history", count, err)
	}
	count, err := s.PruneHistory(time.Now(), 7, 1)
	if err != nil || count != 1 {
		t.Fatal("retention batch did not obey limit", count, err)
	}
	if _, err := s.PruneHistory(time.Now(), 7, 200); err != nil {
		t.Fatal(err)
	}
	view, found, err := s.Event("codex", e.ID)
	if err != nil || !found || view.Record.HistoryPrunedBefore == nil || view.Record.Acknowledged["webhook"].Event.Revision != 1 {
		t.Fatal("retention removed acknowledgment or missing pruning metadata", view, err)
	}
	pending := dueAt(t, s, "webhook", time.Now())
	if len(pending) != 1 || pending[0].Notification.PreviousEvent.Revision != 1 || pending[0].Notification.Event.Revision != 4 {
		t.Fatal("retention damaged pending comparison", pending)
	}
	kept := map[int]bool{}
	for _, revision := range view.Revisions {
		kept[revision.Event.Revision] = true
	}
	if !kept[1] || !kept[4] || kept[2] || kept[3] {
		t.Fatal("wrong revision retention set", kept)
	}
}

func TestDetailTrackingRotatesAndDoesNotInitializeRemovedProvider(t *testing.T) {
	s, _, p, at := changesStore(t)
	a, b := event("a", "reported"), event("b", "reported")
	commitAt(t, s, p, at, a, b)
	for _, delivery := range dueAt(t, s, "webhook", at) {
		finishAt(t, s, claimAt(t, s, delivery.Notification.ID, at), p, at)
	}
	p.Providers = nil
	reconcile(t, s, p)
	tracked, err := s.TrackedProviders(p)
	if err != nil || len(tracked) != 1 || tracked[0] != "codex" {
		t.Fatal("removed provider acknowledgments not tracked", tracked, err)
	}
	missing, err := s.MissingTracked("codex", nil)
	if err != nil || len(missing) != 2 || missing[0].ID != "a" {
		t.Fatal("missing detail candidates", missing, err)
	}
	if err := s.AdvanceDetailCursor("codex", "a"); err != nil {
		t.Fatal(err)
	}
	rotated, err := s.MissingTracked("codex", nil)
	if err != nil || rotated[0].ID != "b" {
		t.Fatal("detail cursor did not rotate", rotated, err)
	}
	a.Revision++
	a.Status = "retracted"
	if _, err := s.CommitDetails("codex", []model.Event{a}, p, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(true, at)
	if err != nil || len(status.Providers) != 0 {
		t.Fatal("detail lookup reactivated provider", status, err)
	}
	if queued := dueAt(t, s, "webhook", at.Add(time.Minute)); len(queued) != 1 || queued[0].Notification.Kind != "retraction" {
		t.Fatal("removed provider withdrawal was lost", queued)
	}
}

func TestExplainIdentifiesSuppressedAmendmentsWithoutChangingDeliveryStatus(t *testing.T) {
	s, _, p, at := changesStore(t)
	original := event("suppressed-change", "reported")
	original.Scope.Plans = []string{"plus"}
	commitAt(t, s, p, at, original)
	first := dueAt(t, s, "webhook", at)[0]
	finishAt(t, s, claimAt(t, s, first.Notification.ID, at), p, at)
	p.Changes = map[string]bool{"webhook": false}
	reconcile(t, s, p)
	revised := original
	revised.Revision = 2
	revised.Scope.Plans = []string{"pro"}
	commitAt(t, s, p, at.Add(time.Minute), revised)
	for _, tc := range []struct {
		name        string
		channels    map[string]string
		changes     map[string]bool
		reason      string
		unavailable bool
	}{
		{"changes_disabled", map[string]string{"webhook": "recipient"}, map[string]bool{"webhook": false}, "changes_disabled", false},
		{"channel_disabled", map[string]string{}, map[string]bool{"webhook": false}, "channel_disabled", false},
		{"recipient_changed", map[string]string{"webhook": "other-recipient"}, map[string]bool{"webhook": false}, "recipient_changed", false},
		{"offline_unknown", nil, nil, "already_delivered", true},
		{"reenabled_without_replay", map[string]string{"webhook": "recipient"}, map[string]bool{"webhook": true}, "awaiting_source_revision", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := p
			current.Channels, current.Changes = tc.channels, tc.changes
			if tc.channels != nil {
				reconcile(t, s, current)
			}
			explained, found, err := s.Explain("codex", original.ID, current)
			if err != nil || !found {
				t.Fatal(err)
			}
			var channel state.ChannelExplanation
			for _, item := range explained.Channels {
				if item.Channel == "webhook" {
					channel = item
				}
			}
			if channel.Status != "delivered" || channel.Reason != tc.reason || channel.AcknowledgedRevision != 1 || channel.LatestRevision != 2 || channel.CurrentRecipientUnavailable != tc.unavailable {
				t.Fatalf("incorrect suppression explanation: %+v", channel)
			}
			if explained.Reason != tc.reason {
				t.Fatalf("aggregate reason %q differs from channel %q", explained.Reason, tc.reason)
			}
			if pending := dueAt(t, s, "webhook", time.Now()); len(pending) != 0 {
				t.Fatal("explanation or configuration replayed an old change", pending)
			}
		})
	}
	// A revision number or presentation edit alone is not a suppressed amendment.
	reconcile(t, s, p)
	presentation := original
	presentation.Revision = 3
	presentation.Title = "Spelling corrected"
	commitAt(t, s, p, at.Add(2*time.Minute), presentation)
	explanation, _, err := s.Explain("codex", original.ID, p)
	if err != nil || explanation.Reason != "already_delivered" {
		t.Fatal("presentation edit was described as a suppressed amendment", explanation, err)
	}
}

func TestNewAmendmentSupersedesPermanentFailureAndStartsPending(t *testing.T) {
	for _, kind := range []string{"correction", "retraction"} {
		t.Run(kind, func(t *testing.T) {
			s, _, p, at := changesStore(t)
			original := event("amendment-failure", "reported")
			original.Scope.Plans = []string{"plus"}
			commitAt(t, s, p, at, original)
			initial := dueAt(t, s, "webhook", at)[0]
			finishAt(t, s, claimAt(t, s, initial.Notification.ID, at), p, at)
			revised := original
			revised.Revision = 2
			revised.Scope.Plans = []string{"pro"}
			commitAt(t, s, p, at.Add(time.Minute), revised)
			correction := dueAt(t, s, "webhook", at.Add(time.Minute))[0]
			claim := claimAt(t, s, correction.Notification.ID, at.Add(time.Minute))
			if err := s.CompleteAttempt(claim.AttemptID, model.DeliveryResult{StatusCode: 403, Error: "forbidden"}, p, at.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			newest := revised
			newest.Revision = 3
			if kind == "retraction" {
				newest.Status = "retracted"
			} else {
				newest.Scope.Plans = []string{"team"}
			}
			commitAt(t, s, p, at.Add(2*time.Minute), newest)
			pending := dueAt(t, s, "webhook", at.Add(2*time.Minute))
			if len(pending) != 1 || pending[0].Status != "pending" || pending[0].Notification.Kind != kind || pending[0].Notification.PreviousEvent.Revision != 1 || pending[0].Attempts != 0 || pending[0].LastError != "" {
				t.Fatal("obsolete amendment failure blocked latest update", pending)
			}
			old, found, err := s.DeliveryDetails(correction.Notification.ID, state.Query{})
			if err != nil || !found || old.Status != "canceled" || old.CancellationReason != "superseded" || old.LastError != "forbidden" || len(old.AttemptHistory) != 1 || old.AttemptHistory[0].Outcome != "failed" || old.AttemptHistory[0].Error != "forbidden" {
				t.Fatal("superseding lost historical failure", old, err)
			}
		})
	}
}

func TestRelativeSinceRejectsUntilBeforeCapturedWindow(t *testing.T) {
	s, _, _, _ := changesStore(t)
	until := time.Now().Add(-2 * time.Hour)
	if _, err := s.ListEvents(state.Query{SinceDuration: "1h", Until: &until}); err == nil {
		t.Fatal("relative since after until was accepted")
	}
}
