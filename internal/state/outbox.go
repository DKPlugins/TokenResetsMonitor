package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
	bolt "go.etcd.io/bbolt"
)

type Delivery struct {
	Channel              string             `json:"channel"`
	Destination          string             `json:"destination"`
	Status               string             `json:"status"`
	Notification         model.Notification `json:"notification"`
	Attempts             int                `json:"attempts"`
	NextAttempt          time.Time          `json:"next_attempt"`
	LastError            string             `json:"last_error,omitempty"`
	DeliveredAt          *time.Time         `json:"delivered_at,omitempty"`
	CreatedAt            time.Time          `json:"created_at"`
	AttemptID            string             `json:"attempt_id,omitempty"`
	CancellationReason   string             `json:"cancellation_reason,omitempty"`
	CanceledAt           *time.Time         `json:"canceled_at,omitempty"`
	EffectiveNextAttempt *time.Time         `json:"effective_next_attempt,omitempty"`
	LegacyAttempts       int                `json:"legacy_attempts,omitempty"`
	HistoryPrunedBefore  *time.Time         `json:"history_pruned_before,omitempty"`
}

type Attempt struct {
	ID                string             `json:"id"`
	DeliveryID        string             `json:"delivery_id"`
	Number            int                `json:"number"`
	StartedAt         time.Time          `json:"started_at"`
	FinishedAt        *time.Time         `json:"finished_at,omitempty"`
	Outcome           string             `json:"outcome"`
	StatusCode        int                `json:"status_code,omitempty"`
	Error             string             `json:"error,omitempty"`
	DurationMS        int64              `json:"duration_ms"`
	Retryable         bool               `json:"retryable"`
	RetryAfterSeconds float64            `json:"retry_after_seconds,omitempty"`
	NextAttempt       *time.Time         `json:"next_attempt,omitempty"`
	Notification      model.Notification `json:"notification"`
}

func effectiveDelivery(tx *bolt.Tx, d Delivery) (Delivery, error) {
	d.EffectiveNextAttempt = nil
	if d.Status == "pending" && d.AttemptID == "" {
		deadline, err := recipientCooldown(tx, d.Channel, d.Destination)
		if err != nil {
			return d, err
		}
		if d.NextAttempt.After(deadline) {
			deadline = d.NextAttempt
		}
		d.EffectiveNextAttempt = timePointer(deadline)
	}
	return d, nil
}

func (s *Store) Due(channel string, now time.Time, limit int) ([]Delivery, error) {
	var deliveries []Delivery
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("outbox")).ForEach(func(_, v []byte) error {
			var d Delivery
			if err := json.Unmarshal(v, &d); err != nil {
				return err
			}
			if d.Channel == channel && d.Status == "pending" && d.AttemptID == "" && !d.NextAttempt.After(now) {
				deadline, err := recipientCooldown(tx, channel, d.Destination)
				if err != nil {
					return err
				}
				if !deadline.After(now) {
					deliveries = append(deliveries, d)
				}
			}
			return nil
		})
	})
	sort.Slice(deliveries, func(i, j int) bool {
		if deliveries[i].NextAttempt.Equal(deliveries[j].NextAttempt) {
			return deliveries[i].Notification.ID < deliveries[j].Notification.ID
		}
		return deliveries[i].NextAttempt.Before(deliveries[j].NextAttempt)
	})
	if limit > 0 && len(deliveries) > limit {
		deliveries = deliveries[:limit]
	}
	return deliveries, err
}

func (s *Store) Delivery(id string) (Delivery, bool, error) {
	var delivery Delivery
	var found bool
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		found, err = getJSON(tx.Bucket([]byte("outbox")), id, &delivery)
		if err != nil || !found {
			return err
		}
		delivery, err = effectiveDelivery(tx, delivery)
		return err
	})
	return delivery, found, err
}

