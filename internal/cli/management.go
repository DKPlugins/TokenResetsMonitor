package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/control"
	"github.com/DKPlugins/TokenResetsMonitor/internal/filter"
	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
	"github.com/DKPlugins/TokenResetsMonitor/internal/monitor"
	"github.com/DKPlugins/TokenResetsMonitor/internal/state"
)

func management(ctx context.Context, command, sub string, opts options, overrides map[string]string, out, errOut io.Writer) int {
	request := control.Request{Operation: command + "." + sub, Provider: opts.provider, ID: opts.recordID, Query: state.Query{Provider: opts.provider, EventID: opts.eventID, Channel: opts.channel, Status: opts.status, Limit: opts.limit, Cursor: opts.cursor}}
	if opts.limit < 1 || opts.limit > 200 {
		fmt.Fprintln(errOut, "--limit must be between 1 and 200.")
		return 2
	}
	if opts.since != "" {
		if duration, err := time.ParseDuration(opts.since); err == nil && duration > 0 {
			request.Query.SinceDuration = duration.String()
		} else {
			since, err := managementSince(opts.since, time.Now())
			if err != nil {
				fmt.Fprintln(errOut, err)
				return 2
			}
			request.Query.Since = &since
		}
	}
	if opts.until != "" {
		until, err := time.Parse(time.RFC3339, opts.until)
		if err != nil {
			fmt.Fprintln(errOut, "--until must be an RFC3339 timestamp.")
			return 2
		}
		until = until.UTC()
		request.Query.Until = &until
	}
	if command == "filters" {
		if opts.candidatePath == "" {
			fmt.Fprintln(errOut, "filters preview requires --candidate-config PATH.")
			return 2
		}
		candidate, err := config.LoadFilterSpec(opts.candidatePath)
		if err != nil {
			fmt.Fprintln(errOut, "Cannot load candidate filters:", err)
			return 2
		}
		request.Candidate = &candidate
	}
	if err := control.ValidateRequest(request); err != nil {
		fmt.Fprintln(errOut, "Invalid management command or arguments. Use --help.")
		return 2
	}
	if control.IsMutation(request.Operation) && (opts.provider != "" || opts.eventID != "" || opts.channel != "" || opts.status != "" || opts.since != "" || opts.cursor != "") {
		fmt.Fprintln(errOut, "Retry commands accept --id only; use deliveries list to select a failed delivery.")
		return 2
	}
	paths, err := loadOperational(opts, overrides)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}
	var result control.Result
	err = control.Call(ctx, paths.StatePath, request, &result)
	if errors.Is(err, control.ErrUnavailable) {
		result, err = offlineManagement(opts, overrides, paths.StatePath, request)
	}
	if err != nil {
		fmt.Fprintln(errOut, "Management command failed:", err)
		return 1
	}
	if opts.json {
		encoder := json.NewEncoder(out)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(result); err != nil {
			fmt.Fprintln(errOut, "Cannot write management result.")
			return 1
		}
		return 0
	}
	if err := printManagement(out, request.Operation, result); err != nil {
		fmt.Fprintln(errOut, "Cannot display management result.")
		return 1
	}
	return 0
}

