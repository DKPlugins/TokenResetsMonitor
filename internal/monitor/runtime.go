package monitor

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/api"
	"github.com/DKPlugins/TokenResetsMonitor/internal/buildinfo"
	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/control"
	"github.com/DKPlugins/TokenResetsMonitor/internal/fileio"
	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
	"github.com/DKPlugins/TokenResetsMonitor/internal/notify"
	"github.com/DKPlugins/TokenResetsMonitor/internal/observability"
	"github.com/DKPlugins/TokenResetsMonitor/internal/state"
	"github.com/DKPlugins/TokenResetsMonitor/internal/updates"
)

// RunOptions carries launch-time inputs, never reloadable infrastructure.
type RunOptions struct {
	ConfigPath string
	Overrides  map[string]string
	OnReload   func(config.Config) error
	// A supplied registry survives every configuration generation.
	Metrics       *observability.Metrics
	WatchInterval time.Duration
}

type generation struct {
	stop       chan struct{}
	cancelScan context.CancelFunc
	done       chan struct{}
	once       sync.Once
}

func (g *generation) drain() { g.once.Do(func() { close(g.stop); g.cancelScan() }) }
func startGeneration(ctx context.Context, cfg config.Config, m *monitor, fatal chan<- error) *generation {
	scanCtx, cancel := context.WithCancel(ctx)
	g := &generation{stop: make(chan struct{}), cancelScan: cancel, done: make(chan struct{})}
	m.stop = g.stop
	var wg sync.WaitGroup
	report := func(err error) {
		select {
		case fatal <- err:
		default:
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			if err := m.poll(scanCtx); err != nil && !recoverablePoll(err) && scanCtx.Err() == nil {
				report(err)
				return
			}
			timer := time.NewTimer(cfg.PollDuration())
			select {
			case <-scanCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	for channel := range m.policy.Channels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-g.stop:
					return
				case <-ctx.Done():
					return
				default:
				}
				if err := m.deliver(ctx, channel, false); err != nil {
					report(err)
					return
				}
				select {
				case <-g.stop:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	go func() { wg.Wait(); close(g.done) }()
	return g
}

type candidate struct {
	cfg config.Config
	err error
}

func readCandidate(path string, overrides map[string]string) (candidate, [32]byte) {
	var hash [32]byte
	f, err := fileio.OpenSnapshot(path)
	if err != nil {
		return candidate{err: errors.New("cannot read configuration")}, hash
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return candidate{err: errors.New("configuration is unreadable or exceeds 1 MiB")}, hash
	}
	hash = sha256.Sum256(raw)
	cfg, err := config.LoadBytes(raw, path, overrides)
	if err == nil {
		err = config.Validate(cfg)
	}
	return candidate{cfg: cfg, err: err}, hash
}

// stableCandidate requires the same bytes and validation outcome in two reads
// separated by a quiet period. Changes during that period restart validation;
// callers run it asynchronously so draining never blocks heartbeat or shutdown.
func stableCandidate(ctx context.Context, interval time.Duration, read func() (candidate, [32]byte)) (candidate, bool) {
	current, hash := read()
	message := func(c candidate) string {
		if c.err != nil {
			return c.err.Error()
		}
		return ""
	}
	for {
		timer := time.NewTimer(max(time.Nanosecond, interval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return candidate{}, false
		case <-timer.C:
		}
		next, again := read()
		if ctx.Err() != nil {
			return candidate{}, false
		}
		if hash == again && message(current) == message(next) {
			return next, true
		}
		current, hash = next, again
	}
}

func watchConfig(ctx context.Context, path string, overrides map[string]string, interval time.Duration, out chan candidate) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var previous [32]byte
	var previousError string
	sent := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		first, hash := readCandidate(path, overrides)
		message := ""
		if first.err != nil {
			message = first.err.Error()
		}
		if sent && hash == previous && message == previousError {
			continue
		}
		settle := time.NewTimer(min(250*time.Millisecond, interval/2))
		select {
		case <-ctx.Done():
			settle.Stop()
			return
		case <-settle.C:
		}
		next, again := readCandidate(path, overrides)
		nextMessage := ""
		if next.err != nil {
			nextMessage = next.err.Error()
		}
		if hash != again || message != nextMessage {
			continue
		}
		previous, previousError, sent = hash, message, true
		select {
		case <-out:
		default:
		}
		select {
		case out <- next:
		case <-ctx.Done():
			return
		}
	}
}

func restartRequired(old, next config.Config) bool {
	a, b := old.Logging, next.Logging
	a.Level = ""
	b.Level = ""
	return old.StatePath != next.StatePath || old.APIBaseURL != next.APIBaseURL || old.Observability != next.Observability || a != b
}

func RunWithOptions(parent context.Context, cfg config.Config, logger *slog.Logger, once bool, opts RunOptions) error {
	if logger == nil {
		logger = slog.Default()
	}
	if err := config.Validate(cfg); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	db, err := state.Open(cfg.StatePath)
	if err != nil {
		return err
	}
	defer db.Close()
	metrics := opts.Metrics
	if metrics == nil {
		metrics = observability.New()
	}
	fatal := make(chan error, 16)
	if cfg.Observability.Enabled && !once {
		listener, err := net.Listen("tcp", cfg.Observability.Listen)
		if err != nil {
			return errors.New("cannot listen on observability address")
		}
		server := &http.Server{Handler: metrics.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
		go func() {
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				select {
				case fatal <- errors.New("observability server failed"):
				case <-ctx.Done():
				}
			}
		}()
		defer func() {
			shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = server.Shutdown(shutdown)
			_ = server.Close()
		}()
	}
	active := cfg
	runtime := model.RuntimeStatus{Version: buildinfo.Version, Commit: buildinfo.Commit, ConfigVersion: config.Version, StateSchemaVersion: state.SchemaVersion, PollIntervalSeconds: cfg.PollDuration().Seconds(), ConfigGeneration: 1, LastReloadSuccessful: true}
	publish := func(running bool) error {
		if err := db.PublishStatusWithRuntime(running, time.Now(), &runtime); err != nil {
			return err
		}
		snapshot, err := state.ReadStatus(cfg.StatePath)
		if err == nil {
			metrics.SetStatus(snapshot)
		}
		return err
	}
	makeMonitor := func(c config.Config) *monitor {
		return &monitor{store: db, source: api.New(c.APIBaseURL, c.HTTPTimeout(), db), sender: notify.New(c), policy: makePolicy(c), logger: logger, metrics: metrics}
	}
	m := makeMonitor(active)
	if err := db.Reconcile(m.policy); err != nil {
		return err
	}
	if err := publish(true); err != nil {
		return err
	}
	logger.Info("Monitor started", "providers", len(cfg.Providers), "channels", len(cfg.EnabledChannels()), "once", once)
	defer func() { _ = publish(false); logger.Info("Monitor stopped") }()
	if once {
		pollErr := m.poll(ctx)
		var wg sync.WaitGroup
		errs := make(chan error, len(m.policy.Channels))
		for channel := range m.policy.Channels {
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
	current := startGeneration(ctx, active, m, fatal)
	defer func() { cancel(); current.drain(); <-current.done }()
	type controlAnswer struct {
		value any
		err   error
	}
	type controlCall struct {
		ctx     context.Context
		request control.Request
		answer  chan controlAnswer
	}
	commands := make(chan controlCall, 16)
	controller, err := control.Start(cfg.StatePath, func(requestCtx context.Context, request control.Request) (any, error) {
		call := controlCall{requestCtx, request, make(chan controlAnswer, 1)}
		select {
		case <-ctx.Done():
			return nil, errors.New("monitor_stopping")
		case <-requestCtx.Done():
			return nil, errors.New("control_request_timeout")
		case commands <- call:
		}
		select {
		case <-ctx.Done():
			return nil, errors.New("monitor_stopping")
		case <-requestCtx.Done():
			return nil, errors.New("control_request_timeout")
		case result := <-call.answer:
			return result.value, result.err
		}
	})
	if err != nil {
		return err
	}
	defer controller.Close()
	maintenance := time.NewTicker(time.Minute)
	defer maintenance.Stop()
	changes := make(chan candidate, 1)
	watchInterval := opts.WatchInterval
	if watchInterval <= 0 {
		watchInterval = 2 * time.Second
	}
	var auxiliaries sync.WaitGroup
	if opts.ConfigPath != "" {
		auxiliaries.Add(1)
		go func() {
			defer auxiliaries.Done()
			watchConfig(ctx, opts.ConfigPath, opts.Overrides, watchInterval, changes)
		}()
	}
	type updateOutcome struct {
		epoch  uint64
		result updates.Result
		err    error
	}
	updateResults := make(chan updateOutcome, 1)
	var updateCancel context.CancelFunc = func() {}
	var updateEpoch uint64
	checker := updates.New(buildinfo.Version, active.Updates.IncludePrerelease)
	startUpdates := func(c config.Config) {
		updateCancel()
		updateEpoch++
		if !c.Updates.Enabled {
			return
		}
		checkCtx, stop := context.WithCancel(ctx)
		updateCancel = stop
		epoch, client := updateEpoch, checker
		interval, _ := time.ParseDuration(c.Updates.Interval)
		auxiliaries.Add(1)
		go func() {
			defer auxiliaries.Done()
			for {
				result, err := client.Check(checkCtx)
				select {
				case <-checkCtx.Done():
					return
				case updateResults <- updateOutcome{epoch, result, err}:
				}
				timer := time.NewTimer(interval)
				select {
				case <-checkCtx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}()
	}
	startUpdates(active)
	defer func() { cancel(); updateCancel(); auxiliaries.Wait() }()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	var pending *candidate
	var drained <-chan struct{}
	var settled <-chan candidate
	settleCancel := func() {}
	defer func() { settleCancel() }()
	beginSettling := func() {
		settleCancel()
		settleCtx, stop := context.WithCancel(ctx)
		settleCancel = stop
		result := make(chan candidate, 1)
		settled = result
		auxiliaries.Add(1)
		go func() {
			defer auxiliaries.Done()
			latest, ok := stableCandidate(settleCtx, min(250*time.Millisecond, watchInterval/2), func() (candidate, [32]byte) {
				return readCandidate(opts.ConfigPath, opts.Overrides)
			})
			if !ok {
				return
			}
			select {
			case result <- latest:
			case <-settleCtx.Done():
			}
		}()
	}
	reject := func(reason string) {
		runtime.LastReloadSuccessful = false
		runtime.ReloadError = reason
		metrics.Reload(reason)
		logger.Warn("Configuration reload rejected; current settings remain active", "reason", reason)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-fatal:
			return err
		case err := <-controller.Errors():
			return err
		case call := <-commands:
			if call.ctx.Err() != nil {
				continue
			}
			if runtime.ReloadPending && (call.request.Operation == "deliveries.retry" || call.request.Operation == "deliveries.retry-failed") {
				call.answer <- controlAnswer{err: control.ErrBusy}
				continue
			}
			value, err := control.Dispatch(db, m.policy, active.FilterSpec(), call.request)
			if err == nil {
				value, err = control.WithMetadata(value, "running", runtime.ConfigGeneration)
			}
			call.answer <- controlAnswer{value, err}
		case <-maintenance.C:
			if _, err := db.PruneHistory(time.Now(), active.History.RetentionDays, 200); err != nil {
				return err
			}
		case <-heartbeat.C:
			if err := publish(true); err != nil {
				return err
			}
		case outcome := <-updateResults:
			if outcome.epoch != updateEpoch {
				continue
			}
			r := outcome.result
			u := &model.UpdateStatus{CheckedAt: r.CheckedAt, CurrentVersion: r.CurrentVersion, LatestVersion: r.LatestVersion, ReleaseURL: r.ReleaseURL, UpdateAvailable: r.UpdateAvailable, Compatibility: r.Compatibility}
			if outcome.err != nil {
				u.Error = "release_check_failed"
				logger.Warn("Release check unavailable; monitoring continues")
			} else {
				logger.Info("Release check completed", "update_available", r.UpdateAvailable, "compatibility", r.Compatibility)
			}
			runtime.Updates = u
			if err := publish(true); err != nil {
				return err
			}
		case next := <-changes:
			if !active.Reload.Enabled {
				continue
			}
			// A newer watcher result invalidates any final check still settling.
			// The stopped generation remains drained until the latest bytes settle.
			if settled != nil {
				beginSettling()
			}
			if next.err != nil {
				pending = nil
				reject("invalid_config")
				if err := publish(true); err != nil {
					return err
				}
				continue
			}
			if restartRequired(active, next.cfg) {
				pending = nil
				reject("restart_required")
				if err := publish(true); err != nil {
					return err
				}
				continue
			}
			if !runtime.ReloadPending && reflect.DeepEqual(active, next.cfg) {
				if !runtime.LastReloadSuccessful {
					runtime.LastReloadSuccessful = true
					runtime.ReloadError = ""
					metrics.Reload("applied")
					if err := publish(true); err != nil {
						return err
					}
				}
				continue
			}
			pending = &next
			if !runtime.ReloadPending {
				runtime.ReloadPending = true
				current.drain()
				drained = current.done
			}
			if err := publish(true); err != nil {
				return err
			}
		case <-drained:
			drained = nil
			beginSettling()
		case latest := <-settled:
			settled = nil
			settleCancel()
			// Coalesce an already queued newer edit before applying a result.
			select {
			case <-changes:
				beginSettling()
				continue
			default:
			}
			pending = nil
			if latest.err != nil {
				if runtime.ReloadError != "invalid_config" {
					reject("invalid_config")
				}
			} else if restartRequired(active, latest.cfg) {
				if runtime.ReloadError != "restart_required" {
					reject("restart_required")
				}
			} else {
				pending = &latest
			}
			if pending != nil {
				next := pending.cfg
				if err := db.Reconcile(makePolicy(next)); err != nil {
					return err
				}
				if opts.OnReload != nil {
					if err := opts.OnReload(next); err != nil {
						return err
					}
				}
				if active.Updates != next.Updates {
					if active.Updates.IncludePrerelease != next.Updates.IncludePrerelease {
						checker = updates.New(buildinfo.Version, next.Updates.IncludePrerelease)
					}
					startUpdates(next)
				}
				active = next
				runtime.ConfigGeneration++
				runtime.PollIntervalSeconds = active.PollDuration().Seconds()
				runtime.LastReloadSuccessful = true
				runtime.ReloadError = ""
				now := time.Now().UTC()
				runtime.LastReloadAt = &now
				metrics.Reload("applied")
				logger.Info("Configuration applied", "config_generation", runtime.ConfigGeneration)
			}
			pending = nil
			runtime.ReloadPending = false
			if err := publish(true); err != nil {
				return err
			}
			previousCycle := m.cycles.Load()
			m = makeMonitor(active)
			m.cycles.Store(previousCycle)
			current = startGeneration(ctx, active, m, fatal)
		}
	}
}