// BeginAttempt atomically claims an immutable payload before network I/O.
func (s *Store) BeginAttempt(id string, now time.Time) (Delivery, bool, error) {
	var delivery Delivery
	claimed := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		outbox := tx.Bucket([]byte("outbox"))
		found, err := getJSON(outbox, id, &delivery)
		if err != nil || !found {
			return err
		}
		if delivery.Status != "pending" || delivery.AttemptID != "" || delivery.NextAttempt.After(now) {
			return nil
		}
		deadline, err := recipientCooldown(tx, delivery.Channel, delivery.Destination)
		if err != nil {
			return err
		}
		if deadline.After(now) {
			return nil
		}
		delivery.Attempts++
		delivery.AttemptID = fmt.Sprintf("%s:%020d", id, delivery.Attempts)
		delivery.EffectiveNextAttempt = nil
		attempt := Attempt{ID: delivery.AttemptID, DeliveryID: id, Number: delivery.Attempts, StartedAt: now.UTC(), Outcome: "in_progress", Notification: delivery.Notification}
		if err := putJSON(tx.Bucket([]byte("attempts")), attempt.ID, attempt); err != nil {
			return err
		}
		if err := putJSON(outbox, id, delivery); err != nil {
			return err
		}
		claimed = true
		return nil
	})
	return delivery, claimed, err
}

// CompleteAttempt acknowledges the exact claimed payload, even when a newer
// source revision canceled that queue entry during the request.
func (s *Store) CompleteAttempt(attemptID string, result model.DeliveryResult, policy Policy, now time.Time) error {
	if policy.Match == nil {
		policy = s.currentPolicy()
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		attempts, outbox := tx.Bucket([]byte("attempts")), tx.Bucket([]byte("outbox"))
		var attempt Attempt
		found, err := getJSON(attempts, attemptID, &attempt)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("attempt does not exist")
		}
		if attempt.Outcome != "in_progress" {
			return nil
		}
		var delivery Delivery
		found, err = getJSON(outbox, attempt.DeliveryID, &delivery)
		if err != nil {
			return err
		}
		if !found || delivery.AttemptID != attemptID {
			return errors.New("attempt no longer owns delivery")
		}
		attempt.FinishedAt = timePointer(now)
		attempt.StatusCode = result.StatusCode
		attempt.Error = result.Error
		attempt.DurationMS = result.Duration.Milliseconds()
		attempt.Retryable = result.Retryable
		attempt.RetryAfterSeconds = result.RetryAfter.Seconds()
		attempt.Outcome = "failed"
		delivery.AttemptID = ""
		if !result.Success && (result.RetryAfter > 0 || result.StatusCode == 429 || result.RateLimited) {
			delay := max(RetryDelay(delivery.Attempts), result.RetryAfter)
			if err := extendRecipientCooldown(tx, delivery.Channel, delivery.Destination, now.UTC().Add(delay)); err != nil {
				return err
			}
		}
		if result.Success {
			attempt.Outcome = "delivered"
			attempt.Error = ""
			delivery.Status = "delivered"
			delivery.DeliveredAt = timePointer(now)
			delivery.LastError = ""
			// Keep cancellation metadata as evidence of the race, while the
			// acknowledged payload and delivery status describe what was sent.
			delivery.Notification = attempt.Notification
		} else if delivery.Status == "pending" {
			delivery.LastError = result.Error
			if result.Retryable {
				delay := max(RetryDelay(delivery.Attempts), result.RetryAfter)
				delivery.NextAttempt = now.UTC().Add(delay)
				attempt.NextAttempt = timePointer(delivery.NextAttempt)
				attempt.Outcome = "retry_scheduled"
			} else {
				delivery.Status = "failed"
			}
		}
		if err := putJSON(outbox, attempt.DeliveryID, delivery); err != nil {
			return err
		}
		if err := putJSON(attempts, attempt.ID, attempt); err != nil {
			return err
		}
		events := tx.Bucket([]byte("events"))
		key := eventKey(attempt.Notification.Event.Provider.Slug, attempt.Notification.Event.ID)
		var record EventRecord
		found, err = getJSON(events, key, &record)
		if err != nil || !found {
			return err
		}
		normalizeRecord(&record)
		if result.Success {
			record.Acknowledged[delivery.Channel] = Acknowledgment{Destination: delivery.Destination, NotificationID: attempt.Notification.ID, Event: attempt.Notification.Event, At: now.UTC()}
		}
		if policy.Match != nil && (result.Success || delivery.Status == "canceled") {
			if _, err := syncDeliveries(tx, &record, policy, now); err != nil {
				return err
			}
		}
		return putJSON(events, key, record)
	})
}

