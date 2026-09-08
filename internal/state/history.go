package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
	bolt "go.etcd.io/bbolt"
)

type Matcher func(model.Event) (bool, string)

// Policy contains recipient fingerprints, never webhook URLs or credentials.
type Policy struct {
	Providers []string
	Channels  map[string]string
	Match     Matcher
}

type providerRecord struct {
	Active         bool       `json:"active"`
	Ready          bool       `json:"ready"`
	LastSuccess    *time.Time `json:"last_success,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	RetryNotBefore *time.Time `json:"retry_not_before,omitempty"`
}

type EventRecord struct {
	Event           model.Event       `json:"event"`
	Baseline        bool              `json:"baseline"`
	DetectedAt      time.Time         `json:"detected_at"`
	AllowedChannels map[string]string `json:"allowed_channels"`
	DeliveryIDs     map[string]string `json:"delivery_ids"`
}

type Detection struct {
	EventID  string
	Revision int
	Queued   int
	Reason   string
}

func eventKey(provider, id string) string { return provider + "\x00" + id }

// Reconcile runs before workers start. It cancels obsolete deliveries and
// prevents enabling a channel or changing recipient from replaying old events.
func (s *Store) Reconcile(policy Policy) error {
	if policy.Match == nil {
		return errors.New("event matcher is required")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		meta, providers, events, outbox := tx.Bucket([]byte("meta")), tx.Bucket([]byte("providers")), tx.Bucket([]byte("events")), tx.Bucket([]byte("outbox"))
		var previous map[string]string
		if _, err := getJSON(meta, "channels", &previous); err != nil {
			return err
		}
		active := map[string]bool{}
		for _, slug := range policy.Providers {
			active[slug] = true
		}
		if err := forEachSnapshot(providers, func(k, v []byte) error {
			var p providerRecord
			if err := json.Unmarshal(v, &p); err != nil {
				return err
			}
			if !active[string(k)] {
				p.Active = false
				p.Ready = false
			}
			return putJSON(providers, string(k), p)
		}); err != nil {
			return err
		}
		for slug := range active {
			var p providerRecord
			if _, err := getJSON(providers, slug, &p); err != nil {
				return err
			}
			if !p.Active {
				p.Ready = false
			}
			p.Active = true
			if err := putJSON(providers, slug, p); err != nil {
				return err
			}
		}
		if err := forEachSnapshot(events, func(k, v []byte) error {
			var record EventRecord
			if err := json.Unmarshal(v, &record); err != nil {
				return err
			}
			for channel, fingerprint := range record.AllowedChannels {
				if !active[record.Event.Provider.Slug] || policy.Channels[channel] == "" || policy.Channels[channel] != fingerprint || previous[channel] != fingerprint {
					delete(record.AllowedChannels, channel)
				}
			}
			return putJSON(events, string(k), record)
		}); err != nil {
			return err
		}
		if err := forEachSnapshot(outbox, func(k, v []byte) error {
			var delivery Delivery
			if err := json.Unmarshal(v, &delivery); err != nil {
				return err
			}
			if delivery.Status != "pending" && delivery.Status != "failed" {
				return nil
			}
			matches, _ := policy.Match(delivery.Notification.Event)
			if !matches || !active[delivery.Notification.Event.Provider.Slug] || policy.Channels[delivery.Channel] != delivery.Destination {
				delivery.Status = "canceled"
				delivery.LastError = "configuration no longer permits delivery"
				return putJSON(outbox, string(k), delivery)
			}
			return nil
		}); err != nil {
			return err
		}
		return putJSON(meta, "channels", policy.Channels)
	})
}

// CommitScan must be called only after a complete, validated provider scan.
// Event revisions and queue entries are committed in the same transaction.
func (s *Store) CommitScan(slug string, incoming []model.Event, policy Policy, now time.Time) ([]Detection, error) {
	var detections []Detection
	err := s.db.Update(func(tx *bolt.Tx) error {
		providers, events, outbox := tx.Bucket([]byte("providers")), tx.Bucket([]byte("events")), tx.Bucket([]byte("outbox"))
		var provider providerRecord
		if _, err := getJSON(providers, slug, &provider); err != nil {
			return err
		}
		if !provider.Active {
			return errors.New("provider is not active")
		}
		initial := !provider.Ready
		seen := map[string]bool{}
		for _, event := range incoming {
			if event.ID == "" || event.Provider.Slug != slug || event.Revision < 1 {
				return errors.New("provider scan contains invalid event identity")
			}
			key := eventKey(slug, event.ID)
			if seen[key] {
				return errors.New("provider scan contains duplicate event IDs")
			}
			seen[key] = true
			var record EventRecord
			exists, err := getJSON(events, key, &record)
			if err != nil {
				return err
			}
			if exists && event.Revision < record.Event.Revision {
				if !initial {
					detections = append(detections, Detection{EventID: event.ID, Revision: event.Revision, Reason: "stale_revision"})
					continue
				}
				// Reenabling still establishes a baseline from the newest known
				// payload instead of overwriting it with an outdated replica.
				event = record.Event
			}
			if exists && !initial && sameEvent(record.Event, event) {
				continue
			}
			if !exists {
				record = EventRecord{Baseline: initial, DetectedAt: now.UTC(), AllowedChannels: map[string]string{}, DeliveryIDs: map[string]string{}}
				if !initial {
					for channel, recipient := range policy.Channels {
						record.AllowedChannels[channel] = recipient
					}
				}
			}
			if initial {
				record.Baseline = true
				record.AllowedChannels = map[string]string{}
			}
			if record.DeliveryIDs == nil {
				record.DeliveryIDs = map[string]string{}
			}
			record.Event = event
			matched, reason := policy.Match(event)
			detection := Detection{EventID: event.ID, Revision: event.Revision, Reason: reason}
			for channel, id := range record.DeliveryIDs {
				var delivery Delivery
				found, err := getJSON(outbox, id, &delivery)
				if err != nil {
					return err
				}
				if !found {
					return fmt.Errorf("event references missing delivery for %s", channel)
				}
				if delivery.Status != "pending" && delivery.Status != "failed" {
					continue
				}
				delivery.Notification.Event = event
				if !matched || record.Baseline || record.AllowedChannels[channel] != policy.Channels[channel] || policy.Channels[channel] == "" {
					delivery.Status = "canceled"
					delivery.LastError = "event no longer permits delivery"
				}
				if err := putJSON(outbox, id, delivery); err != nil {
					return err
				}
			}
			if !record.Baseline && matched {
				for channel, destination := range record.AllowedChannels {
					if destination == "" || policy.Channels[channel] != destination || record.DeliveryIDs[channel] != "" {
						continue
					}
					sum := sha256.Sum256([]byte(key + "\x00" + channel + "\x00" + destination))
					id := hex.EncodeToString(sum[:])
					delivery := Delivery{Channel: channel, Destination: destination, Status: "pending", NextAttempt: now.UTC(), Notification: model.Notification{SchemaVersion: 1, ID: id, DetectedAt: record.DetectedAt, Event: event}}
					if err := putJSON(outbox, id, delivery); err != nil {
						return err
					}
					record.DeliveryIDs[channel] = id
					detection.Queued++
				}
			}
			if err := putJSON(events, key, record); err != nil {
				return err
			}
			if !initial {
				detections = append(detections, detection)
			}
		}
		provider.Ready = true
		provider.LastSuccess = timePointer(now)
		provider.LastError = ""
		provider.RetryNotBefore = nil
		return putJSON(providers, slug, provider)
	})
	if err != nil {
		return nil, err
	}
	return detections, nil
}

func sameEvent(a, b model.Event) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

func timePointer(t time.Time) *time.Time { utc := t.UTC(); return &utc }

func (s *Store) MarkProviderError(slug, message string, retryNotBefore time.Time) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("providers"))
		var p providerRecord
		if _, err := getJSON(b, slug, &p); err != nil {
			return err
		}
		p.LastError = message
		p.RetryNotBefore = nil
		if !retryNotBefore.IsZero() {
			p.RetryNotBefore = timePointer(retryNotBefore)
		}
		return putJSON(b, slug, p)
	})
}

// ProviderPollState exposes the persisted upstream cooldown and recovery state
// without exposing event history or requiring an HTTP call.
func (s *Store) ProviderPollState(slug string) (notBefore time.Time, recovering bool, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		var p providerRecord
		if _, err := getJSON(tx.Bucket([]byte("providers")), slug, &p); err != nil {
			return err
		}
		if p.RetryNotBefore != nil {
			notBefore = *p.RetryNotBefore
		}
		recovering = p.LastError != ""
		return nil
	})
	return
}

// MissingPending returns disappeared records that still have queued deliveries.
// A missing listing entry alone does not cancel anything; the caller must verify
// an explicit retraction through the event detail endpoint.
func (s *Store) MissingPending(slug string, incoming []model.Event) ([]model.Event, error) {
	seen := map[string]bool{}
	for _, event := range incoming {
		seen[event.ID] = true
	}
	var missing []model.Event
	err := s.db.View(func(tx *bolt.Tx) error {
		found := map[string]bool{}
		return tx.Bucket([]byte("outbox")).ForEach(func(_, v []byte) error {
			var d Delivery
			if err := json.Unmarshal(v, &d); err != nil {
				return err
			}
			e := d.Notification.Event
			if e.Provider.Slug == slug && !seen[e.ID] && !found[e.ID] && (d.Status == "pending" || d.Status == "failed") {
				found[e.ID] = true
				missing = append(missing, e)
			}
			return nil
		})
	})
	return missing, err
}

func (s *Store) Status(running bool, now time.Time) (model.Status, error) {
	status := model.Status{Running: running, UpdatedAt: now.UTC(), Providers: map[string]model.ProviderStatus{}, Channels: map[string]model.ChannelStatus{}}
	runtime, err := s.runtimeStatus()
	if err != nil {
		return status, err
	}
	status.Runtime = runtime
	err = s.db.View(func(tx *bolt.Tx) error {
		var recipients map[string]string
		if _, err := getJSON(tx.Bucket([]byte("meta")), "channels", &recipients); err != nil {
			return err
		}
		for channel, destination := range recipients {
			info := model.ChannelStatus{}
			deadline, err := recipientCooldown(tx, channel, destination)
			if err != nil {
				return err
			}
			if deadline.After(now) {
				info.CooldownUntil = timePointer(deadline)
			}
			status.Channels[channel] = info
		}
		if err := tx.Bucket([]byte("providers")).ForEach(func(k, v []byte) error {
			var p providerRecord
			if err := json.Unmarshal(v, &p); err != nil {
				return err
			}
			if p.Active {
				status.Providers[string(k)] = model.ProviderStatus{Ready: p.Ready, LastSuccess: p.LastSuccess, LastError: p.LastError}
			}
			return nil
		}); err != nil {
			return err
		}
		return tx.Bucket([]byte("outbox")).ForEach(func(_, v []byte) error {
			var d Delivery
			if err := json.Unmarshal(v, &d); err != nil {
				return err
			}
			channel := status.Channels[d.Channel]
			provider, active := status.Providers[d.Notification.Event.Provider.Slug]
			switch d.Status {
			case "pending":
				status.Pending++
				channel.Pending++
				provider.Pending++
				detected := d.Notification.DetectedAt
				if !detected.IsZero() {
					if channel.OldestPending == nil || detected.Before(*channel.OldestPending) {
						channel.OldestPending = timePointer(detected)
					}
					if provider.OldestPending == nil || detected.Before(*provider.OldestPending) {
						provider.OldestPending = timePointer(detected)
					}
				}
			case "failed":
				status.Failed++
				channel.Failed++
				provider.Failed++
			case "delivered":
				channel.Delivered++
			case "canceled":
				channel.Canceled++
			}
			status.Channels[d.Channel] = channel
			if active {
				status.Providers[d.Notification.Event.Provider.Slug] = provider
			}
			return nil
		})
	})
	return status, err
}
