package state

import (
	"encoding/json"
	"errors"
	"os"
	"time"

	bolt "go.etcd.io/bbolt"
)

// OpenReadOnly never creates, migrates, backs up or recovers a database.
func OpenReadOnly(path string) (*Store, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return nil, errors.New("state database is empty or not a regular file")
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		return nil, openError(err)
	}
	version, err := schema(db)
	if err == nil && version > SchemaVersion {
		err = errors.New("state database schema is newer than this application")
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, path: path}, nil
}

func (s *Store) setPolicy(policy Policy) {
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	copy := policy
	copy.Providers = append([]string(nil), policy.Providers...)
	copy.FilterConfig = append(json.RawMessage(nil), policy.FilterConfig...)
	copy.Channels = map[string]string{}
	for channel, destination := range policy.Channels {
		copy.Channels[channel] = destination
	}
	copy.Changes = map[string]bool{}
	for channel, enabled := range policy.Changes {
		copy.Changes[channel] = enabled
	}
	s.policy = copy
}

func (s *Store) currentPolicy() Policy {
	s.policyMu.RLock()
	defer s.policyMu.RUnlock()
	return s.policy
}

func migrateHistory(tx *bolt.Tx, version int) error {
	if version >= 3 {
		return nil
	}
	events, outbox := tx.Bucket([]byte("events")), tx.Bucket([]byte("outbox"))
	if err := forEachSnapshot(events, func(k, v []byte) error {
		var record EventRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return err
		}
		normalizeRecord(&record)
		record.DiscoveryUnavailable = true
		record.Decision = Decision{Unavailable: true, Reason: "legacy_decision_unavailable"}
		if record.Baseline {
			record.Decision.Reason = "initial_history"
		}
		return putJSON(events, string(k), record)
	}); err != nil {
		return err
	}
	return forEachSnapshot(outbox, func(k, v []byte) error {
		var delivery Delivery
		if err := json.Unmarshal(v, &delivery); err != nil {
			return err
		}
		delivery.LegacyAttempts = delivery.Attempts
		delivery.CreatedAt = delivery.Notification.DetectedAt
		if delivery.Status == "canceled" {
			delivery.CancellationReason = "legacy_cancellation"
		}
		if err := putJSON(outbox, string(k), delivery); err != nil {
			return err
		}
		if delivery.Status != "delivered" {
			return nil
		}
		key := eventKey(delivery.Notification.Event.Provider.Slug, delivery.Notification.Event.ID)
		var record EventRecord
		found, err := getJSON(events, key, &record)
		if err != nil || !found {
			return err
		}
		normalizeRecord(&record)
		at := time.Time{}
		if delivery.DeliveredAt != nil {
			at = *delivery.DeliveredAt
		}
		prior, exists := record.Acknowledged[delivery.Channel]
		if !exists || !prior.At.After(at) {
			record.Acknowledged[delivery.Channel] = Acknowledgment{LegacyUnverified: true, Destination: delivery.Destination, NotificationID: delivery.Notification.ID, Event: delivery.Notification.Event, At: at}
		}
		return putJSON(events, key, record)
	})
}

func recoverAttempts(tx *bolt.Tx, now time.Time) error {
	attempts, outbox := tx.Bucket([]byte("attempts")), tx.Bucket([]byte("outbox"))
	return forEachSnapshot(attempts, func(k, v []byte) error {
		var attempt Attempt
		if err := json.Unmarshal(v, &attempt); err != nil {
			return err
		}
		if attempt.Outcome != "in_progress" {
			return nil
		}
		attempt.Outcome = "interrupted"
		attempt.FinishedAt = timePointer(now)
		attempt.Error = "process stopped before attempt completion was recorded"
		if err := putJSON(attempts, string(k), attempt); err != nil {
			return err
		}
		var delivery Delivery
		found, err := getJSON(outbox, attempt.DeliveryID, &delivery)
		if err != nil || !found {
			return err
		}
		if delivery.AttemptID != attempt.ID {
			return nil
		}
		delivery.AttemptID = ""
		if delivery.Status == "canceled" && delivery.CancellationReason == "superseded" {
			events := tx.Bucket([]byte("events"))
			key := eventKey(delivery.Notification.Event.Provider.Slug, delivery.Notification.Event.ID)
			var record EventRecord
			found, err := getJSON(events, key, &record)
			if err != nil {
				return err
			}
			if found {
				record.NeedsDeliverySync = true
				if err := putJSON(events, key, record); err != nil {
					return err
				}
			}
		}
		// Retry the same logical notification after an uncertain acknowledgement.
		// A source/configuration cancellation remains authoritative.
		if delivery.Status == "pending" {
			delivery.NextAttempt = now.UTC()
		}
		return putJSON(outbox, attempt.DeliveryID, delivery)
	})
}

