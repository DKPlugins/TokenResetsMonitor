// Package filter applies explicit source scope without guessing from prose.
package filter

import (
	"slices"

	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
)

var confidence = map[string]int{"unverified": 0, "probable": 1, "reported": 2, "verified": 3, "official": 4}

// Match uses AND between dimensions and OR within each configured list.
// Reasons are fixed diagnostic codes and never include source-provided prose.
func Match(event model.Event, cfg config.Config) (bool, string) {
	if event.Status != "published" {
		return false, "not_published"
	}
	var provider *config.ProviderFilter
	for i := range cfg.Providers {
		if cfg.Providers[i].Slug == event.Provider.Slug {
			provider = &cfg.Providers[i]
			break
		}
	}
	if provider == nil {
		return false, "provider_not_selected"
	}
	if !slices.Contains(cfg.EventTypes, event.EventType) {
		return false, "event_type_not_selected"
	}
	rank, known := confidence[event.Confidence.Label]
	minimum, valid := confidence[cfg.MinimumConfidence]
	if !known || !valid || rank < minimum {
		return false, "confidence_below_minimum_or_unknown"
	}
	includeUnknown := cfg.UnknownScope == "include"
	if !scopeMatches(event.Scope.Products, provider.Products, includeUnknown, false) {
		return false, "products_outside_scope"
	}
	if !scopeMatches(event.Scope.Plans, provider.Plans, includeUnknown, true) {
		return false, "plans_outside_scope"
	}
	if !scopeMatches(event.Scope.Windows, provider.Windows, includeUnknown, false) {
		return false, "windows_outside_scope"
	}
	return true, "matched"
}

func scopeMatches(actual, wanted []string, includeUnknown, plans bool) bool {
	if len(wanted) == 0 {
		return true
	}
	if len(actual) == 0 {
		return includeUnknown
	}
	for _, a := range actual {
		for _, w := range wanted {
			if a == w {
				return true
			}
			// any-paid is the source's wildcard for this provider's paid plans.
			// Explicit free plans are never covered by that wildcard.
			if plans && ((a == "any-paid" && w != "free") || (w == "any-paid" && a != "free")) {
				return true
			}
		}
	}
	return false
}
