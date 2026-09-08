// Package monitor coordinates full-history scans and independent delivery workers.
package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/api"
	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/filter"
	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
	"github.com/DKPlugins/TokenResetsMonitor/internal/observability"
	"github.com/DKPlugins/TokenResetsMonitor/internal/state"
)

type source interface {
	Events(context.Context, string) ([]model.Event, error)
	Event(context.Context, string) (model.Event, error)
}

type sender interface {
	Send(context.Context, string, model.Notification) model.DeliveryResult
}

type sourceFailures struct{ error }

type monitor struct {
	store   *state.Store
	source  source
	sender  sender
	policy  state.Policy
	logger  *slog.Logger
	cycles  atomic.Uint64
	clock   func() time.Time
	metrics *observability.Metrics
	stop    <-chan struct{}
}

// Run polls immediately; CLI callers supply the configuration path through RunWithOptions.
func Run(ctx context.Context, cfg config.Config, logger *slog.Logger, once bool) error {
	return RunWithOptions(ctx, cfg, logger, once, RunOptions{})
}

func recoverablePoll(err error) bool { var outage sourceFailures; return errors.As(err, &outage) }

func (m *monitor) now() time.Time {
	if m.clock != nil {
		return m.clock().UTC()
	}
	return time.Now().UTC()
}

func retryDeadline(err error, now time.Time) time.Time {
	var apiError *api.Error
	if errors.As(err, &apiError) && apiError.RetryAfter > 0 {
		return now.Add(apiError.RetryAfter)
	}
	return time.Time{}
}

func (m *monitor) poll(ctx context.Context) error {
	cycle := m.cycles.Add(1)
	var failures error
	selected := make(map[string]bool)
	for _, slug := range m.policy.Providers {
		selected[slug] = true
		if ctx.Err() != nil {
			return ctx.Err()
		}
		notBefore, recovering, err := m.store.ProviderPollState(slug)
		if err != nil {
			return err
		}
		if m.now().Before(notBefore) {
			m.logger.Debug("Provider scan deferred by upstream Retry-After", "cycle_id", cycle, "provider", slug, "retry_at", notBefore)
			continue
		}
		started := time.Now()
		budget := api.WithScanBudget(ctx)
		scanCtx, cancel := context.WithTimeout(budget, 5*time.Minute)
		events, sourceErr := m.source.Events(scanCtx, slug)
		if scanCtx.Err() != nil {
			sourceErr = scanCtx.Err()
		}
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if sourceErr != nil {
			if err := m.store.MarkProviderError(slug, "Provider scan failed; see program logs", retryDeadline(sourceErr, m.now())); err != nil {
				return err
			}
			m.metrics.ObserveScan(slug, time.Since(started), false)
			m.logger.Warn("Provider scan failed", "cycle_id", cycle, "provider", slug, "error", safeSourceError(sourceErr))
			if until := retryDeadline(sourceErr, m.now()); !until.IsZero() {
				m.logger.Warn("Provider polling paused by upstream Retry-After", "cycle_id", cycle, "provider", slug, "retry_at", until, "delay", until.Sub(m.now()))
			}
			failures = errors.Join(failures, fmt.Errorf("provider %s scan failed", slug))
			continue
		}
		// A completed listing is committed independently of optional detail
		// verification. Slow or unavailable old records cannot erase fresh work.
		detections, err := m.store.CommitScan(slug, events, m.policy, m.now())
		if err != nil {
			return fmt.Errorf("provider %s scan could not be committed: %w", slug, err)
		}
		m.logDetections(cycle, slug, detections)
		if err := m.verifyDetails(budget, cycle, slug, events); err != nil {
			if !recoverablePoll(err) {
				return err
			}
			failures = errors.Join(failures, err)
		}
		m.metrics.ObserveScan(slug, time.Since(started), true)
		m.logger.Info("Provider scan completed", "cycle_id", cycle, "provider", slug, "events", len(events), "duration", time.Since(started), "recovered", recovering)
	}
	// Removing a provider filter must not hide corrections to announcements
	// already accepted by a still-active destination.
	tracked, err := m.store.TrackedProviders(m.policy)
	if err != nil {
		return err
	}
	for _, slug := range tracked {
		if selected[slug] {
			continue
		}
		if err := m.verifyDetails(api.WithScanBudget(ctx), cycle, slug, nil); err != nil {
			if !recoverablePoll(err) {
				return err
			}
			failures = errors.Join(failures, err)
		}
	}
	if err := m.store.PublishStatus(true, time.Now()); err != nil {
		return errors.Join(failures, err)
	}
	if failures != nil {
		return sourceFailures{failures}
	}
	return nil
}