// PruneHistory removes bounded batches of old details. The current event,
// recipient acknowledgement and every retry payload remain in their owner record.
func (s *Store) PruneHistory(now time.Time, retentionDays int, limit int) (int, error) {
	if retentionDays <= 0 {
		return 0, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	cutoff := now.UTC().AddDate(0, 0, -retentionDays)
	count := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		events, outbox := tx.Bucket([]byte("events")), tx.Bucket([]byte("outbox"))
		attempts := tx.Bucket([]byte("attempts"))
		selected := make([]Attempt, 0, limit)
		cursor := attempts.Cursor()
		for k, v := cursor.First(); k != nil && len(selected) < limit; k, v = cursor.Next() {
			var attempt Attempt
			if err := json.Unmarshal(v, &attempt); err != nil {
				return err
			}
			if attempt.FinishedAt == nil || !attempt.FinishedAt.Before(cutoff) || attempt.Outcome == "in_progress" {
				continue
			}
			var delivery Delivery
			found, err := getJSON(outbox, attempt.DeliveryID, &delivery)
			if err != nil {
				return err
			}
			if found && (delivery.AttemptID == attempt.ID || (delivery.Status == "pending" || delivery.Status == "failed") && delivery.Attempts == attempt.Number) {
				continue
			}
			selected = append(selected, attempt)
		}
		// Only the selected bounded batch is retained before mutating its bucket.
		for _, attempt := range selected {
			var delivery Delivery
			found, err := getJSON(outbox, attempt.DeliveryID, &delivery)
			if err != nil {
				return err
			}
			if found {
				delivery.HistoryPrunedBefore = laterBoundary(delivery.HistoryPrunedBefore, cutoff)
				if err := putJSON(outbox, attempt.DeliveryID, delivery); err != nil {
					return err
				}
			}
			if err := attempts.Delete([]byte(attempt.ID)); err != nil {
				return err
			}
			count++
		}
		if count == limit {
			return nil
		}
		revisions := tx.Bucket([]byte("revisions"))
		type expiredRevision struct {
			key      string
			revision RevisionRecord
		}
		expired := make([]expiredRevision, 0, limit-count)
		cursor = revisions.Cursor()
		for k, v := cursor.First(); k != nil && len(expired) < limit-count; k, v = cursor.Next() {
			var revision RevisionRecord
			if err := json.Unmarshal(v, &revision); err != nil {
				return err
			}
			if revision.ObservedAt.IsZero() || !revision.ObservedAt.Before(cutoff) {
				continue
			}
			var record EventRecord
			found, err := getJSON(events, eventKey(revision.Event.Provider.Slug, revision.Event.ID), &record)
			if err != nil {
				return err
			}
			protected := false
			if found {
				protected, err = revisionProtected(tx, revision, record)
				if err != nil {
					return err
				}
			}
			if !protected {
				expired = append(expired, expiredRevision{string(k), revision})
			}
		}
		for _, item := range expired {
			key := eventKey(item.revision.Event.Provider.Slug, item.revision.Event.ID)
			var record EventRecord
			found, err := getJSON(events, key, &record)
			if err != nil {
				return err
			}
			if found {
				record.HistoryPrunedBefore = laterBoundary(record.HistoryPrunedBefore, cutoff)
				if err := putJSON(events, key, record); err != nil {
					return err
				}
			}
			if err := revisions.Delete([]byte(item.key)); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	return count, err
}

func laterBoundary(previous *time.Time, next time.Time) *time.Time {
	if previous != nil && previous.After(next) {
		return previous
	}
	return timePointer(next)
}

func revisionProtected(tx *bolt.Tx, revision RevisionRecord, record EventRecord) (bool, error) {
	if sameEvent(record.Event, revision.Event) {
		return true, nil
	}
	for _, ack := range record.Acknowledged {
		if sameEvent(ack.Event, revision.Event) {
			return true, nil
		}
	}
	for _, id := range record.DeliveryIDs {
		var delivery Delivery
		found, err := getJSON(tx.Bucket([]byte("outbox")), id, &delivery)
		if err != nil {
			return false, err
		}
		if !found || delivery.Status != "pending" && delivery.Status != "failed" && delivery.AttemptID == "" {
			continue
		}
		if sameEvent(delivery.Notification.Event, revision.Event) || delivery.Notification.PreviousEvent != nil && sameEvent(*delivery.Notification.PreviousEvent, revision.Event) {
			return true, nil
		}
	}
	return false, nil
}
