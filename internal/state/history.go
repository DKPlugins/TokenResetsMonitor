package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
	bolt "go.etcd.io/bbolt"
)

type Matcher func(model.Event) (bool, string)

// Policy contains recipient fingerprints and filter metadata, never credentials.
type Policy struct {
	Providers    []string
	Channels     map[string]string
	Changes      map[string]bool
	FilterConfig json.RawMessage
	Match        Matcher
}

type providerRecord struct {
	Active         bool       `json:"active"`
	Ready          bool       `json:"ready"`
	LastSuccess    *time.Time `json:"last_success,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	RetryNotBefore *time.Time `json:"retry_not_before,omitempty"`
}

type Decision struct {
	Matched      bool            `json:"matched"`
	Reason       string          `json:"reason"`
	ObservedAt   time.Time       `json:"observed_at"`
	FilterConfig json.RawMessage `json:"filter_config,omitempty"`
	Unavailable  bool            `json:"unavailable,omitempty"`
}

type Acknowledgment struct {
	LegacyUnverified bool        `json:"legacy_unverified,omitempty"`
	Destination      string      `json:"destination"`
	NotificationID   string      `json:"notification_id"`
	Event            model.Event `json:"event"`
	At               time.Time   `json:"at"`
}

type EventRecord struct {
	NotificationStatus   string                    `json:"notification_status,omitempty"`
	ChannelStatuses      map[string]string         `json:"channel_statuses,omitempty"`
	NeedsDeliverySync    bool                      `json:"needs_delivery_sync,omitempty"`
	DiscoveryChannels    map[string]string         `json:"discovery_channels,omitempty"`
	DiscoveryUnavailable bool                      `json:"discovery_unavailable,omitempty"`
	ChannelSuppressions  map[string]string         `json:"channel_suppressions,omitempty"`
	Event                model.Event               `json:"event"`
	Baseline             bool                      `json:"baseline"`
	DetectedAt           time.Time                 `json:"detected_at"`
	ObservedAt           time.Time                 `json:"observed_at"`
	Decision             Decision                  `json:"decision"`
	AllowedChannels      map[string]string         `json:"allowed_channels"`
	DeliveryIDs          map[string]string         `json:"delivery_ids"`
	Acknowledged         map[string]Acknowledgment `json:"acknowledged,omitempty"`
	HistoryPrunedBefore  *time.Time                `json:"history_pruned_before,omitempty"`
}

type RevisionRecord struct {
	Sequence   uint64      `json:"sequence"`
	Event      model.Event `json:"event"`
	Decision   Decision    `json:"decision"`
	ObservedAt time.Time   `json:"observed_at"`
}

type Detection struct {
	EventID  string
	Revision int
	Queued   int
	Reason   string
}

func eventKey(provider, id string) string { return provider + "\x00" + id }

func normalizeRecord(record *EventRecord) {
	if record.ChannelSuppressions == nil {
		record.ChannelSuppressions = map[string]string{}
	}
	if record.AllowedChannels == nil {
		record.AllowedChannels = map[string]string{}
	}
	if record.DeliveryIDs == nil {
		record.DeliveryIDs = map[string]string{}
	}
	if record.Acknowledged == nil {
		record.Acknowledged = map[string]Acknowledgment{}
	}
}

func (s *Store) Reconcile(policy Policy) error {
	if policy.Match == nil {
		return errors.New("event matcher is required")
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
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
			normalizeRecord(&record)
			for channel, fingerprint := range record.AllowedChannels {
				if !active[record.Event.Provider.Slug] || policy.Channels[channel] == "" || policy.Channels[channel] != fingerprint || previous[channel] != fingerprint {
					reason := "provider_not_selected"
					if policy.Channels[channel] == "" {
						reason = "channel_disabled"
					} else if policy.Channels[channel] != fingerprint {
						reason = "recipient_changed"
					}
					record.ChannelSuppressions[channel] = reason
					delete(record.AllowedChannels, channel)
				}
			}
			if record.NeedsDeliverySync {
				if _, err := syncDeliveries(tx, &record, policy, time.Now()); err != nil {
					return err
				}
				record.NeedsDeliverySync = false
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
			reason := deliveryDisallowed(delivery, policy)
			if reason == "" {
				return nil
			}
			cancelDelivery(&delivery, reason, time.Now())
			return putJSON(outbox, string(k), delivery)
		}); err != nil {
			return err
		}
		if err := putJSON(meta, "channels", policy.Channels); err != nil {
			return err
		}
		return putJSON(meta, "changes", policy.Changes)
	})
	if err == nil {
		s.setPolicy(policy)
	}
	return err
}

func deliveryDisallowed(delivery Delivery, policy Policy) string {
	if policy.Channels[delivery.Channel] == "" {
		return "channel_disabled"
	}
	if policy.Channels[delivery.Channel] != delivery.Destination {
		return "recipient_changed"
	}
	if delivery.Notification.Kind != "" {
		if !policy.Changes[delivery.Channel] {
			return "changes_disabled"
		}
		return ""
	}
	if policy.Match == nil {
		return "filter_unavailable"
	}
	matched, reason := policy.Match(delivery.Notification.Event)
	if !matched {
		return reason
	}
	for _, provider := range policy.Providers {
		if provider == delivery.Notification.Event.Provider.Slug {
			return ""
		}
	}
	return "provider_not_selected"
}

func cancelDelivery(delivery *Delivery, reason string, now time.Time) {
	delivery.Status = "canceled"
	delivery.CancellationReason = reason
	delivery.CanceledAt = timePointer(now)
	// Preserve the last network failure separately from cancellation evidence.
}

func (s *Store) CommitScan(slug string, incoming []model.Event, policy Policy, now time.Time) ([]Detection, error) {
	return s.commitEvents(slug, incoming, policy, now, false)
}

// CommitDetails records verified details without initializing or refreshing a provider baseline.
func (s *Store) CommitDetails(slug string, incoming []model.Event, policy Policy, now time.Time) ([]Detection, error) {
	return s.commitEvents(slug, incoming, policy, now, true)
}

func (s *Store) commitEvents(slug string, incoming []model.Event, policy Policy, now time.Time, details bool) ([]Detection, error) {
	if policy.Match == nil {
		return nil, errors.New("event matcher is required")
	}
	var detections []Detection
	err := s.db.Update(func(tx *bolt.Tx) error {
		providers, events := tx.Bucket([]byte("providers")), tx.Bucket([]byte("events"))
		var provider providerRecord
		if _, err := getJSON(providers, slug, &provider); err != nil {
			return err
		}
		if !details && !provider.Active {
			return errors.New("provider is not active")
		}
		initial := !details && !provider.Ready
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
			if details && !exists {
				continue
			}
			if exists && event.Revision < record.Event.Revision {
				if !initial {
					detections = append(detections, Detection{EventID: event.ID, Revision: event.Revision, Reason: "stale_revision"})
					continue
				}
				event = record.Event
			}
			if exists && !initial && sameEvent(record.Event, event) {
				continue
			}
			if !exists {
				record = EventRecord{Baseline: initial, DetectedAt: now.UTC(), DiscoveryChannels: map[string]string{}}
				for channel, recipient := range policy.Channels {
					record.DiscoveryChannels[channel] = recipient
				}
				normalizeRecord(&record)
				if !initial {
					for channel, recipient := range policy.Channels {
						record.AllowedChannels[channel] = recipient
					}
				}
			}
			normalizeRecord(&record)
			if initial {
				record.Baseline = true
				record.AllowedChannels = map[string]string{}
			}
			record.Event = event
			record.ObservedAt = now.UTC()
			matched, reason := policy.Match(event)
			if record.Baseline {
				reason = "initial_history"
			}
			record.Decision = Decision{Matched: matched, Reason: reason, ObservedAt: now.UTC(), FilterConfig: append(json.RawMessage(nil), policy.FilterConfig...)}
			queued, err := syncDeliveries(tx, &record, policy, now)
			if err != nil {
				return err
			}
			if err := appendRevision(tx, record); err != nil {
				return err
			}
			if err := putJSON(events, key, record); err != nil {
				return err
			}
			if !initial {
				detections = append(detections, Detection{event.ID, event.Revision, queued, reason})
			}
		}
		if details {
			return nil
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

func appendRevision(tx *bolt.Tx, record EventRecord) error {
	bucket := tx.Bucket([]byte("revisions"))
	seq, err := bucket.NextSequence()
	if err != nil {
		return err
	}
	revision := RevisionRecord{seq, record.Event, record.Decision, record.ObservedAt}
	key := fmt.Sprintf("%s\x00%020d", eventKey(record.Event.Provider.Slug, record.Event.ID), seq)
	return putJSON(bucket, key, revision)
}

func syncDeliveries(tx *bolt.Tx, record *EventRecord, policy Policy, now time.Time) (int, error) {
	normalizeRecord(record)
	channels := map[string]bool{}
	for channel := range policy.Channels {
		channels[channel] = true
	}
	for channel := range record.DeliveryIDs {
		channels[channel] = true
	}
	queued := 0
	for channel := range channels {
		count, err := syncChannel(tx, record, channel, policy, now)
		if err != nil {
			return 0, err
		}
		queued += count
	}
	return queued, nil
}

func syncChannel(tx *bolt.Tx, record *EventRecord, channel string, policy Policy, now time.Time) (int, error) {
	outbox := tx.Bucket([]byte("outbox"))
	var current Delivery
	previousID := record.DeliveryIDs[channel]
	if previousID != "" {
		found, err := getJSON(outbox, previousID, &current)
		if err != nil {
			return 0, err
		}
		if !found {
			return 0, fmt.Errorf("event references missing delivery for %s", channel)
		}
	}
	destination := policy.Channels[channel]
	ack, acknowledged := record.Acknowledged[channel]
	notification := model.Notification{SchemaVersion: 1, DetectedAt: record.DetectedAt, Event: record.Event}
	reason := ""
	if acknowledged {
		switch {
		case destination == "":
			reason = "channel_disabled"
		case destination != ack.Destination:
			reason = "recipient_changed"
		case !policy.Changes[channel]:
			reason = "changes_disabled"
		default:
			changes := model.MeaningfulChanges(ack.Event, record.Event)
			if len(changes) == 0 {
				reason = "no_meaningful_change"
				break
			}
			notification.Kind = "correction"
			if record.Event.Status == "retracted" && ack.Event.Status != "retracted" {
				notification.Kind = "retraction"
			}
			notification.SchemaVersion = 1
			previous := ack.Event
			notification.PreviousEvent = &previous
			notification.PreviousEventUnverified = ack.LegacyUnverified
			notification.Changes = changes
			notification.RelatedNotificationID = ack.NotificationID
			notification.DetectedAt = record.ObservedAt
		}
	} else {
		matched, matchReason := policy.Match(record.Event)
		switch {
		case record.Baseline:
			reason = "initial_history"
		case destination == "":
			reason = "channel_disabled"
		case record.AllowedChannels[channel] != destination:
			reason = "recipient_not_eligible_at_discovery"
		case !matched:
			reason = matchReason
		}
	}
	wasFailed, previousFailure := current.Status == "failed", current.LastError
	active := current.Status == "pending" || current.Status == "failed"
	if reason != "" {
		if active {
			cancelDelivery(&current, reason, now)
			if err := putJSON(outbox, previousID, current); err != nil {
				return 0, err
			}
		}
		return 0, nil
	}
	if current.AttemptID != "" {
		// The sender owns an immutable claim. Its outcome determines whether a
		// replacement is an initial announcement or a correction.
		if active && !equivalentNotification(current.Notification, notification) {
			cancelDelivery(&current, "superseded", now)
			if err := putJSON(outbox, previousID, current); err != nil {
				return 0, err
			}
		}
		return 0, nil
	}
	if active && current.Destination == destination && equivalentNotification(current.Notification, notification) {
		return 0, nil
	}
	if active {
		cancelDelivery(&current, "superseded", now)
		if err := putJSON(outbox, previousID, current); err != nil {
			return 0, err
		}
	}
	key := eventKey(record.Event.Provider.Slug, record.Event.ID)
	identity := key + "\x00" + channel + "\x00" + destination
	if previousID != "" || notification.Kind != "" {
		raw, _ := json.Marshal(notification)
		identity += "\x00" + previousID + "\x00" + string(raw)
	}
	sum := sha256.Sum256([]byte(identity))
	notification.ID = hex.EncodeToString(sum[:])
	delivery := Delivery{Channel: channel, Destination: destination, Status: "pending", Notification: notification, NextAttempt: now.UTC(), CreatedAt: now.UTC()}
	if wasFailed && notification.Kind == "" {
		delivery.Status = "failed"
		delivery.LastError = previousFailure
	}
	if err := putJSON(outbox, notification.ID, delivery); err != nil {
		return 0, err
	}
	record.DeliveryIDs[channel] = notification.ID
	return 1, nil
}

func equivalentNotification(a, b model.Notification) bool {
	if a.Kind != b.Kind || a.RelatedNotificationID != b.RelatedNotificationID {
		return false
	}
	if a.Kind == "" {
		return sameEvent(a.Event, b.Event)
	}
	return len(model.MeaningfulChanges(a.Event, b.Event)) == 0
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

func (s *Store) MissingPending(slug string, incoming []model.Event) ([]model.Event, error) {
	return s.MissingTracked(slug, incoming)
}

// MissingTracked includes pending work and announcements acknowledged by a
// currently enabled correction recipient. A missing list item is never withdrawal.
func (s *Store) MissingTracked(slug string, incoming []model.Event) ([]model.Event, error) {
	seen := map[string]bool{}
	for _, event := range incoming {
		seen[event.ID] = true
	}
	var missing []model.Event
	err := s.db.View(func(tx *bolt.Tx) error {
		var channels map[string]string
		var changes map[string]bool
		var cursor string
		meta := tx.Bucket([]byte("meta"))
		if _, err := getJSON(meta, "channels", &channels); err != nil {
			return err
		}
		if _, err := getJSON(meta, "changes", &changes); err != nil {
			return err
		}
		if _, err := getJSON(meta, "detail_cursor:"+slug, &cursor); err != nil {
			return err
		}
		if err := tx.Bucket([]byte("events")).ForEach(func(_, v []byte) error {
			var record EventRecord
			if err := json.Unmarshal(v, &record); err != nil {
				return err
			}
			if record.Event.Provider.Slug != slug || seen[record.Event.ID] {
				return nil
			}
			tracked := false
			for channel, ack := range record.Acknowledged {
				if changes[channel] && channels[channel] != "" && channels[channel] == ack.Destination && ack.Event.Status != "retracted" {
					tracked = true
				}
			}
			for _, id := range record.DeliveryIDs {
				var delivery Delivery
				if _, err := getJSON(tx.Bucket([]byte("outbox")), id, &delivery); err != nil {
					return err
				}
				if delivery.Status == "pending" || delivery.Status == "failed" || delivery.AttemptID != "" {
					tracked = true
				}
			}
			if tracked {
				missing = append(missing, record.Event)
			}
			return nil
		}); err != nil {
			return err
		}
		sort.Slice(missing, func(i, j int) bool { return missing[i].ID < missing[j].ID })
		split := sort.Search(len(missing), func(i int) bool { return missing[i].ID > cursor })
		if split < len(missing) {
			missing = append(append([]model.Event(nil), missing[split:]...), missing[:split]...)
		}
		return nil
	})
	return missing, err
}

func (s *Store) AdvanceDetailCursor(slug, id string) error {
	return s.db.Update(func(tx *bolt.Tx) error { return putJSON(tx.Bucket([]byte("meta")), "detail_cursor:"+slug, id) })
}

func (s *Store) TrackedProviders(policy Policy) ([]string, error) {
	unique := map[string]bool{}
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("events")).ForEach(func(_, v []byte) error {
			var record EventRecord
			if err := json.Unmarshal(v, &record); err != nil {
				return err
			}
			for channel, ack := range record.Acknowledged {
				if policy.Changes[channel] && policy.Channels[channel] != "" && policy.Channels[channel] == ack.Destination && ack.Event.Status != "retracted" {
					unique[record.Event.Provider.Slug] = true
				}
			}
			return nil
		})
	})
	providers := make([]string, 0, len(unique))
	for provider := range unique {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	return providers, err
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
