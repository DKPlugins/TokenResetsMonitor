package state

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

type Query struct {
	SinceDuration string     `json:"since_duration,omitempty"`
	Provider      string     `json:"provider,omitempty"`
	EventID       string     `json:"event_id,omitempty"`
	Channel       string     `json:"channel,omitempty"`
	Status        string     `json:"status,omitempty"`
	Limit         int        `json:"limit,omitempty"`
	Cursor        string     `json:"cursor,omitempty"`
	Since         *time.Time `json:"since,omitempty"`
	Until         *time.Time `json:"until,omitempty"`
}

type EventPage struct {
	Items      []EventRecord `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
	AsOf       time.Time     `json:"as_of"`
}
type DeliveryPage struct {
	Items      []Delivery `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
	AsOf       time.Time  `json:"as_of"`
}
type EventView struct {
	Record             EventRecord      `json:"record"`
	Deliveries         []Delivery       `json:"deliveries"`
	Revisions          []RevisionRecord `json:"revisions"`
	NextCursor         string           `json:"next_cursor,omitempty"`
	DeliveryNextCursor string           `json:"delivery_next_cursor,omitempty"`
	AsOf               time.Time        `json:"as_of"`
}
type DeliveryView struct {
	Delivery
	AttemptHistory []Attempt `json:"attempt_history"`
	NextCursor     string    `json:"next_cursor,omitempty"`
	AsOf           time.Time `json:"as_of"`
}
type EventExplanation struct {
	Channels []ChannelExplanation `json:"channels"`
	EventView
	Reason         string `json:"reason"`
	CurrentMatched bool   `json:"current_matched"`
	CurrentReason  string `json:"current_reason"`
}
type pageCursor struct {
	Kind   string    `json:"kind"`
	Last   string    `json:"last"`
	AsOf   time.Time `json:"as_of"`
	Filter string    `json:"filter"`
}

func queryPage(q Query, kind string) (int, pageCursor, error) {
	if q.SinceDuration != "" {
		duration, err := time.ParseDuration(q.SinceDuration)
		if err != nil || duration <= 0 || q.Since != nil {
			return 0, pageCursor{}, errors.New("since duration must be positive and cannot accompany an absolute since")
		}
	}
	limit := q.Limit
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 200 {
		return 0, pageCursor{}, errors.New("limit must be between 1 and 200")
	}
	if q.Since != nil && q.Until != nil && q.Since.After(*q.Until) {
		return 0, pageCursor{}, errors.New("since must not be after until")
	}
	filterQuery := q
	filterQuery.Cursor = ""
	filterQuery.Limit = 0
	raw, _ := json.Marshal(filterQuery)
	sum := sha256.Sum256(raw)
	fingerprint := hex.EncodeToString(sum[:])
	cursor := pageCursor{Kind: kind, AsOf: time.Now().UTC(), Filter: fingerprint}
	if q.Cursor != "" {
		if len(q.Cursor) > 4096 {
			return 0, cursor, errors.New("invalid cursor")
		}
		data, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.Kind != kind || cursor.Filter != fingerprint || cursor.AsOf.IsZero() || cursor.Last == "" {
			return 0, cursor, errors.New("cursor does not match this query")
		}
	}
	if q.SinceDuration != "" && q.Until != nil {
		duration, _ := time.ParseDuration(q.SinceDuration)
		if cursor.AsOf.Add(-duration).After(*q.Until) {
			return 0, cursor, errors.New("since must not be after until")
		}
	}
	return limit, cursor, nil
}
func nextPage(cursor pageCursor, last string) string {
	cursor.Last = last
	data, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(data)
}
func inTimeRange(at time.Time, q Query, asOf time.Time) bool {
	if q.SinceDuration != "" {
		duration, _ := time.ParseDuration(q.SinceDuration)
		since := asOf.Add(-duration)
		q.Since = &since
	}
	return !at.After(asOf) && (q.Since == nil || !at.Before(*q.Since)) && (q.Until == nil || !at.After(*q.Until))
}
func eventOrder(record EventRecord) string {
	return record.DetectedAt.UTC().Format("2006-01-02T15:04:05.000000000Z") + "\x00" + eventKey(record.Event.Provider.Slug, record.Event.ID)
}
func deliveryOrder(delivery Delivery) string {
	return delivery.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z") + "\x00" + delivery.Notification.ID
}

