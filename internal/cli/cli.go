// Package cli implements user-facing commands. Auxiliary commands never open the daemon log writer.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/buildinfo"
	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/logging"
	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
	"github.com/DKPlugins/TokenResetsMonitor/internal/monitor"
	"github.com/DKPlugins/TokenResetsMonitor/internal/notify"
	"github.com/DKPlugins/TokenResetsMonitor/internal/observability"
	"github.com/DKPlugins/TokenResetsMonitor/internal/platform"
	"github.com/DKPlugins/TokenResetsMonitor/internal/state"
)

const help = `TokenResetsMonitor - public AI reset announcement notifications

Usage: tokenresetsmonitor <command> [options]

  init                         Interactive configuration wizard
  init --defaults              Write defaults without prompting
  setup telegram|slack        Connect a notification destination
  config validate              Validate configuration
  config migrate [--apply]     Preview/apply a configuration migration
  run [--once]                 Monitor continuously or run one cycle
  status                       Show the latest daemon status snapshot
  test-notification <channel>  Send a TEST notification (webhook|telegram|slack|all)
  logs export                  Export redacted diagnostics locally
  history list|show|explain    Inspect real events and notification decisions
  filters preview             Compare candidate filters without sending
  deliveries list|show|retry   Inspect deliveries or retry a failed delivery
  deliveries retry-failed      Requeue eligible failures, including while running
  service <action>             Windows service install/start/stop/status/uninstall
  healthcheck [--ready]        Check local daemon health without secrets
  check-update                 Check releases and declared compatibility
  doctor [--offline]           Diagnose configuration, state and source API
  version                      Show build information

Common options: --config PATH --state-path PATH --poll-interval 30m --log-level info
test-notification: --dry-run --provider SLUG
logs export: --since 24h --output diagnostics.zip [--input FILE|-]
Use README.md for configuration, service installation and upgrade instructions.
`

type options struct {
	configPath        string
	statePath         string
	pollInterval      string
	logLevel          string
	once              bool
	dryRun            bool
	defaults          bool
	apply             bool
	provider          string
	since             string
	output            string
	input             string
	json              bool
	ready             bool
	offline           bool
	includePrerelease bool
	structural        bool
	logDirectory      string
	recordID          string
	channel           string
	status            string
	eventID           string
	limit             int
	cursor            string
	candidatePath     string
	until             string
}

