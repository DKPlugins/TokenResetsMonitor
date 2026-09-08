package model

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// FieldChange describes a meaningful source change using stable field names.
type FieldChange struct {
	Field  string `json:"field"`
	Before string `json:"before"`
	After  string `json:"after"`
}

// MeaningfulChanges ignores presentation prose, confidence scores and list order.
func MeaningfulChanges(before, after Event) []FieldChange {
	var changes []FieldChange
	add := func(field, a, b string) {
		if a != b {
			changes = append(changes, FieldChange{field, a, b})
		}
	}
	add("status", before.Status, after.Status)
	add("event_type", before.EventType, after.EventType)
	add("scope.products", canonicalValues(before.Scope.Products), canonicalValues(after.Scope.Products))
	add("scope.plans", canonicalValues(before.Scope.Plans), canonicalValues(after.Scope.Plans))
	add("scope.windows", canonicalValues(before.Scope.Windows), canonicalValues(after.Scope.Windows))
	add("scope.scope_evidence", before.Scope.Evidence, after.Scope.Evidence)
	add("confidence.label", before.Confidence.Label, after.Confidence.Label)
	add("announced_at", timestamp(&before.AnnouncedAt), timestamp(&after.AnnouncedAt))
	add("published_at", timestamp(before.PublishedAt), timestamp(after.PublishedAt))
	add("effective_at", timestamp(before.EffectiveAt), timestamp(after.EffectiveAt))
	add("expected_by", timestamp(before.ExpectedBy), timestamp(after.ExpectedBy))
	add("observed_effective_at", timestamp(before.ObservedEffectiveAt), timestamp(after.ObservedEffectiveAt))
	add("expires_at", timestamp(before.ExpiresAt), timestamp(after.ExpiresAt))
	return changes
}

func canonicalValues(values []string) string {
	unique := map[string]bool{}
	for _, value := range values {
		unique[value] = true
	}
	sorted := make([]string, 0, len(unique))
	for value := range unique {
		sorted = append(sorted, value)
	}
	sort.Strings(sorted)
	if len(sorted) == 0 {
		return "[]"
	}
	data, _ := json.Marshal(sorted)
	return string(data)
}

func timestamp(value *time.Time) string {
	if value == nil || value.IsZero() {
		return ""
	}
	return strings.TrimSpace(value.UTC().Format(time.RFC3339Nano))
}