func (m *monitor) logDetections(cycle uint64, slug string, detections []state.Detection) {
	for _, d := range detections {
		m.logger.Debug("Event observation recorded", "cycle_id", cycle, "provider", slug, "event_id", d.EventID, "revision", d.Revision, "queued", d.Queued, "reason", d.Reason)
	}
}

// verifyDetails advances after each lookup, including failed lookups, so a
// missing or oversized record cannot starve the rest of the tracked history.
func (m *monitor) verifyDetails(ctx context.Context, cycle uint64, slug string, events []model.Event) error {
	notBefore, _, err := m.store.ProviderPollState(slug)
	if err != nil {
		return err
	}
	if m.now().Before(notBefore) {
		return nil
	}
	missing, err := m.store.MissingTracked(slug, events)
	if err != nil {
		return err
	}
	detailCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var unavailable bool
	var budgetFailure bool
	for i, old := range missing {
		if i >= 32 || detailCtx.Err() != nil {
			break
		}
		detail, sourceErr := m.source.Event(detailCtx, old.ID)
		if err := m.store.AdvanceDetailCursor(slug, old.ID); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if sourceErr != nil {
			unavailable = true
			budgetFailure = errors.Is(sourceErr, api.ErrScanBudget)
			until := retryDeadline(sourceErr, m.now())
			if !until.IsZero() {
				if err := m.store.MarkProviderError(slug, "Event detail verification deferred by upstream Retry-After", until); err != nil {
					return err
				}
				break
			}
			if errors.Is(sourceErr, api.ErrScanBudget) || detailCtx.Err() != nil {
				break
			}
			continue // HTTP 404/410 is not proof of withdrawal.
		}
		if detail.ID != old.ID || detail.Provider.Slug != slug || (detail.Status != "published" && detail.Status != "retracted") {
			continue
		}
		detections, err := m.store.CommitDetails(slug, []model.Event{detail}, m.policy, m.now())
		if err != nil {
			return err
		}
		m.logDetections(cycle, slug, detections)
	}
	if unavailable {
		m.logger.Warn("Some event details could not be verified; stored decisions remain available", "provider", slug)
		if budgetFailure {
			return sourceFailures{fmt.Errorf("provider %s detail verification exceeded scan budget", slug)}
		}
	}
	return nil
}

func safeSourceError(err error) string {
	var apiError *api.Error
	if errors.As(err, &apiError) {
		return apiError.Error()
	}
	return "source request failed"
}