func Execute(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(out, help)
		return 0
	}
	if args[0] == "compatibility-manifest" {
		if len(args) != 1 {
			return 2
		}
		return writeManifest(out)
	}
	if args[0] == "version" || args[0] == "--version" {
		if len(args) == 2 && (args[1] == "--help" || args[1] == "-h") {
			fmt.Fprintln(out, "Usage: tokenresetsmonitor version [--json]\nShow application version, commit and compatibility schema information.")
			return 0
		}
		if len(args) > 2 || (len(args) == 2 && args[1] != "--json") {
			fmt.Fprintln(errOut, "Invalid version arguments; use version [--json].")
			return 2
		}
		if len(args) > 1 && args[1] == "--json" {
			_ = json.NewEncoder(out).Encode(map[string]any{"version": buildinfo.Version, "commit": buildinfo.Commit, "date": buildinfo.Date, "compatibility": buildinfo.CurrentManifest()})
		} else {
			fmt.Fprintf(out, "TokenResetsMonitor %s (%s, %s)\n", buildinfo.Version, buildinfo.Commit, buildinfo.Date)
		}
		return 0
	}
	command := args[0]
	rest := args[1:]
	if len(rest) == 1 && (rest[0] == "--help" || rest[0] == "-h") {
		printCommandHelp(command, "", out)
		return 0
	}
	sub := ""
	switch command {
	case "config", "test-notification", "logs", "deliveries", "history", "filters", "service", "setup":
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			fmt.Fprintln(errOut, "A subcommand/channel is required.")
			return 2
		}
		sub = rest[0]
		rest = rest[1:]
	}
	opts := options{}
	if (command == "history" && (sub == "show" || sub == "explain")) || (command == "deliveries" && (sub == "show" || sub == "retry")) {
		if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
			opts.recordID, rest = rest[0], rest[1:]
		}
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := config.DefaultPath()
	if v := os.Getenv("TRM_CONFIG"); v != "" {
		path = v
	}
	fs.StringVar(&opts.configPath, "config", path, "configuration path")
	fs.StringVar(&opts.statePath, "state-path", "", "state database path")
	fs.StringVar(&opts.pollInterval, "poll-interval", "", "poll interval")
	fs.StringVar(&opts.logLevel, "log-level", "", "log level")
	switch command {
	case "setup":
	case "init":
		fs.BoolVar(&opts.defaults, "defaults", false, "write defaults")
	case "run":
		fs.BoolVar(&opts.once, "once", false, "one scan and delivery pass")
	case "config":
		fs.BoolVar(&opts.apply, "apply", false, "apply migration")
		if sub == "validate" {
			fs.BoolVar(&opts.structural, "structural", false, "defer unavailable service environment references")
		}
	case "test-notification":
		fs.BoolVar(&opts.dryRun, "dry-run", false, "validate without sending")
		fs.StringVar(&opts.provider, "provider", "", "synthetic event provider")
	case "logs":
		fs.StringVar(&opts.logDirectory, "log-directory", "", "log directory when configuration is unavailable")
		fs.StringVar(&opts.since, "since", "24h", "collection period")
		fs.StringVar(&opts.output, "output", "diagnostics.zip", "new archive path")
		fs.StringVar(&opts.input, "input", "", "structured logs file or -")
	case "status", "healthcheck", "check-update", "doctor":
		fs.BoolVar(&opts.json, "json", false, "JSON output")
		if command == "healthcheck" {
			fs.BoolVar(&opts.ready, "ready", false, "require recent successful provider scans")
		}
		if command == "doctor" {
			fs.BoolVar(&opts.offline, "offline", false, "skip all network checks")
		}
		if command == "check-update" {
			fs.BoolVar(&opts.includePrerelease, "include-prerelease", false, "also consider prereleases")
		}

	case "history", "filters", "deliveries":
		fs.BoolVar(&opts.json, "json", false, "JSON output")
		fs.StringVar(&opts.provider, "provider", "", "provider slug")
		fs.StringVar(&opts.eventID, "event", "", "event ID")
		fs.StringVar(&opts.eventID, "event-id", "", "event ID")
		fs.StringVar(&opts.recordID, "id", opts.recordID, "event or delivery ID")
		fs.StringVar(&opts.since, "since", "", "detected/created since duration or RFC3339 timestamp")
		fs.StringVar(&opts.until, "until", "", "detected/created until RFC3339 timestamp")
		fs.StringVar(&opts.channel, "channel", "", "delivery channel")
		fs.StringVar(&opts.status, "status", "", "record status")
		fs.IntVar(&opts.limit, "limit", 50, "page size (1-200)")
		fs.StringVar(&opts.cursor, "cursor", "", "next page cursor")
		if command == "filters" {
			fs.StringVar(&opts.candidatePath, "candidate", "", "candidate filter YAML file")
			fs.StringVar(&opts.candidatePath, "candidate-config", "", "candidate filter YAML file")
		}
	case "service":
	default:
		fmt.Fprintln(errOut, "Unknown command. Use --help.")
		return 2
	}
	if e := fs.Parse(rest); errors.Is(e, flag.ErrHelp) {
		printCommandHelp(command, sub, out)
		return 0
	} else if e != nil {
		fmt.Fprintln(errOut, "Invalid arguments. Use --help.")
		return 2
	}
	if fs.NArg() != 0 {
		needsID := (command == "history" && (sub == "show" || sub == "explain")) || (command == "deliveries" && (sub == "show" || sub == "retry"))
		if !needsID || opts.recordID != "" || fs.NArg() != 1 {
			fmt.Fprintln(errOut, "Invalid arguments. Use --help.")
			return 2
		}
		opts.recordID = fs.Arg(0)
	}
	overrides := map[string]string{}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "state-path":
			overrides["state_path"] = opts.statePath
		case "poll-interval":
			overrides["poll_interval"] = opts.pollInterval
		case "log-level":
			overrides["logging_level"] = opts.logLevel
		}
	})
	if (command == "service" || (command == "config" && sub == "migrate")) && len(overrides) > 0 {
		fmt.Fprintln(errOut, "Runtime overrides are not supported by this command; update the configuration file instead.")
		return 2
	}
	if command == "config" && sub != "migrate" && opts.apply {
		fmt.Fprintln(errOut, "--apply is only valid for config migrate.")
		return 2
	}
	if command == "config" && sub == "validate" && opts.structural {
		return validateInstallation(opts.configPath, overrides, out, errOut)
	}
	if command == "healthcheck" {
		return healthcheck(opts, out, errOut)
	}
	if command == "history" || command == "filters" || command == "deliveries" {
		return management(ctx, command, sub, opts, overrides, out, errOut)
	}
	if command == "setup" {
		if len(overrides) > 0 {
			fmt.Fprintln(errOut, "Setup saves configuration; edit runtime overrides separately.")
			return 2
		}
		if e := setupChannel(ctx, opts, sub, in, out); e != nil {
			fmt.Fprintln(errOut, e)
			return 2
		}
		return 0
	}
	if command == "init" {
		if e := initialize(ctx, opts, overrides, in, out); e != nil {
			fmt.Fprintln(errOut, e)
			return 2
		}
		return 0
	}
	if command == "config" && sub == "migrate" {
		message, e := config.Migrate(opts.configPath, opts.apply)
		if e != nil {
			fmt.Fprintln(errOut, e)
			return 2
		}
		fmt.Fprintln(out, message)
		return 0
	}
	if command == "service" {
		configPath, e := filepath.Abs(opts.configPath)
		if e != nil {
			fmt.Fprintln(errOut, "Invalid configuration path.")
			return 2
		}
		if sub == "install" {
			if code := validateInstallation(configPath, nil, out, errOut); code != 0 {
				return code
			}
		}
		exe, e := os.Executable()
		if e == nil {
			e = platform.Service(sub, configPath, exe)
		}
		if e != nil {
			fmt.Fprintln(errOut, "Service operation failed:", logging.NewRedactor(config.Defaults()).Text(e.Error()))
			return 1
		}
		return 0
	}
	if command == "run" {
		run := func(runCtx context.Context) (runError error) {
			cfg, err := config.Load(opts.configPath, overrides)
			if err != nil {
				return &configError{err}
			}
			if err = config.Validate(cfg); err != nil {
				return &configError{err}
			}
			logger, control, closeFn, err := logging.NewRuntime(cfg, out, errOut)
			if err != nil {
				return err
			}
			defer func() { runError = errors.Join(runError, closeFn()) }()
			logger = logger.With("version", buildinfo.Version, "commit", buildinfo.Commit, "run_id", fmt.Sprintf("%d", time.Now().UnixNano()))
			return monitor.RunWithOptions(runCtx, cfg, logger, opts.once, monitor.RunOptions{ConfigPath: opts.configPath, Overrides: overrides, OnReload: control.Apply})
		}
		handled, err := platform.Run(ctx, run)
		if !handled && err == nil {
			err = run(ctx)
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintln(errOut, "Monitor failed:", err)
			var ce *configError
			if errors.As(err, &ce) {
				return 2
			}
			return 1
		}
		return 0
	}
	var cfg config.Config
	var e error
	operational := command == "status" || command == "logs"
	if operational {
		cfg, e = loadOperational(opts, overrides)
	} else if command == "check-update" {
		cfg, e = config.LoadStructural(opts.configPath, overrides)
	} else {
		cfg, e = config.Load(opts.configPath, overrides)
	}
	if command == "check-update" {
		if e != nil {
			if _, statErr := os.Stat(opts.configPath); errors.Is(statErr, os.ErrNotExist) {
				cfg = config.Defaults()
				e = nil
			}
		}
		if e == nil {
			if opts.includePrerelease {
				cfg.Updates.IncludePrerelease = true
			}
			return checkUpdate(ctx, cfg, opts.json, out, errOut)
		}
	}
	if command == "doctor" {
		loadErr := e
		if e != nil {
			if operationalCfg, opErr := loadOperational(opts, overrides); opErr == nil {
				cfg = operationalCfg
			} else {
				cfg = config.Defaults()
				if opts.statePath != "" {
					cfg.StatePath = opts.statePath
				}
			}
		}
		return doctor(ctx, cfg, opts.json, !opts.offline, out, errOut, loadErr)
	}
	if e != nil {
		fmt.Fprintln(errOut, e)
		return 2
	}
	validation := cfg
	if command == "test-notification" {
		validation.Webhook.Enabled = false
		validation.Telegram.Enabled = false
		validation.Slack.Enabled = false
	}
	if !operational {
		if e = config.Validate(validation); e != nil {
			fmt.Fprintln(errOut, e)
			return 2
		}
	}
	switch command {
	case "config":
		if sub != "validate" {
			fmt.Fprintln(errOut, "Unknown config subcommand.")
			return 2
		}
		fmt.Fprintln(out, "Configuration is valid.")
		return 0
	case "test-notification":
		return testNotification(ctx, cfg, opts, sub, out, errOut)
	case "status":
		status, e := state.ReadStatus(cfg.StatePath)
		if e != nil {
			fmt.Fprintln(errOut, "Status is unavailable; start the monitor first.")
			return 1
		}
		if opts.json {
			_ = json.NewEncoder(out).Encode(struct {
				modelStatus
				Health             observability.HealthResult `json:"health"`
				SnapshotAgeSeconds float64                    `json:"snapshot_age_seconds"`
			}{modelStatus: status, Health: observability.Health(status, time.Now(), true), SnapshotAgeSeconds: time.Since(status.UpdatedAt).Seconds()})
		} else {
			fmt.Fprintf(out, "Running: %t | Pending: %d | Failed: %d | Updated: %s\n", status.Running, status.Pending, status.Failed, status.UpdatedAt.UTC().Format(time.RFC3339))
			if time.Since(status.UpdatedAt) > time.Minute {
				fmt.Fprintln(out, "Status snapshot is stale; the process may have stopped unexpectedly.")
			}
			if r := status.Runtime; r != nil {
				fmt.Fprintf(out, "Configuration: generation=%d reload_pending=%t last_reload_successful=%t\n", r.ConfigGeneration, r.ReloadPending, r.LastReloadSuccessful)
				if r.ReloadError != "" {
					fmt.Fprintln(out, "Reload:", r.ReloadError)
				}
				if u := r.Updates; u != nil {
					if u.Error != "" {
						fmt.Fprintln(out, "Update check unavailable; try check-update for details.")
					} else {
						fmt.Fprintf(out, "Updates: available=%t compatibility=%s checked=%s\n", u.UpdateAvailable, u.Compatibility, u.CheckedAt.Format(time.RFC3339))
					}
				}
			}
			for name, p := range status.Providers {
				last := "never"
				if p.LastSuccess != nil {
					last = p.LastSuccess.UTC().Format(time.RFC3339)
				}
				fmt.Fprintf(out, "%s: ready=%t last_success=%s\n", name, p.Ready, last)
			}
		}
		return 0
	case "logs":
		if sub != "export" {
			fmt.Fprintln(errOut, "Unknown logs subcommand.")
			return 2
		}
		since, e := time.ParseDuration(opts.since)
		if e != nil || since <= 0 {
			fmt.Fprintln(errOut, "--since must be a positive duration, such as 24h.")
			return 2
		}
		if e = logging.Export(cfg, logging.ExportOptions{Since: since, Output: opts.output, Input: opts.input, Stdin: in}); e != nil {
			fmt.Fprintln(errOut, e)
			return 1
		}
		fmt.Fprintln(out, "Diagnostics saved locally; nothing was uploaded.")
		return 0

	}
	return 2
}