func forEachValue(bucket *bolt.Bucket, fn func([]byte, []byte) error) error {
	if bucket == nil {
		return nil
	}
	return bucket.ForEach(fn)
}

func (s *Store) ListEvents(q Query) (EventPage, error) {
	limit, cursor, err := queryPage(q, "events")
	page := EventPage{Items: []EventRecord{}, AsOf: cursor.AsOf}
	if err != nil {
		return page, err
	}
	switch q.Status {
	case "", "baseline", "filtered", "pending", "failed", "delivered", "canceled", "no_eligible_channel", "legacy_unknown":
	default:
		return page, errors.New("unknown history status")
	}
	err = s.db.View(func(tx *bolt.Tx) error {
		var records []EventRecord
		if err := forEachValue(tx.Bucket([]byte("events")), func(_, v []byte) error {
			var record EventRecord
			if err := json.Unmarshal(v, &record); err != nil {
				return err
			}
			normalizeLegacyView(&record)
			if q.Provider != "" && record.Event.Provider.Slug != q.Provider || q.EventID != "" && record.Event.ID != q.EventID {
				return nil
			}
			if !inTimeRange(record.DetectedAt, q, cursor.AsOf) || cursor.Last != "" && eventOrder(record) >= cursor.Last {
				return nil
			}
			if q.Channel != "" && record.DeliveryIDs[q.Channel] == "" && record.DiscoveryChannels[q.Channel] == "" && record.AllowedChannels[q.Channel] == "" {
				return nil
			}
			if err := deriveOutcome(tx, &record, q.Channel); err != nil {
				return err
			}
			if q.Status != "" && record.NotificationStatus != q.Status {
				return nil
			}
			records = keepNewest(records, record, limit+1, eventOrder)
			return nil
		}); err != nil {
			return err
		}
		if len(records) > limit {
			page.NextCursor = nextPage(cursor, eventOrder(records[limit-1]))
			records = records[:limit]
		}
		page.Items = append(page.Items, records...)
		return nil
	})
	return page, err
}

func keepNewest[T any](items []T, item T, limit int, key func(T) string) []T {
	order := key(item)
	index := sort.Search(len(items), func(i int) bool { return key(items[i]) <= order })
	if index >= limit {
		return items
	}
	if len(items) < limit {
		var zero T
		items = append(items, zero)
	}
	copy(items[index+1:], items[index:])
	items[index] = item
	return items
}

func deriveOutcome(tx *bolt.Tx, record *EventRecord, channelFilter string) error {
	record.ChannelStatuses = map[string]string{}
	record.NotificationStatus = "no_eligible_channel"
	if record.Baseline {
		record.NotificationStatus = "baseline"
	} else if record.Decision.Unavailable {
		record.NotificationStatus = "legacy_unknown"
	} else if !record.Decision.Matched {
		record.NotificationStatus = "filtered"
	}
	rank := 0
	priority := map[string]int{"canceled": 1, "delivered": 2, "failed": 3, "pending": 4}
	for channel, id := range record.DeliveryIDs {
		var delivery Delivery
		found, err := getJSON(tx.Bucket([]byte("outbox")), id, &delivery)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		record.ChannelStatuses[channel] = delivery.Status
		if (channelFilter == "" || channelFilter == channel) && priority[delivery.Status] > rank {
			rank, record.NotificationStatus = priority[delivery.Status], delivery.Status
		}
	}
	return nil
}
func normalizeLegacyView(record *EventRecord) {
	normalizeRecord(record)
	if record.Decision.Reason == "" {
		record.DiscoveryUnavailable = true
		record.Decision.Unavailable = true
		record.Decision.Reason = "legacy_decision_unavailable"
		if record.Baseline {
			record.Decision.Reason = "initial_history"
		}
	}
}
func findRecord(tx *bolt.Tx, provider, id string) (EventRecord, bool, error) {
	var record EventRecord
	if id == "" {
		return record, false, errors.New("event ID is required")
	}
	if provider != "" {
		found, err := getJSON(tx.Bucket([]byte("events")), eventKey(provider, id), &record)
		if found {
			normalizeLegacyView(&record)
		}
		return record, found, err
	}
	found := false
	err := forEachValue(tx.Bucket([]byte("events")), func(_, v []byte) error {
		var candidate EventRecord
		if err := json.Unmarshal(v, &candidate); err != nil {
			return err
		}
		if candidate.Event.ID != id {
			return nil
		}
		if found {
			return errors.New("event ID is ambiguous; specify provider")
		}
		record, found = candidate, true
		normalizeLegacyView(&record)
		return nil
	})
	return record, found, err
}

