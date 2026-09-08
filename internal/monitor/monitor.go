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
	"sync"
	"sync/atomic"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/api"
	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/filter"
	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
	"github.com/DKPlugins/TokenResetsMonitor/internal/notify"
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
	store  *state.Store
	source source
	sender sender
	policy state.Policy
	logger *slog.Logger
	cycles atomic.Uint64
	clock  func() time.Time
}

// Run polls immediately. Each enabled channel has its own durable worker, so a
// slow or failing recipient does not stall another channel or the next scan.
func Run(ctx context.Context, cfg config.Config, logger *slog.Logger, once bool) error {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.PollDuration() <= 0 {
		return errors.New("poll interval must be positive")
	}
	db, err := state.Open(cfg.StatePath)
	if err != nil {
		return err
	}
	defer db.Close()
	policy := makePolicy(cfg)
	if err := db.Reconcile(policy); err != nil {
		return err
	}
	m := &monitor{store: db, source: api.New(cfg.APIBaseURL, cfg.HTTPTimeout(), db), sender: notify.New(cfg), policy: policy, logger: logger}
	if err := db.PublishStatus(true, time.Now()); err != nil {
		return err
	}
	defer func() {
		if err := db.PublishStatus(false, time.Now()); err != nil {
			logger.Error("Unable to publish stopped status")
		}
	}()
	logger.Info("Monitor started", "providers", len(policy.Providers), "channels", len(policy.Channels), "once", once)
	defer logger.Info("Monitor stopped")
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if once {
		pollErr := m.poll(ctx)
		var wg sync.WaitGroup
		errs := make(chan error, len(policy.Channels))
		for channel := range policy.Channels {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := m.deliver(ctx, channel, true); err != nil {
					errs <- err
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			pollErr = errors.Join(pollErr, err)
		}
		if ctx.Err() != nil {
			return nil
		}
		return pollErr
	}
	fatal := make(chan error, len(policy.Channels)+1)
	var workers sync.WaitGroup
	for channel := range policy.Channels {
		workers.Add(1)
		go func() {
			defer workers.Done()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				if err := m.deliver(ctx, channel, false); err != nil {
					fatal <- err
					cancel()
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := db.PublishStatus(true, time.Now()); err != nil {
					fatal <- err
					cancel()
					return
				}
			}
		}
	}()
	defer workers.Wait()
	defer cancel()
	if err := m.poll(ctx); err != nil && !recoverablePoll(err) && ctx.Err() == nil {
		return err
	}
	ticker := time.NewTicker(cfg.PollDuration())
	defer ticker.Stop()
	for {
		select {
		case err := <-fatal:
			return err
		case <-ctx.Done():
			select {
			case err := <-fatal:
				return err
			default:
				return nil
			}
		case <-ticker.C:
			if err := m.poll(ctx); err != nil && !recoverablePoll(err) && ctx.Err() == nil {
				return err
			}
		}
	}
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
		events, err := m.source.Events(ctx, slug)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			notBefore = retryDeadline(err, m.now())
			if saveErr := m.store.MarkProviderError(slug, "Provider scan failed; see program logs", notBefore); saveErr != nil {
				return saveErr
			}
			m.logger.Warn("Provider scan failed", "cycle_id", cycle, "provider", slug, "error", safeSourceError(err))
			if !notBefore.IsZero() {
				m.logger.Warn("Provider polling paused by upstream Retry-After", "cycle_id", cycle, "provider", slug, "retry_at", notBefore, "delay", notBefore.Sub(m.now()))
			}
			failures = errors.Join(failures, fmt.Errorf("provider %s scan failed", slug))
			continue
		}
		missing, err := m.store.MissingPending(slug, events)
		if err != nil {
			return err
		}
		var detailNotBefore time.Time
		for _, old := range missing {
			detail, err := m.source.Event(ctx, old.ID)
			if err != nil {
				m.logger.Debug("Missing event could not be verified; delivery remains unchanged", "cycle_id", cycle, "provider", slug, "event_id", old.ID)
				detailNotBefore = retryDeadline(err, m.now())
				if !detailNotBefore.IsZero() {
					break
				}
				continue
			}
			if detail.ID == old.ID && detail.Provider.Slug == slug && detail.Status == "retracted" {
				events = append(events, detail)
			}
		}
		detections, err := m.store.CommitScan(slug, events, m.policy, m.now())
		if err != nil {
			if saveErr := m.store.MarkProviderError(slug, "Provider scan could not be committed", time.Time{}); saveErr != nil {
				return saveErr
			}
			m.logger.Error("Provider scan could not be committed", "cycle_id", cycle, "provider", slug)
			return fmt.Errorf("provider %s scan could not be committed: %w", slug, err)
		}
		if !detailNotBefore.IsZero() {
			if err := m.store.MarkProviderError(slug, "Event detail verification deferred by upstream Retry-After", detailNotBefore); err != nil {
				return err
			}
			m.logger.Warn("Provider polling paused by upstream Retry-After", "cycle_id", cycle, "provider", slug, "retry_at", detailNotBefore, "delay", detailNotBefore.Sub(m.now()))
		}
		for _, detection := range detections {
			m.logger.Info("Event discovered or revised", "cycle_id", cycle, "provider", slug, "event_id", detection.EventID, "revision", detection.Revision, "queued", detection.Queued)
			if detection.Queued == 0 {
				m.logger.Debug("Event produced no new notification", "cycle_id", cycle, "provider", slug, "event_id", detection.EventID, "reason", detection.Reason)
			}
		}
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
		// A scan may have canceled or updated an entry after Due's snapshot.
		delivery, found, err := m.store.Delivery(queued.Notification.ID)
		if err != nil {
			return err
		}
		if !found || delivery.Status != "pending" {
			continue
		}
		m.logger.Info("Notification attempt", "channel", channel, "notification_id", delivery.Notification.ID, "event_id", delivery.Notification.Event.ID, "attempt", delivery.Attempts+1)
		result := m.sender.Send(ctx, channel, delivery.Notification)
		if ctx.Err() != nil {
			return nil
		} // Preserve pending work on shutdown.
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
