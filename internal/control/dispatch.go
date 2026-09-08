package control

import (
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/filter"
	"github.com/DKPlugins/TokenResetsMonitor/internal/state"
)

func IsMutation(operation string) bool {
	return operation == "deliveries.retry" || operation == "deliveries.retry-failed"
}

func ValidateRequest(r Request) error {
	switch r.Operation {
	case "history.list", "history.show", "history.explain", "filters.preview", "deliveries.list", "deliveries.show", "deliveries.retry", "deliveries.retry-failed":
	default:
		return ErrInvalid
	}
	if r.Query.Limit < 0 || r.Query.Limit > 200 || len(r.Query.Cursor) > 4096 {
		return ErrInvalid
	}
	if r.Query.SinceDuration != "" {
		duration, err := time.ParseDuration(r.Query.SinceDuration)
		if err != nil || duration <= 0 || r.Query.Since != nil {
			return ErrInvalid
		}
	}
	if r.Query.Since != nil && r.Query.Until != nil && r.Query.Since.After(*r.Query.Until) {
		return ErrInvalid
	}
	switch r.Query.Status {
	case "", "pending", "failed", "delivered", "canceled":
	case "baseline", "filtered", "no_eligible_channel", "legacy_unknown":
		if r.Operation != "history.list" && r.Operation != "filters.preview" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	for _, value := range []string{r.Provider, r.ID, r.Query.Provider, r.Query.EventID, r.Query.Channel, r.Query.Status} {
		if len(value) > 256 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return ErrInvalid
		}
	}
	if r.Query.Channel != "" && r.Query.Channel != "webhook" && r.Query.Channel != "telegram" && r.Query.Channel != "slack" {
		return ErrInvalid
	}
	if r.Candidate != nil {
		if r.Operation != "filters.preview" || r.Candidate.Validate() != nil {
			return ErrInvalid
		}
	}
	if r.Operation == "filters.preview" && r.Candidate == nil {
		return ErrInvalid
	}
	if IsMutation(r.Operation) {
		if r.Provider != "" || r.Query.Provider != "" || r.Query.EventID != "" || r.Query.Channel != "" || r.Query.Status != "" || r.Query.Cursor != "" || r.Query.Since != nil || r.Query.Until != nil || r.Query.SinceDuration != "" || (r.Query.Limit != 0 && r.Query.Limit != 50) {
			return ErrInvalid
		}
	}
	switch r.Operation {
	case "history.show", "history.explain", "deliveries.show":
		if r.Query.Status != "" || r.Query.Channel != "" || r.Query.EventID != "" {
			return ErrInvalid
		}
	}
	if r.Operation == "history.explain" && (r.Query.Cursor != "" || r.Query.Since != nil || r.Query.Until != nil || r.Query.SinceDuration != "" || (r.Query.Limit != 0 && r.Query.Limit != 50)) {
		return ErrInvalid
	}
	if r.Operation == "deliveries.show" && (r.Provider != "" || r.Query.Provider != "") {
		return ErrInvalid
	}
	switch r.Operation {
	case "history.show", "history.explain", "deliveries.show", "deliveries.retry":
		if r.ID == "" {
			return ErrInvalid
		}
	default:
		if r.ID != "" {
			return ErrInvalid
		}
	}
	return nil
}

type RetryResult struct {
	Requeued int             `json:"requeued"`
	Delivery *state.Delivery `json:"delivery,omitempty"`
}

type PreviewItem struct {
	Provider         string `json:"provider"`
	EventID          string `json:"event_id"`
	Revision         int    `json:"revision"`
	Title            string `json:"title"`
	Baseline         bool   `json:"baseline"`
	ActiveMatched    bool   `json:"active_matched"`
	ActiveReason     string `json:"active_reason"`
	CandidateMatched bool   `json:"candidate_matched"`
	CandidateReason  string `json:"candidate_reason"`
	RecordedReason   string `json:"recorded_reason"`
	Changed          bool   `json:"changed"`
}

type PreviewPage struct {
	Items      []PreviewItem     `json:"items"`
	NextCursor string            `json:"next_cursor,omitempty"`
	Active     config.FilterSpec `json:"active_filters"`
	Candidate  config.FilterSpec `json:"candidate_filters"`
	Notice     string            `json:"notice"`
}

// Dispatch does no external I/O. The daemon calls it from its runtime owner so
// filter snapshots and mutation eligibility belong to one config generation.
// Offline callers must use a read-only Store and only non-mutating operations.
func Dispatch(store *state.Store, policy state.Policy, active config.FilterSpec, r Request) (any, error) {
	if err := ValidateRequest(r); err != nil {
		return nil, err
	}
	if r.Query.Limit == 0 {
		r.Query.Limit = 50
	}
	switch r.Operation {
	case "history.list":
		return store.ListEvents(r.Query)
	case "history.show":
		v, found, err := store.EventDetails(r.Provider, r.ID, r.Query)
		if err == nil && !found {
			err = ErrNotFound
		}
		return v, err
	case "history.explain":
		v, found, err := store.Explain(r.Provider, r.ID, policy)
		if err == nil && !found {
			err = ErrNotFound
		}
		return v, err
	case "deliveries.list":
		return store.ListDeliveries(r.Query)
	case "deliveries.show":
		v, found, err := store.DeliveryDetails(r.ID, r.Query)
		if err == nil && !found {
			err = ErrNotFound
		}
		return v, err
	case "deliveries.retry":
		d, err := store.RetryDelivery(r.ID, policy, time.Now())
		return RetryResult{Requeued: 1, Delivery: &d}, err
	case "deliveries.retry-failed":
		count, err := store.RetryFailedWithPolicy(policy, time.Now())
		return RetryResult{Requeued: count}, err
	case "filters.preview":
		page, err := store.ListEvents(r.Query)
		if err != nil {
			return nil, err
		}
		result := PreviewPage{Items: []PreviewItem{}, NextCursor: page.NextCursor, Active: active, Candidate: *r.Candidate, Notice: "Filter matches only; baseline, previous deliveries and recipient eligibility still apply. No messages were queued or sent."}
		for _, record := range page.Items {
			e := record.Event
			oldMatch, oldReason := filter.Match(e, active.Config())
			newMatch, newReason := filter.Match(e, r.Candidate.Config())
			result.Items = append(result.Items, PreviewItem{Provider: e.Provider.Slug, EventID: e.ID, Revision: e.Revision, Title: e.Title, Baseline: record.Baseline, ActiveMatched: oldMatch, ActiveReason: oldReason, CandidateMatched: newMatch, CandidateReason: newReason, RecordedReason: record.Decision.Reason, Changed: oldMatch != newMatch || oldReason != newReason})
		}
		return result, nil
	}
	return nil, errors.New("unsupported management operation")
}
