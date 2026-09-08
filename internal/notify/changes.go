package notify

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
)

func amendmentText(n model.Notification) string {
	heading := "Announcement corrected"
	if n.Kind == "retraction" {
		heading = "Announcement retracted"
	}
	if n.Test {
		heading += " — TEST"
	}
	labels := map[string]string{
		"status": "Status", "event_type": "Event type", "scope.plans": "Plans",
		"scope.products": "Products", "scope.windows": "Limit windows", "scope.scope_evidence": "Scope evidence",
		"confidence.label": "Confidence", "announced_at": "Announced", "published_at": "Published",
		"effective_at": "Effective", "expected_by": "Expected by", "observed_effective_at": "Observed effective",
		"expires_at": "Expires",
	}
	var differences strings.Builder
	for _, change := range n.Changes {
		label := labels[change.Field]
		if label == "" {
			label = change.Field
		}
		fmt.Fprintf(&differences, "%s: %s → %s\n", truncate(label, 60), truncate(changeValue(change.Before), 180), truncate(changeValue(change.After), 180))
	}
	previous := "Previous delivered version unavailable"
	if n.PreviousEvent != nil {
		previous = fmt.Sprintf("Revision %d → %d", n.PreviousEvent.Revision, n.Event.Revision)
	}
	if n.PreviousEventUnverified {
		previous += "\nComparison uses the saved legacy version; exact previous delivery unavailable."
	}
	return fmt.Sprintf("TokenResetsMonitor — %s\n%s\nProvider: %s\n%s\n\n%s\n\nThis updates an earlier public announcement.\n%s",
		heading, truncate(n.Event.Title, 400), truncate(n.Event.Provider.Name, 100), previous,
		truncate(differences.String(), 2200), truncate(n.Event.Links.HTML, 700))
}

func changeValue(value string) string {
	if value == "" || value == "[]" {
		return "unknown"
	}
	var values []string
	if json.Unmarshal([]byte(value), &values) == nil && values != nil {
		return strings.Join(values, ", ")
	}
	return value
}