func listDeliveries(tx *bolt.Tx, q Query) (DeliveryPage, error) {
	limit, cursor, err := queryPage(q, "deliveries")
	page := DeliveryPage{Items: []Delivery{}, AsOf: cursor.AsOf}
	if err != nil {
		return page, err
	}
	switch q.Status {
	case "", "pending", "failed", "delivered", "canceled":
	default:
		return page, errors.New("unknown delivery status")
	}
	var deliveries []Delivery
	if err := forEachValue(tx.Bucket([]byte("outbox")), func(_, v []byte) error {
		var delivery Delivery
		if err := json.Unmarshal(v, &delivery); err != nil {
			return err
		}
		event := delivery.Notification.Event
		if q.Provider != "" && q.Provider != event.Provider.Slug || q.EventID != "" && q.EventID != event.ID || q.Channel != "" && q.Channel != delivery.Channel || q.Status != "" && q.Status != delivery.Status {
			return nil
		}
		if delivery.CreatedAt.IsZero() {
			delivery.CreatedAt = delivery.Notification.DetectedAt
		}
		if !inTimeRange(delivery.CreatedAt, q, cursor.AsOf) || cursor.Last != "" && deliveryOrder(delivery) >= cursor.Last {
			return nil
		}
		var err error
		delivery, err = effectiveDelivery(tx, delivery)
		if err != nil {
			return err
		}
		deliveries = keepNewest(deliveries, delivery, limit+1, deliveryOrder)
		return nil
	}); err != nil {
		return page, err
	}
	sort.Slice(deliveries, func(i, j int) bool { return deliveryOrder(deliveries[i]) > deliveryOrder(deliveries[j]) })
	if len(deliveries) > limit {
		page.NextCursor = nextPage(cursor, deliveryOrder(deliveries[limit-1]))
		deliveries = deliveries[:limit]
	}
	page.Items = append(page.Items, deliveries...)
	return page, nil
}
func (s *Store) ListDeliveries(q Query) (DeliveryPage, error) {
	var page DeliveryPage
	err := s.db.View(func(tx *bolt.Tx) error { var err error; page, err = listDeliveries(tx, q); return err })
	return page, err
}

func eventDetails(tx *bolt.Tx, provider, id string, q Query) (EventView, bool, error) {
	view := EventView{Deliveries: []Delivery{}, Revisions: []RevisionRecord{}}
	record, found, err := findRecord(tx, provider, id)
	if err != nil || !found {
		return view, found, err
	}
	if err := deriveOutcome(tx, &record, ""); err != nil {
		return view, false, err
	}
	view.Record = record
	q.Provider, q.EventID = record.Event.Provider.Slug, id
	limit, cursor, err := queryPage(q, "revisions")
	if err != nil {
		return view, false, err
	}
	view.AsOf = cursor.AsOf
	deliveries, err := listDeliveries(tx, Query{Provider: q.Provider, EventID: id, Limit: limit})
	if err != nil {
		return view, false, err
	}
	view.Deliveries, view.DeliveryNextCursor = deliveries.Items, deliveries.NextCursor
	prefix := eventKey(q.Provider, id) + "\x00"
	keys := []string{}
	err = reversePrefix(tx.Bucket([]byte("revisions")), prefix, cursor.Last, func(k, v []byte) (bool, error) {
		var revision RevisionRecord
		if err := json.Unmarshal(v, &revision); err != nil {
			return false, err
		}
		if !inTimeRange(revision.ObservedAt, q, cursor.AsOf) {
			return false, nil
		}
		view.Revisions = append(view.Revisions, revision)
		keys = append(keys, string(k))
		return len(view.Revisions) > limit, nil
	})
	if err != nil {
		return view, false, err
	}
	if len(view.Revisions) > limit {
		view.NextCursor = nextPage(cursor, keys[limit-1])
		view.Revisions = view.Revisions[:limit]
	}
	return view, true, nil
}