type configError struct{ error }

func testNotification(ctx context.Context, cfg config.Config, opts options, channel string, out, errOut io.Writer) int {
	channels := []string{channel}
	if channel == "all" {
		channels = cfg.EnabledChannels()
		if len(channels) == 0 {
			fmt.Fprintln(errOut, "No channels are enabled.")
			return 2
		}
	}
	provider := opts.provider
	if provider == "" {
		provider = cfg.Providers[0].Slug
	}
	n := notify.TestNotification(provider)
	sender := notify.New(cfg)
	exitCode := 0
	for _, c := range channels {
		if e := config.ValidateChannel(cfg, c); e != nil {
			fmt.Fprintln(errOut, c+":", e)
			exitCode = 2
			continue
		}
		preview, e := sender.Preview(c, n)
		if e != nil {
			fmt.Fprintln(errOut, c+":", e)
			exitCode = 2
			continue
		}
		if opts.dryRun {
			_ = json.NewEncoder(out).Encode(preview)
			continue
		}
		result := sender.Send(ctx, c, n)
		if result.Success {
			fmt.Fprintf(out, "%s: TEST delivered (HTTP %d, %s, id=%s)\n", c, result.StatusCode, result.Duration.Round(time.Millisecond), n.ID)
		} else {
			fmt.Fprintf(errOut, "%s: TEST failed (HTTP %d, %s): %s\n", c, result.StatusCode, result.Duration.Round(time.Millisecond), logging.NewRedactor(cfg).Text(result.Error))
			if exitCode == 0 {
				exitCode = 1
			}
		}
	}
	return exitCode
}

// Alias preserves the existing top-level status JSON fields.
type modelStatus = model.Status
