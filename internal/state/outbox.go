package state

import (
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
	bolt "go.etcd.io/bbolt"
)

type Delivery struct {
	Channel      string             `json:"channel"`
	Destination  string             `json:"destination"`
	Status       string             `json:"status"`
	Notification model.Notification `json:"notification"`
	Attempts     int                `json:"attempts"`
	NextAttempt  time.Time          `json:"next_attempt"`
	LastError    string             `json:"last_error,omitempty"`
	DeliveredAt  *time.Time         `json:"delivered_at,omitempty"`
}

func (s *Store) Due(channel string, now time.Time, limit int) ([]Delivery, error) {
	var deliveries []Delivery
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("outbox")).ForEach(func(_, v []byte) error {
			var d Delivery
			if err := json.Unmarshal(v, &d); err != nil {
				return err
			}
			if d.Channel == channel && d.Status == "pending" && !d.NextAttempt.After(now) {
				deliveries = append(deliveries, d)
			}
			return nil
		})
	})
	sort.Slice(deliveries, func(i, j int) bool { return deliveries[i].NextAttempt.Before(deliveries[j].NextAttempt) })
	if limit > 0 && len(deliveries) > limit {
		deliveries = deliveries[:limit]
	}
	return deliveries, err
}

func (s *Store) Delivery(id string) (Delivery, bool, error) {
	var d Delivery
	var exists bool
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		exists, err = getJSON(tx.Bucket([]byte("outbox")), id, &d)
		return err
	})
	return d, exists, err
}

// Complete records an attempt only after network I/O has finished. A crash in
// between is retried with the same notification ID (at-least-once delivery).
func (s *Store) Complete(id string, result model.DeliveryResult, now time.Time) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("outbox"))
		var d Delivery
		found, err := getJSON(b, id, &d)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("delivery does not exist")
		}
		if d.Status != "pending" {
			return nil
		}
		d.Attempts++
		switch {
		case result.Success:
			d.Status = "delivered"
			d.DeliveredAt = timePointer(now)
			d.LastError = ""
		case result.Retryable:
			delay := RetryDelay(d.Attempts)
			if result.RetryAfter > delay {
				delay = result.RetryAfter
			}
			d.NextAttempt = now.UTC().Add(delay)
			d.LastError = result.Error
		default:
			d.Status = "failed"
			d.LastError = result.Error
		}
		return putJSON(b, id, d)
	})
}

// RetryDelay caps the monitor's exponential backoff at 30 minutes. An explicit
// server Retry-After may be longer and is never shortened.
func RetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 8 {
		return 30 * time.Minute
	}
	delay := 10 * time.Second * time.Duration(1<<uint(attempt-1))
	if delay > 30*time.Minute {
		return 30 * time.Minute
	}
	return delay
}

func (s *Store) RetryFailed(now time.Time) (int, error) {
	count := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("outbox"))
		return forEachSnapshot(b, func(k, v []byte) error {
			var d Delivery
			if err := json.Unmarshal(v, &d); err != nil {
				return err
			}
			if d.Status != "failed" {
				return nil
			}
			d.Status = "pending"
			d.NextAttempt = now.UTC()
			d.LastError = ""
			if err := putJSON(b, string(k), d); err != nil {
				return err
			}
			count++
			return nil
		})
	})
	return count, err
}