func (m *monitor) deliver(ctx context.Context, channel string, once bool) error {
	limit := 100
	if once {
		limit = 0
	}
	due, err := m.store.Due(channel, time.Now(), limit)
	if err != nil {
		return err
	}
	failed := false
	for _, queued := range due {
		if ctx.Err() != nil {
			return nil
		}
		select {
		case <-m.stop:
			return nil
		default:
		}
		// A scan may have canceled or updated an entry after Due's snapshot.
		delivery, found, err := m.store.BeginAttempt(queued.Notification.ID, time.Now())
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		m.logger.Info("Notification attempt", "channel", channel, "notification_id", delivery.Notification.ID, "event_id", delivery.Notification.Event.ID, "attempt", delivery.Attempts)
		result := m.sender.Send(ctx, channel, delivery.Notification)
		// An acknowledged request must be recorded even if shutdown raced with its response.
		m.metrics.ObserveDelivery(channel, result)
		if err := m.store.CompleteAttempt(delivery.AttemptID, result, m.policy, time.Now()); err != nil {
			return err
		}
		if result.Success {
			m.logger.Info("Notification delivered", "channel", channel, "notification_id", delivery.Notification.ID, "event_id", delivery.Notification.Event.ID, "status_code", result.StatusCode, "duration", result.Duration, "recovered", delivery.Attempts > 1)
		} else {
			failed = true
			m.logger.Warn("Notification delivery failed", "channel", channel, "notification_id", delivery.Notification.ID, "event_id", delivery.Notification.Event.ID, "status_code", result.StatusCode, "retryable", result.Retryable, "error", result.Error)
		}
		if err := m.store.PublishStatus(true, time.Now()); err != nil {
			return err
		}
	}
	if once && failed {
		return errors.New("one or more notifications could not be delivered; delivery state was saved")
	}
	return nil
}

func RetryFailed(cfg config.Config) (int, error) {
	db, err := state.Open(cfg.StatePath)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	if err := db.Reconcile(makePolicy(cfg)); err != nil {
		return 0, err
	}
	count, err := db.RetryFailedWithPolicy(makePolicy(cfg), time.Now())
	if err != nil {
		return 0, err
	}
	return count, db.PublishStatus(false, time.Now())
}

// RetryDelivery schedules one permanent failure. Offline commands take the
// exclusive database lock; the daemon uses the same store operation via control.
func RetryDelivery(cfg config.Config, id string) (state.Delivery, error) {
	db, err := state.Open(cfg.StatePath)
	if err != nil {
		return state.Delivery{}, err
	}
	defer db.Close()
	policy := makePolicy(cfg)
	if err := db.Reconcile(policy); err != nil {
		return state.Delivery{}, err
	}
	delivery, err := db.RetryDelivery(id, policy, time.Now())
	if err != nil {
		return state.Delivery{}, err
	}
	return delivery, db.PublishStatus(false, time.Now())
}

func makePolicy(cfg config.Config) state.Policy {
	filters, _ := json.Marshal(cfg.FilterSpec())
	policy := state.Policy{Channels: map[string]string{}, Changes: map[string]bool{"telegram": cfg.Telegram.NotifyChanges, "slack": cfg.Slack.NotifyChanges, "webhook": cfg.Webhook.NotifyChanges}, FilterConfig: filters, Match: func(event model.Event) (bool, string) { return filter.Match(event, cfg) }}
	for _, provider := range cfg.Providers {
		policy.Providers = append(policy.Providers, provider.Slug)
	}
	if cfg.Webhook.Enabled {
		policy.Channels["webhook"] = fingerprint(recipientURL(cfg.Webhook.URL))
	}
	if cfg.Telegram.Enabled {
		policy.Channels["telegram"] = fingerprint(recipientURL(cfg.Telegram.APIBaseURL) + "\x00" + cfg.Telegram.ChatID + "\x00" + strconv.FormatInt(cfg.Telegram.MessageThreadID, 10))
	}
	if cfg.Slack.Enabled {
		policy.Channels["slack"] = fingerprint(cfg.Slack.WebhookURL)
	}
	return policy
}

func fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Authentication supplied in headers, URL userinfo, or common credential query
// parameters is excluded from recipient identity so rotation retains the queue.
func recipientURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = nil
	u.Fragment = ""
	query := u.Query()
	var keys []string
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		lower := strings.ToLower(key)
		for _, secret := range []string{"token", "access_token", "refresh_token", "api_token", "key", "api_key", "apikey", "secret", "client_secret", "password", "signature", "authorization", "auth", "credential", "credentials"} {
			if lower == secret {
				query.Del(key)
				break
			}
		}
	}
	u.RawQuery = query.Encode()
	return u.String()
}
