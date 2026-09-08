package state_test

import (
	"testing"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
	"github.com/DKPlugins/TokenResetsMonitor/internal/state"
)

func TestAmendmentsCoalesceAgainstEachRecipientsAcknowledgment(t *testing.T) {
	s, _, p, at := changesStore(t)
	p.Channels["telegram"] = "second-recipient"
	p.Changes["telegram"] = true
	reconcile(t, s, p)
	e := event("independent-changes", "reported")
	e.Scope.Plans = []string{"plus"}
	commitAt(t, s, p, at, e)
	firstWebhook := claimAt(t, s, dueAt(t, s, "webhook", at)[0].Notification.ID, at)
	finishAt(t, s, firstWebhook, p, at)
	firstTelegram := claimAt(t, s, dueAt(t, s, "telegram", at)[0].Notification.ID, at)
	if err := s.CompleteAttempt(firstTelegram.AttemptID, model.DeliveryResult{Retryable: true, Error: "temporary"}, p, at); err != nil {
		t.Fatal(err)
	}

	e.Revision = 2
	e.Scope.Plans = []string{"pro"}
	commitAt(t, s, p, at.Add(time.Minute), e)
	staleCorrection := dueAt(t, s, "webhook", at.Add(time.Minute))[0]
	telegramAnnouncement := dueAt(t, s, "telegram", at.Add(time.Minute))[0]
	if telegramAnnouncement.Notification.Kind != "" || telegramAnnouncement.Notification.PreviousEvent != nil {
		t.Fatal("unsent recipient received correction")
	}
	finishAt(t, s, claimAt(t, s, telegramAnnouncement.Notification.ID, at.Add(time.Minute)), p, at.Add(time.Minute))

	e.Revision = 3
	e.Scope.Plans = []string{"team", "enterprise"}
	commitAt(t, s, p, at.Add(2*time.Minute), e)
	webhook := dueAt(t, s, "webhook", at.Add(2*time.Minute))[0]
	telegram := dueAt(t, s, "telegram", at.Add(2*time.Minute))[0]
	if webhook.Notification.PreviousEvent.Revision != 1 || telegram.Notification.PreviousEvent.Revision != 2 ||
		webhook.Notification.Event.Revision != 3 || telegram.Notification.Event.Revision != 3 {
		t.Fatal("channel acknowledgments contaminated each other")
	}
	old, found, err := s.Delivery(staleCorrection.Notification.ID)
	if err != nil || !found || old.CancellationReason != "superseded" || old.Status != "canceled" || old.Notification.Event.Revision != 2 {
		t.Fatal("coalescing erased old delivery history", old, err)
	}

	// Order, prose, score, and source revision alone must not replace queued amendments.
	e.Revision = 4
	e.Title = "Presentation-only edit"
	e.Scope.Plans = []string{"enterprise", "team"}
	e.Confidence.Score = 0.99
	commitAt(t, s, p, at.Add(3*time.Minute), e)
	if got := dueAt(t, s, "webhook", at.Add(3*time.Minute)); len(got) != 1 || got[0].Notification.ID != webhook.Notification.ID {
		t.Fatal("presentation-only update replaced amendment")
	}
	finishAt(t, s, claimAt(t, s, webhook.Notification.ID, at.Add(3*time.Minute)), p, at.Add(3*time.Minute))
	finishAt(t, s, claimAt(t, s, telegram.Notification.ID, at.Add(3*time.Minute)), p, at.Add(3*time.Minute))
	commitAt(t, s, p, at.Add(4*time.Minute), e)
	for _, channel := range []string{"webhook", "telegram"} {
		if got := dueAt(t, s, channel, at.Add(4*time.Minute)); len(got) != 0 {
			t.Fatal("identical scan duplicated delivered amendment", channel, got)
		}
	}
}

func TestPendingAmendmentReturningToAcknowledgedContentIsCanceled(t *testing.T) {
	s, _, p, at := changesStore(t)
	e := event("reverted-change", "reported")
	e.Scope.Plans = []string{"plus"}
	commitAt(t, s, p, at, e)
	finishAt(t, s, claimAt(t, s, dueAt(t, s, "webhook", at)[0].Notification.ID, at), p, at)
	e.Revision = 2
	e.Scope.Plans = []string{"pro"}
	commitAt(t, s, p, at.Add(time.Minute), e)
	queued := dueAt(t, s, "webhook", at.Add(time.Minute))[0]
	e.Revision = 3
	e.Scope.Plans = []string{"plus"}
	commitAt(t, s, p, at.Add(2*time.Minute), e)
	if got := dueAt(t, s, "webhook", at.Add(2*time.Minute)); len(got) != 0 {
		t.Fatal("reverted change queued an unnecessary message")
	}
	detail, found, err := s.DeliveryDetails(queued.Notification.ID, state.Query{})
	if err != nil || !found || detail.Status != "canceled" || detail.CancellationReason != "no_meaningful_change" {
		t.Fatal("reverted amendment cancellation not explained", detail, err)
	}
}
