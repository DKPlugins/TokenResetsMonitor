package notify

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
)

func TestAmendmentsShowComparisonAndEscapeSlack(t *testing.T) {
	n := TestNotification("openai-codex")
	old := n.Event
	n.PreviousEvent, n.Kind, n.RelatedNotificationID = &old, "correction", "original-id"
	n.Event.Revision++
	n.Event.Scope.Plans = []string{"plus", "pro"}
	n.Changes = []model.FieldChange{{Field: "scope.plans", Before: "pro", After: "plus, pro"}}
	text := telegramText(n)
	if !strings.Contains(text, "Announcement corrected") || !strings.Contains(text, "Plans: pro → plus, pro") || !strings.Contains(text, n.Event.Links.HTML) {
		t.Fatalf("missing comparison: %s", text)
	}
	n.PreviousEventUnverified = true
	if text := telegramText(n); !strings.Contains(text, "exact previous delivery unavailable") {
		t.Fatal("legacy comparison presented as an exact delivered snapshot")
	}
	n.Kind = "retraction"
	n.Event.Title = "withdrawn <!everyone> & <@U123>"
	if text := slackText(n); !strings.Contains(text, "Announcement retracted") || strings.Contains(text, "<!everyone>") || strings.Contains(text, "<@U123>") {
		t.Fatal("retraction or Slack escaping missing")
	}
	n.Event.Title = strings.Repeat("😀", 5000)
	for i := 0; i < 20; i++ {
		n.Changes = append(n.Changes, model.FieldChange{Field: "scope.plans", Before: strings.Repeat("😀", 5000), After: strings.Repeat("😀", 5000)})
	}
	text = telegramText(n)
	if len(text) > 3800 || !utf8.ValidString(text) || !strings.Contains(text, "Announcement retracted") {
		t.Fatal("amendment exceeds text budget")
	}
}

func TestAmendmentWebhookFieldsAreAdditive(t *testing.T) {
	n := TestNotification("")
	ordinary, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"kind", "previous_event", "changes", "related_notification_id"} {
		if strings.Contains(string(ordinary), "\""+key+"\"") {
			t.Fatalf("ordinary contract changed: %s", key)
		}
	}
	old := n.Event
	n.PreviousEvent, n.Kind, n.RelatedNotificationID = &old, "retraction", "original"
	n.Changes = []model.FieldChange{{Field: "status", Before: "published", After: "retracted"}}
	n.Event.Status = "retracted"
	rendered, err := New(webhookConfig("https://example.invalid/hook")).render("webhook", n)
	if err != nil {
		t.Fatal(err)
	}
	var got model.Notification
	if err := json.Unmarshal(rendered.body, &got); err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != 1 || got.Kind != "retraction" || got.PreviousEvent.Status != "published" || got.RelatedNotificationID != "original" {
		t.Fatal("amendment contract lost")
	}
	cfg := webhookConfig("https://example.invalid/hook")
	cfg.Webhook.BodyTemplate = `{{.Kind}}:{{.PreviousEvent.Status}}:{{.RelatedNotificationID}}`
	rendered, err = New(cfg).render("webhook", n)
	if err != nil || string(rendered.body) != "retraction:published:original" {
		t.Fatal("template amendment fields unavailable", err)
	}
}