func reversePrefix(bucket *bolt.Bucket, prefix, last string, fn func([]byte, []byte) (bool, error)) error {
	if bucket == nil {
		return nil
	}
	cursor := bucket.Cursor()
	upper := prefix + "\xff"
	if last != "" {
		upper = last
	}
	k, v := cursor.Seek([]byte(upper))
	if k == nil {
		k, v = cursor.Last()
	} else {
		k, v = cursor.Prev()
	}
	for ; k != nil && strings.HasPrefix(string(k), prefix); k, v = cursor.Prev() {
		stop, err := fn(k, v)
		if err != nil {
			return err
		}
		if stop {
			break
		}
	}
	return nil
}
func (s *Store) Event(provider, id string) (EventView, bool, error) {
	return s.EventDetails(provider, id, Query{})
}
func (s *Store) EventDetails(provider, id string, q Query) (EventView, bool, error) {
	var view EventView
	var found bool
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		view, found, err = eventDetails(tx, provider, id, q)
		return err
	})
	return view, found, err
}

func (s *Store) Explain(provider, id string, policy Policy) (EventExplanation, bool, error) {
	var explanation EventExplanation
	var found bool
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		explanation.EventView, found, err = eventDetails(tx, provider, id, Query{})
		if err != nil || !found {
			return err
		}
		record := explanation.Record
		explanation.Reason = record.Decision.Reason
		explanation.Channels, err = explainChannels(tx, record, policy)
		if err != nil {
			return err
		}
		if policy.Match != nil {
			explanation.CurrentMatched, explanation.CurrentReason = policy.Match(record.Event)
		} else {
			explanation.CurrentReason = "filter_unavailable"
		}
		priority := -1
		ranks := map[string]int{"canceled": 1, "delivered": 2, "failed": 3, "pending": 4}
		// Channel explanations are sorted, so equal-priority outcomes are stable.
		for _, channel := range explanation.Channels {
			rank := ranks[channel.Status]
			if rank > 0 && rank > priority {
				priority, explanation.Reason = rank, channel.Reason
			}
		}
		if priority < 0 && !record.Baseline && record.Decision.Matched {
			explanation.Reason = "no_eligible_channel"
		}
		return nil
	})
	return explanation, found, err
}

func (s *Store) DeliveryDetails(id string, q Query) (DeliveryView, bool, error) {
	view := DeliveryView{AttemptHistory: []Attempt{}}
	q.EventID = id
	limit, cursor, err := queryPage(q, "attempts")
	if err != nil {
		return view, false, err
	}
	view.AsOf = cursor.AsOf
	found := false
	err = s.db.View(func(tx *bolt.Tx) error {
		var err error
		found, err = getJSON(tx.Bucket([]byte("outbox")), id, &view.Delivery)
		if err != nil || !found {
			return err
		}
		view.Delivery, err = effectiveDelivery(tx, view.Delivery)
		if err != nil {
			return err
		}
		if tx.Bucket([]byte("attempts")) == nil {
			view.LegacyAttempts = view.Attempts
		}
		if err := reversePrefix(tx.Bucket([]byte("attempts")), id+":", cursor.Last, func(_, v []byte) (bool, error) {
			var attempt Attempt
			if err := json.Unmarshal(v, &attempt); err != nil {
				return false, err
			}
			if inTimeRange(attempt.StartedAt, q, cursor.AsOf) {
				view.AttemptHistory = append(view.AttemptHistory, attempt)
			}
			return len(view.AttemptHistory) > limit, nil
		}); err != nil {
			return err
		}
		if len(view.AttemptHistory) > limit {
			view.NextCursor = nextPage(cursor, view.AttemptHistory[limit-1].ID)
			view.AttemptHistory = view.AttemptHistory[:limit]
		}
		return nil
	})
	return view, found, err
}