func managementSince(value string, now time.Time) (time.Time, error) {
	if d, err := time.ParseDuration(value); err == nil && d > 0 {
		return now.Add(-d).UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, errors.New("--since must be a positive duration (24h) or RFC3339 timestamp")
}

func offlineManagement(opts options, overrides map[string]string, path string, request control.Request) (control.Result, error) {
	if control.IsMutation(request.Operation) {
		cfg, err := config.Load(opts.configPath, overrides)
		if err != nil {
			return control.Result{}, err
		}
		if err := config.Validate(cfg); err != nil {
			return control.Result{}, err
		}
		var value control.RetryResult
		if request.Operation == "deliveries.retry" {
			d, err := monitor.RetryDelivery(cfg, request.ID)
			if err != nil {
				return control.Result{}, err
			}
			value = control.RetryResult{Requeued: 1, Delivery: &d}
		} else {
			count, err := monitor.RetryFailed(cfg)
			if err != nil {
				return control.Result{}, err
			}
			value.Requeued = count
		}
		return control.WithMetadata(value, "saved", 0)
	}
	cfg := config.Defaults()
	if request.Operation == "history.explain" || request.Operation == "filters.preview" {
		var err error
		cfg, err = config.LoadManagement(opts.configPath, overrides)
		if err != nil {
			return control.Result{}, err
		}
	}
	store, err := state.OpenReadOnly(path)
	if err != nil {
		return control.Result{}, err
	}
	defer store.Close()
	policy := state.Policy{Match: func(event model.Event) (bool, string) { return filter.Match(event, cfg) }}
	for _, p := range cfg.Providers {
		policy.Providers = append(policy.Providers, p.Slug)
	}
	value, err := control.Dispatch(store, policy, cfg.FilterSpec(), request)
	if err != nil {
		return control.Result{}, err
	}
	return control.WithMetadata(value, "saved", 0)
}

// Terminal output quotes control characters; public source prose cannot issue
// escape sequences or create misleading extra CLI records.
func terminal(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
}

func printManagement(out io.Writer, operation string, result control.Result) error {
	if result.ConfigurationSource == "running" {
		fmt.Fprintf(out, "Configuration: running generation %d\n", result.ConfigGeneration)
	} else {
		fmt.Fprintln(out, "Configuration: saved file (monitor stopped)")
	}
	switch operation {
	case "history.list":
		var page state.EventPage
		if err := json.Unmarshal(result.Data, &page); err != nil {
			return err
		}
		table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(table, "PROVIDER\tEVENT ID\tREVISION\tNOTIFICATION\tDECISION\tTITLE")
		for _, record := range page.Items {
			fmt.Fprintf(table, "%s\t%s\t%d\t%s\t%s\t%s\n", terminal(record.Event.Provider.Slug), terminal(record.Event.ID), record.Event.Revision, terminal(record.NotificationStatus), terminal(record.Decision.Reason), terminal(record.Event.Title))
		}
		if err := table.Flush(); err != nil {
			return err
		}
		printCursor(out, page.NextCursor)
	case "history.show", "history.explain":
		var view state.EventExplanation
		if err := json.Unmarshal(result.Data, &view); err != nil {
			return err
		}
		e := view.Record.Event
		fmt.Fprintf(out, "%s\nProvider: %s | Event ID: %s | Revision: %d | Status: %s\n", terminal(e.Title), terminal(e.Provider.Slug), terminal(e.ID), e.Revision, terminal(e.Status))
		fmt.Fprintf(out, "Detected: %s | Initial history: %t\nRecorded decision: %s\n", view.Record.DetectedAt.UTC().Format(time.RFC3339), view.Record.Baseline, reasonText(view.Record.Decision.Reason))
		if !view.Record.Decision.ObservedAt.IsZero() {
			fmt.Fprintln(out, "Decision recorded at:", view.Record.Decision.ObservedAt.UTC().Format(time.RFC3339))
		}
		if view.Record.Decision.Unavailable {
			fmt.Fprintln(out, "The original decision details were not recorded by the older version.")
		}
		printRecordedFilters(out, view.Record.Decision.FilterConfig)
		if view.Record.HistoryPrunedBefore != nil {
			fmt.Fprintln(out, "Detailed revision history pruned before:", view.Record.HistoryPrunedBefore.UTC().Format(time.RFC3339))
		}
		var acknowledged []string
		for channel := range view.Record.Acknowledged {
			acknowledged = append(acknowledged, channel)
		}
		sort.Strings(acknowledged)
		for _, channel := range acknowledged {
			ack := view.Record.Acknowledged[channel]
			if ack.LegacyUnverified {
				fmt.Fprintf(out, "%s: legacy acknowledgement uses the stored snapshot; the exact historical network payload is unverified.\n", terminal(channel))
			}
		}
		if operation == "history.explain" {
			fmt.Fprintf(out, "Why: %s\nCurrent filter result: %t (%s)\n", reasonText(view.Reason), view.CurrentMatched, reasonText(view.CurrentReason))
			for _, channel := range view.Channels {
				fmt.Fprintf(out, "%s: %s | %s\n", terminal(channel.Channel), terminal(channel.Status), reasonText(channel.Reason))
				if channel.EffectiveNextAttempt != nil {
					fmt.Fprintln(out, "  Next attempt:", channel.EffectiveNextAttempt.UTC().Format(time.RFC3339))
				}
			}
		}
		if len(view.Deliveries) == 0 {
			fmt.Fprintln(out, "Deliveries: none")
		}
		for _, delivery := range view.Deliveries {
			printDelivery(out, delivery)
		}
		if view.DeliveryNextCursor != "" {
			fmt.Fprintf(out, "More deliveries: deliveries list --provider %s --event-id %s --cursor %s\n", terminal(e.Provider.Slug), terminal(e.ID), terminal(view.DeliveryNextCursor))
		}
		for _, revision := range view.Revisions {
			fmt.Fprintf(out, "Revision %d | observed %s | %s\n", revision.Event.Revision, revision.ObservedAt.UTC().Format(time.RFC3339), reasonText(revision.Decision.Reason))
		}
		printCursor(out, view.NextCursor)
	case "deliveries.list":
		var page state.DeliveryPage
		if err := json.Unmarshal(result.Data, &page); err != nil {
			return err
		}
		if len(page.Items) == 0 {
			fmt.Fprintln(out, "No deliveries match.")
		}
		for _, delivery := range page.Items {
			printDelivery(out, delivery)
		}
		printCursor(out, page.NextCursor)
	case "deliveries.show":
		var view state.DeliveryView
		if err := json.Unmarshal(result.Data, &view); err != nil {
			return err
		}
		printDelivery(out, view.Delivery)
		for _, attempt := range view.AttemptHistory {
			finished := "in progress"
			if attempt.FinishedAt != nil {
				finished = attempt.FinishedAt.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(out, "Attempt %d | %s -> %s | %s | HTTP %d | %d ms\n", attempt.Number, attempt.StartedAt.UTC().Format(time.RFC3339), finished, terminal(attempt.Outcome), attempt.StatusCode, attempt.DurationMS)
			if attempt.Error != "" {
				fmt.Fprintln(out, "  Reason:", terminal(attempt.Error))
			}
			if attempt.NextAttempt != nil {
				fmt.Fprintln(out, "  Retry scheduled:", attempt.NextAttempt.UTC().Format(time.RFC3339))
			}
		}
		if len(view.AttemptHistory) == 0 {
			fmt.Fprintln(out, "No individual attempt records are available.")
		}
		printCursor(out, view.NextCursor)
	case "filters.preview":
		var page control.PreviewPage
		if err := json.Unmarshal(result.Data, &page); err != nil {
			return err
		}
		fmt.Fprintln(out, page.Notice)
		table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(table, "PROVIDER\tEVENT ID\tBASELINE\tACTIVE\tCANDIDATE\tCHANGED\tCANDIDATE REASON")
		for _, item := range page.Items {
			fmt.Fprintf(table, "%s\t%s\t%t\t%t\t%t\t%t\t%s\n", terminal(item.Provider), terminal(item.EventID), item.Baseline, item.ActiveMatched, item.CandidateMatched, item.Changed, terminal(item.CandidateReason))
		}
		if err := table.Flush(); err != nil {
			return err
		}
		printCursor(out, page.NextCursor)
	case "deliveries.retry", "deliveries.retry-failed":
		var value control.RetryResult
		if err := json.Unmarshal(result.Data, &value); err != nil {
			return err
		}
		fmt.Fprintf(out, "Requeued %d failed deliveries.\n", value.Requeued)
		if value.Delivery != nil {
			printDelivery(out, *value.Delivery)
		}
		if result.ConfigurationSource != "running" {
			fmt.Fprintln(out, "Start the monitor to send queued deliveries.")
		}
	}
	return nil
}

func printRecordedFilters(out io.Writer, raw json.RawMessage) {
	var spec config.FilterSpec
	if len(raw) == 0 || json.Unmarshal(raw, &spec) != nil {
		fmt.Fprintln(out, "Recorded filters: unavailable.")
		return
	}
	values := func(items []string) string {
		if len(items) == 0 {
			return "all"
		}
		return terminal(strings.Join(items, ","))
	}
	fmt.Fprintf(out, "Recorded filters: event types=%s; minimum confidence=%s; unknown scope=%s\n", values(spec.EventTypes), terminal(spec.MinimumConfidence), terminal(spec.UnknownScope))
	for _, provider := range spec.Providers {
		fmt.Fprintf(out, "  %s: plans=%s; products=%s; windows=%s\n", terminal(provider.Slug), values(provider.Plans), values(provider.Products), values(provider.Windows))
	}
}

func printDelivery(out io.Writer, d state.Delivery) {
	fmt.Fprintf(out, "Delivery %s | %s | %s | event %s | attempts %d\n", terminal(d.Notification.ID), terminal(d.Channel), terminal(d.Status), terminal(d.Notification.Event.ID), d.Attempts)
	if d.AttemptID != "" {
		fmt.Fprintln(out, "An attempt is currently in progress.")
	}
	if d.Status == "pending" && d.AttemptID == "" {
		next := d.NextAttempt
		if d.EffectiveNextAttempt != nil {
			next = *d.EffectiveNextAttempt
		}
		fmt.Fprintln(out, "Next attempt:", next.UTC().Format(time.RFC3339))
	}
	if d.CancellationReason != "" {
		fmt.Fprintln(out, "Cancellation:", reasonText(d.CancellationReason))
	}
	if d.LegacyAttempts > 0 {
		fmt.Fprintf(out, "Legacy attempts without individual records: %d\n", d.LegacyAttempts)
	}
	if d.HistoryPrunedBefore != nil {
		fmt.Fprintln(out, "Detailed history pruned before:", d.HistoryPrunedBefore.UTC().Format(time.RFC3339))
	}
	if d.LastError != "" {
		fmt.Fprintln(out, "Reason:", terminal(d.LastError))
	}
	if d.DeliveredAt != nil {
		fmt.Fprintln(out, "Delivered:", d.DeliveredAt.UTC().Format(time.RFC3339))
	}
}

func printCursor(out io.Writer, cursor string) {
	if cursor != "" {
		fmt.Fprintln(out, "Next page: --cursor", terminal(cursor))
	}
}

func reasonText(code string) string {
	messages := map[string]string{
		"initial_history":                     "The event belonged to the initial history and was not queued.",
		"matched":                             "The event passes all configured filters.",
		"awaiting_delivery":                   "A notification is queued; see the next attempt time.",
		"delivery_in_progress":                "A notification is being sent.",
		"delivery_failed":                     "Delivery failed permanently and requires a manual retry.",
		"already_delivered":                   "The destination acknowledged this notification.",
		"delivery_canceled":                   "The delivery was canceled.",
		"no_eligible_channel":                 "No destination was eligible to receive this event.",
		"legacy_decision_unavailable":         "The old state does not contain the original filtering decision.",
		"not_published":                       "The source event is not published.",
		"provider_not_selected":               "The provider is not selected.",
		"event_type_not_selected":             "The event type is not selected.",
		"confidence_below_minimum_or_unknown": "Confidence is below the configured minimum or unknown.",
		"products_outside_scope":              "The products do not match the configured scope.",
		"plans_outside_scope":                 "The plans do not match the configured scope.",
		"windows_outside_scope":               "The windows do not match the configured scope.",
		"recipient_changed":                   "The destination changed after this event was discovered.",
		"channel_disabled":                    "The channel is disabled.",
		"provider_disabled":                   "The provider was disabled.",
		"changes_disabled":                    "Correction and retraction notifications are disabled for this channel.",
		"recipient_not_eligible_at_discovery": "This destination was not eligible when the event was discovered.",
		"superseded":                          "A newer notification replaced this delivery.",
		"no_meaningful_change":                "There is no meaningful change from the acknowledged announcement.",
		"filter_unavailable":                  "Current filters could not be evaluated.",
		"channel_state_unavailable":           "Current destination eligibility is unavailable while reading offline history.",
		"channel_not_enabled_at_discovery":    "The channel was not enabled when this event was discovered.",
		"awaiting_source_revision":            "No delivery was queued; a future source revision may be evaluated.",
	}
	if text, found := messages[code]; found {
		return text + " (" + code + ")"
	}
	return terminal(code)
}
