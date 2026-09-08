// Package monitor coordinates full-history scans and independent delivery workers.
package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	for _, slug := range m.policy.Providers {
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
		scanCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		scanCtx = api.WithScanBudget(scanCtx)
		events, sourceErr := m.source.Events(scanCtx, slug)
		var detailNotBefore time.Time
		if sourceErr == nil {
			missing, err := m.store.MissingPending(slug, events)
			if err != nil {
				cancel()
				return err
			}
			for _, old := range missing {
				detail, err := m.source.Event(scanCtx, old.ID)
				if err != nil {
					if errors.Is(err, api.ErrScanBudget) {
						sourceErr = err
						break
					}
					detailNotBefore = retryDeadline(err, m.now())
					if scanCtx.Err() != nil || !detailNotBefore.IsZero() {
						break
					}
					continue
				}
				if detail.ID == old.ID && detail.Provider.Slug == slug && detail.Status == "retracted" {
					events = append(events, detail)
				}
			}
		}
		if scanCtx.Err() != nil {
			sourceErr = scanCtx.Err()
		}
		cancel()
		// Quiescing a generation must never commit its interrupted scan.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if sourceErr != nil {
			notBefore = retryDeadline(sourceErr, m.now())
			if err := m.store.MarkProviderError(slug, "Provider scan failed; see program logs", notBefore); err != nil {
				return err
			}
			m.metrics.ObserveScan(slug, time.Since(started), false)
			m.logger.Warn("Provider scan failed", "cycle_id", cycle, "provider", slug, "error", safeSourceError(sourceErr))
			if !notBefore.IsZero() {
				m.logger.Warn("Provider polling paused by upstream Retry-After", "cycle_id", cycle, "provider", slug, "retry_at", notBefore, "delay", notBefore.Sub(m.now()))
			}
			failures = errors.Join(failures, fmt.Errorf("provider %s scan failed", slug))
			continue
		}
		detections, err := m.store.CommitScan(slug, events, m.policy, m.now())
		if err != nil {
			return fmt.Errorf("provider %s scan could not be committed: %w", slug, err)
		}
		if !detailNotBefore.IsZero() {
			if err := m.store.MarkProviderError(slug, "Event detail verification deferred by upstream Retry-After", detailNotBefore); err != nil {
				return err
			}
			m.logger.Warn("Provider polling paused by upstream Retry-After", "cycle_id", cycle, "provider", slug, "retry_at", detailNotBefore, "delay", detailNotBefore.Sub(m.now()))
		}
		for _, d := range detections {
			if d.Reason == "stale_revision" {
				m.logger.Warn("Ignored older event revision", "provider", slug, "event_id", d.EventID, "reason", d.Reason)
				continue
			}
			m.logger.Info("Event discovered or revised", "cycle_id", cycle, "provider", slug, "event_id", d.EventID, "revision", d.Revision, "queued", d.Queued)
			if d.Queued == 0 {
				m.logger.Debug("Event produced no new notification", "provider", slug, "event_id", d.EventID, "reason", d.Reason)
			}
		}
		m.metrics.ObserveScan(slug, time.Since(started), detailNotBefore.IsZero())
		m.logger.Info("Provider scan completed", "cycle_id", cycle, "provider", slug, "events", len(events), "duration", time.Since(started), "recovered", recovering && detailNotBefore.IsZero())
	}
	if err := m.store.PublishStatus(true, time.Now()); err != nil {
		return errors.Join(failures, err)
	}
	if failures != nil {
		return sourceFailures{failures}
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
		delivery, found, err := m.store.Delivery(queued.Notification.ID)
		if err != nil {
			return err
		}
		if !found || delivery.Status != "pending" {
			continue
		}
		cooldown, err := m.store.RecipientCooldown(channel, delivery.Destination)
		if err != nil {
			return err
		}
		if time.Now().Before(cooldown) {
			break
		}
		m.logger.Info("Notification attempt", "channel", channel, "notification_id", delivery.Notification.ID, "event_id", delivery.Notification.Event.ID, "attempt", delivery.Attempts+1)
		result := m.sender.Send(ctx, channel, delivery.Notification)
		// An acknowledged request must be recorded even if shutdown raced with its response.
		if ctx.Err() != nil && !result.Success {
			return nil
		}
		m.metrics.ObserveDelivery(channel, result)
		if err := m.store.Complete(delivery.Notification.ID, result, time.Now()); err != nil {
			return err
		}
		if result.Success {
			m.logger.Info("Notification delivered", "channel", channel, "notification_id", delivery.Notification.ID, "event_id", delivery.Notification.Event.ID, "status_code", result.StatusCode, "duration", result.Duration, "recovered", delivery.Attempts > 0)
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
	count, err := db.RetryFailed(time.Now())
	if err != nil {
		return 0, err
	}
	return count, db.PublishStatus(false, time.Now())
}

func makePolicy(cfg config.Config) state.Policy {
	policy := state.Policy{Channels: map[string]string{}, Match: func(event model.Event) (bool, string) { return filter.Match(event, cfg) }}
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
