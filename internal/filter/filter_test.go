package filter

import (
	"testing"

	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
)

func base() (model.Event, config.Config) {
	return model.Event{Provider: model.Provider{Slug: "openai-codex"}, Status: "published", EventType: "hard_reset", Confidence: model.Confidence{Label: "reported"}, Scope: model.Scope{Plans: []string{"plus"}, Products: []string{"codex"}, Windows: []string{"weekly"}}},
		config.Config{Providers: []config.ProviderFilter{{Slug: "openai-codex", Plans: []string{"plus"}, Products: []string{"codex"}, Windows: []string{"weekly"}}}, EventTypes: []string{"hard_reset"}, MinimumConfidence: "reported", UnknownScope: "include"}
}

func TestScopeAndUnknownApplyPerDimension(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(*model.Event, *config.Config)
		want   bool
	}{
		{"exact", func(*model.Event, *config.Config) {}, true},
		{"or_in_plans", func(e *model.Event, c *config.Config) { e.Scope.Plans = []string{"pro", "plus"} }, true},
		{"and_dimensions", func(e *model.Event, c *config.Config) { e.Scope.Windows = []string{"session-5h"} }, false},
		{"unknown_included", func(e *model.Event, c *config.Config) { e.Scope.Products = nil }, true},
		{"unknown_excluded", func(e *model.Event, c *config.Config) { e.Scope.Products = nil; c.UnknownScope = "exclude" }, false},
		{"unrestricted_dimension", func(e *model.Event, c *config.Config) {
			e.Scope.Products = nil
			c.Providers[0].Products = nil
			c.UnknownScope = "exclude"
		}, true},
		{"source_paid_wildcard", func(e *model.Event, c *config.Config) { e.Scope.Plans = []string{"any-paid"} }, true},
		{"selected_paid_wildcard", func(e *model.Event, c *config.Config) { c.Providers[0].Plans = []string{"any-paid"} }, true},
		{"wildcard_excludes_free", func(e *model.Event, c *config.Config) {
			e.Scope.Plans = []string{"free"}
			c.Providers[0].Plans = []string{"any-paid"}
		}, false},
		{"no_prose_inference", func(e *model.Event, c *config.Config) {
			e.Title = "All Plus users reset"
			e.Scope.Plans = nil
			c.UnknownScope = "exclude"
		}, false},
		{"provider", func(e *model.Event, c *config.Config) { e.Provider.Slug = "anthropic-claude" }, false},
		{"retracted", func(e *model.Event, c *config.Config) { e.Status = "retracted" }, false},
		{"event_type", func(e *model.Event, c *config.Config) { e.EventType = "scheduled_reset" }, false},
		{"extra_event_type", func(e *model.Event, c *config.Config) {
			e.EventType = "scheduled_reset"
			c.EventTypes = append(c.EventTypes, "scheduled_reset")
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, c := base()
			tc.modify(&e, &c)
			got, reason := Match(e, c)
			if got != tc.want {
				t.Fatalf("got %v (%s), want %v", got, reason, tc.want)
			}
		})
	}
}

func TestAllConfidenceThresholds(t *testing.T) {
	levels := []string{"unverified", "probable", "reported", "verified", "official"}
	for minimum, want := range levels {
		for actual, label := range levels {
			e, c := base()
			e.Confidence.Label = label
			c.MinimumConfidence = want
			got, _ := Match(e, c)
			if got != (actual >= minimum) {
				t.Fatalf("minimum %s actual %s: %v", want, label, got)
			}
		}
	}
	e, c := base()
	e.Confidence.Label = "future-label"
	if got, _ := Match(e, c); got {
		t.Fatal("unknown confidence must not notify")
	}
}
