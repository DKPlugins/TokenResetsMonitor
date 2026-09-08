package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/control"
	"github.com/DKPlugins/TokenResetsMonitor/internal/filter"
	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
	"github.com/DKPlugins/TokenResetsMonitor/internal/state"
)

func managementFixture(t *testing.T) (config.Config, string, state.Policy) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.StatePath = filepath.Join(dir, "state.db")
	cfg.Providers = []config.ProviderFilter{{Slug: "openai-codex", Plans: []string{"pro"}}}
	cfg.Telegram.Enabled = true
	cfg.Telegram.BotToken = "${TRM_TEST_MANAGEMENT_BOT}"
	cfg.Telegram.ChatID = "123"
	t.Setenv("TRM_TEST_MANAGEMENT_BOT", "")
	path := filepath.Join(dir, "config.yaml")
	if err := config.WriteNew(path, cfg); err != nil {
		t.Fatal(err)
	}
	policy := state.Policy{Providers: []string{"openai-codex"}, Channels: map[string]string{"telegram": "destination-fingerprint"}, Match: func(e model.Event) (bool, string) { return filter.Match(e, cfg) }}
	return cfg, path, policy
}

func seedManagementStore(t *testing.T, cfg config.Config, policy state.Policy) *state.Store {
	t.Helper()
	store, err := state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Reconcile(policy); err != nil {
		store.Close()
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	event := model.Event{ID: "baseline-event", Provider: model.Provider{Slug: "openai-codex", Name: "Codex"}, Revision: 1, EventType: "hard_reset", Status: "published", Title: "Stored announcement", AnnouncedAt: now, Scope: model.Scope{Plans: []string{"pro"}}, Confidence: model.Confidence{Label: "official"}}
	if _, err := store.CommitScan("openai-codex", []model.Event{event}, policy, now); err != nil {
		store.Close()
		t.Fatal(err)
	}
	event.ID = "new-event"
	if _, err := store.CommitScan("openai-codex", []model.Event{event}, policy, now.Add(time.Second)); err != nil {
		store.Close()
		t.Fatal(err)
	}
	return store
}

func TestManagementOfflineReadAndPreviewDoNotWriteStateOrNeedSecrets(t *testing.T) {
	cfg, path, policy := managementFixture(t)
	store := seedManagementStore(t, cfg, policy)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	candidate := cfg
	candidate.Providers = []config.ProviderFilter{{Slug: "openai-codex", Plans: []string{"free"}}}
	candidatePath := filepath.Join(t.TempDir(), "candidate.yaml")
	if err := config.WriteNew(candidatePath, candidate); err != nil {
		t.Fatal(err)
	}
	commands := [][]string{
		{"history", "list", "--since", "24h", "--limit", "1"},
		{"history", "show", "--id", "new-event"},
		{"history", "explain", "--id", "baseline-event"},
		{"filters", "preview", "--candidate-config", candidatePath},
		{"deliveries", "list", "--event-id", "new-event", "--status", "pending"},
	}
	for _, args := range commands {
		var out, errOut bytes.Buffer
		args = append(args, "--json", "--config", path)
		if code := Execute(context.Background(), args, strings.NewReader(""), &out, &errOut); code != 0 {
			t.Fatalf("%v: %d %s", args, code, errOut.String())
		}
		var result control.Result
		if json.Unmarshal(out.Bytes(), &result) != nil || result.ConfigurationSource != "saved" {
			t.Fatalf("invalid offline result: %s", out.String())
		}
		if args[0] == "filters" {
			var preview control.PreviewPage
			if json.Unmarshal(result.Data, &preview) != nil || len(preview.Items) != 2 {
				t.Fatalf("preview missing real events: %s", result.Data)
			}
			for _, item := range preview.Items {
				if !item.ActiveMatched || item.CandidateMatched || item.CandidateReason != "plans_outside_scope" {
					t.Fatalf("preview did not compare filters: %+v", item)
				}
			}
		}
	}
	after, err := os.ReadFile(cfg.StatePath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("read-only management changed state")
	}
	matches, _ := filepath.Glob(cfg.StatePath + ".backup-*")
	if len(matches) != 0 {
		t.Fatal("read-only commands triggered a migration backup")
	}
}

func TestManagementUsesLiveGenerationDespiteInvalidSavedConfig(t *testing.T) {
	cfg, path, policy := managementFixture(t)
	store := seedManagementStore(t, cfg, policy)
	defer store.Close()
	server, err := control.Start(cfg.StatePath, func(ctx context.Context, r control.Request) (any, error) {
		value, err := control.Dispatch(store, policy, cfg.FilterSpec(), r)
		if err != nil {
			return nil, err
		}
		return control.WithMetadata(value, "running", 23)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if err := os.WriteFile(path, []byte("broken: [yaml"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := Execute(context.Background(), []string{"history", "explain", "--id", "new-event", "--state-path", cfg.StatePath, "--config", path, "--json"}, strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("live command depends on invalid saved filters: %d %s", code, errOut.String())
	}
	var result control.Result
	if json.Unmarshal(out.Bytes(), &result) != nil || result.ConfigGeneration != 23 || result.ConfigurationSource != "running" {
		t.Fatalf("wrong source: %s", out.String())
	}
}

func TestManagementRetriesOneFailedDeliveryWhileDatabaseRemainsOpen(t *testing.T) {
	cfg, path, policy := managementFixture(t)
	store := seedManagementStore(t, cfg, policy)
	defer store.Close()
	due, err := store.Due("telegram", time.Now().Add(time.Hour), 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("queue fixture: %v %+v", err, due)
	}
	id := due[0].Notification.ID
	if err := store.Complete(id, model.DeliveryResult{StatusCode: 400, Error: "permanent rejection"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	server, err := control.Start(cfg.StatePath, func(ctx context.Context, r control.Request) (any, error) {
		value, err := control.Dispatch(store, policy, cfg.FilterSpec(), r)
		if err != nil {
			return nil, err
		}
		return control.WithMetadata(value, "running", 1)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	var out, errOut bytes.Buffer
	args := []string{"deliveries", "retry", "--id", id, "--config", path, "--json"}
	if code := Execute(context.Background(), args, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("live retry failed: %d %s", code, errOut.String())
	}
	d, found, err := store.Delivery(id)
	if err != nil || !found || d.Status != "pending" || d.Attempts != 1 {
		t.Fatalf("retry lost identity/history: %+v %v", d, err)
	}
	out.Reset()
	errOut.Reset()
	if code := Execute(context.Background(), args, strings.NewReader(""), &out, &errOut); code == 0 {
		t.Fatal("already-pending retry reported success")
	}
	if !strings.Contains(errOut.String(), "already pending") {
		t.Fatalf("retry refusal is not explained: %s", errOut.String())
	}
}

func TestManagementArgumentsAndMissingState(t *testing.T) {
	for _, args := range [][]string{
		{"history", "show"}, {"deliveries", "retry"}, {"filters", "preview"},
		{"history", "list", "--limit", "201"}, {"history", "list", "--since", "-1h"},
		{"deliveries", "retry-failed", "--channel", "telegram"},
	} {
		var out, errOut bytes.Buffer
		if code := Execute(context.Background(), args, strings.NewReader(""), &out, &errOut); code != 2 {
			t.Fatalf("invalid args %v returned %d: %s", args, code, errOut.String())
		}
	}
	path := filepath.Join(t.TempDir(), "absent.db")
	var out, errOut bytes.Buffer
	if code := Execute(context.Background(), []string{"history", "list", "--state-path", path}, strings.NewReader(""), &out, &errOut); code == 0 {
		t.Fatal("missing state treated as empty history")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("missing state was created")
	}
}

func TestTerminalOutputDoesNotInterpretSourceControlCharacters(t *testing.T) {
	value := "title\x1b[31m\nforged\trow"
	got := terminal(value)
	if strings.ContainsAny(got, "\x1b\n\t") {
		t.Fatalf("source controls reached terminal: %q", got)
	}
}

func TestHistoryRelativeSincePaginationAndTextExplanation(t *testing.T) {
	cfg, path, policy := managementFixture(t)
	policy.FilterConfig, _ = json.Marshal(cfg.FilterSpec())
	store := seedManagementStore(t, cfg, policy)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	args := []string{"history", "list", "--since", "24h", "--limit", "1", "--json", "--config", path}
	if code := Execute(context.Background(), args, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("first page: %d %s", code, errOut.String())
	}
	var result control.Result
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var first state.EventPage
	if err := json.Unmarshal(result.Data, &first); err != nil || len(first.Items) != 1 || first.NextCursor == "" {
		t.Fatalf("first page unavailable: %s", result.Data)
	}
	out.Reset()
	errOut.Reset()
	if code := Execute(context.Background(), append(args, "--cursor", first.NextCursor), strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("relative since changed across pages: %d %s", code, errOut.String())
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var second state.EventPage
	if err := json.Unmarshal(result.Data, &second); err != nil || len(second.Items) != 1 || second.Items[0].Event.ID == first.Items[0].Event.ID || !second.AsOf.Equal(first.AsOf) {
		t.Fatalf("second page repeated/missing: %s", result.Data)
	}
	out.Reset()
	errOut.Reset()
	if code := Execute(context.Background(), []string{"history", "explain", "--id", "new-event", "--config", path}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("explanation: %d %s", code, errOut.String())
	}
	for _, part := range []string{"Decision recorded at:", "Recorded filters:", "plans=pro", "telegram: pending", "A notification is queued"} {
		if !strings.Contains(out.String(), part) {
			t.Fatalf("explanation missing %q: %s", part, out.String())
		}
	}
}

func TestManagementUntilAndAdditionalHistoryStatuses(t *testing.T) {
	cfg, path, policy := managementFixture(t)
	store := seedManagementStore(t, cfg, policy)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"no_eligible_channel", "legacy_unknown"} {
		var out, errOut bytes.Buffer
		if code := Execute(context.Background(), []string{"history", "list", "--status", status, "--config", path, "--json"}, strings.NewReader(""), &out, &errOut); code != 0 {
			t.Fatalf("valid history status %s rejected: %d %s", status, code, errOut.String())
		}
	}
	var out, errOut bytes.Buffer
	until := time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	if code := Execute(context.Background(), []string{"history", "list", "--until", until, "--config", path, "--json"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("until query rejected: %d %s", code, errOut.String())
	}
	var result control.Result
	var page state.EventPage
	if json.Unmarshal(out.Bytes(), &result) != nil || json.Unmarshal(result.Data, &page) != nil || len(page.Items) != 0 {
		t.Fatalf("until did not constrain results: %s", out.String())
	}
	for _, args := range [][]string{
		{"history", "list", "--until", "24h"},
		{"history", "list", "--since", time.Now().UTC().Format(time.RFC3339), "--until", until},
	} {
		out.Reset()
		errOut.Reset()
		if code := Execute(context.Background(), append(args, "--config", path), strings.NewReader(""), &out, &errOut); code != 2 {
			t.Fatalf("invalid date range was not rejected: %v: %d %s", args, code, errOut.String())
		}
	}
}

func TestPreviewChangedTracksMatchAndReasonChanges(t *testing.T) {
	cfg, _, policy := managementFixture(t)
	cfg.MinimumConfidence = "official"
	policy.Match = func(event model.Event) (bool, string) { return filter.Match(event, cfg) }
	store := seedManagementStore(t, cfg, policy)
	defer store.Close()
	now := time.Now().UTC().Add(-30 * time.Second)
	event := model.Event{ID: "low-confidence", Provider: model.Provider{Slug: "openai-codex", Name: "Codex"}, Revision: 1, EventType: "hard_reset", Status: "published", Title: "Low confidence", AnnouncedAt: now, Scope: model.Scope{Plans: []string{"pro"}}, Confidence: model.Confidence{Label: "reported"}}
	if _, err := store.CommitScan("openai-codex", []model.Event{event}, policy, now); err != nil {
		t.Fatal(err)
	}
	candidate := cfg.FilterSpec()
	candidate.MinimumConfidence = "reported"
	candidate.Providers = []config.ProviderFilter{{Slug: "openai-codex", Plans: []string{"free"}}}
	value, err := control.Dispatch(store, policy, cfg.FilterSpec(), control.Request{Operation: "filters.preview", Candidate: &candidate})
	if err != nil {
		t.Fatal(err)
	}
	page, ok := value.(control.PreviewPage)
	if !ok {
		t.Fatal("preview type unavailable")
	}
	found := false
	for _, item := range page.Items {
		if item.EventID == "low-confidence" {
			found = true
			if item.ActiveMatched || item.CandidateMatched || !item.Changed || item.ActiveReason == item.CandidateReason {
				t.Fatalf("changed reason was hidden: %+v", item)
			}
		}
	}
	if !found {
		t.Fatal("real event missing from preview")
	}
	same := cfg.FilterSpec()
	value, err = control.Dispatch(store, policy, same, control.Request{Operation: "filters.preview", Candidate: &same})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range value.(control.PreviewPage).Items {
		if item.Changed {
			t.Fatalf("unchanged filters marked changed: %+v", item)
		}
	}
}