// Complete is retained for callers which report an already-finished attempt.
// Runtime workers use BeginAttempt/CompleteAttempt so the sent revision is exact.
func (s *Store) Complete(id string, result model.DeliveryResult, now time.Time) error {
	delivery, found, err := s.Delivery(id)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("delivery does not exist")
	}
	if delivery.AttemptID != "" {
		return s.CompleteAttempt(delivery.AttemptID, result, s.currentPolicy(), now)
	}
	if delivery.Status != "pending" {
		if !result.Success && (result.RetryAfter > 0 || result.StatusCode == 429 || result.RateLimited) {
			return s.db.Update(func(tx *bolt.Tx) error {
				return extendRecipientCooldown(tx, delivery.Channel, delivery.Destination, now.UTC().Add(max(RetryDelay(delivery.Attempts+1), result.RetryAfter)))
			})
		}
		return nil
	}
	// Legacy callers have already done I/O, so do not lose its result merely
	// because a cooldown or a previously scheduled deadline is still active.
	err = s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("outbox"))
		if _, err := getJSON(b, id, &delivery); err != nil {
			return err
		}
		delivery.Attempts++
		delivery.AttemptID = fmt.Sprintf("%s:%020d", id, delivery.Attempts)
		attempt := Attempt{ID: delivery.AttemptID, DeliveryID: id, Number: delivery.Attempts, StartedAt: now.UTC().Add(-result.Duration), Outcome: "in_progress", Notification: delivery.Notification}
		if err := putJSON(tx.Bucket([]byte("attempts")), attempt.ID, attempt); err != nil {
			return err
		}
		return putJSON(b, id, delivery)
	})
	if err != nil {
		return err
	}
	return s.CompleteAttempt(delivery.AttemptID, result, s.currentPolicy(), now)
}

func RetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 8 {
		return 30 * time.Minute
	}
	return min(10*time.Second*time.Duration(1<<uint(attempt-1)), 30*time.Minute)
}

var (
	ErrInFlight           = errors.New("delivery attempt is in flight")
	ErrSuperseded         = errors.New("delivery was superseded by a newer announcement")
	ErrAlreadyPending     = errors.New("delivery is already pending")
	ErrNotRetryable       = errors.New("only failed deliveries can be retried")
	ErrDeliveryMissing    = errors.New("delivery does not exist")
	ErrDeliveryIneligible = errors.New("current configuration no longer permits delivery")
)

func retryDelivery(tx *bolt.Tx, id string, policy Policy, now time.Time) (Delivery, error) {
	outbox := tx.Bucket([]byte("outbox"))
	var delivery Delivery
	found, err := getJSON(outbox, id, &delivery)
	if err != nil {
		return delivery, err
	}
	if !found {
		return delivery, ErrDeliveryMissing
	}
	if delivery.AttemptID != "" {
		return delivery, ErrInFlight
	}
	if delivery.CancellationReason == "superseded" {
		return delivery, ErrSuperseded
	}
	if delivery.Status == "pending" {
		return delivery, ErrAlreadyPending
	}
	if delivery.Status != "failed" || delivery.AttemptID != "" {
		return delivery, ErrNotRetryable
	}
	if deliveryDisallowed(delivery, policy) != "" {
		return delivery, ErrDeliveryIneligible
	}
	delivery.Status = "pending"
	delivery.NextAttempt = now.UTC()
	// Preserve LastError and the attempt journal until the retry completes.
	if err := putJSON(outbox, id, delivery); err != nil {
		return delivery, err
	}
	return effectiveDelivery(tx, delivery)
}

func (s *Store) RetryDelivery(id string, policy Policy, now time.Time) (Delivery, error) {
	var delivery Delivery
	err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		delivery, err = retryDelivery(tx, id, policy, now)
		return err
	})
	return delivery, err
}

func (s *Store) RetryFailedWithPolicy(policy Policy, now time.Time) (int, error) {
	count := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		return forEachSnapshot(tx.Bucket([]byte("outbox")), func(k, v []byte) error {
			var delivery Delivery
			if err := json.Unmarshal(v, &delivery); err != nil {
				return err
			}
			if delivery.Status != "failed" || deliveryDisallowed(delivery, policy) != "" {
				return nil
			}
			if _, err := retryDelivery(tx, string(k), policy, now); err != nil {
				return err
			}
			count++
			return nil
		})
	})
	return count, err
}

func (s *Store) RetryFailed(now time.Time) (int, error) {
	return s.RetryFailedWithPolicy(s.currentPolicy(), now)
}
