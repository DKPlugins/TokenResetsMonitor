package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/observability"
	"github.com/DKPlugins/TokenResetsMonitor/internal/state"
)

func healthcheck(opts options, out, errOut io.Writer) int {
	path := opts.statePath
	if path == "" {
		path = os.Getenv("TRM_STATE_PATH")
	}
	if path == "" {
		path = config.Defaults().StatePath
	}
	snapshot, err := state.ReadStatus(path)
	if err != nil {
		fmt.Fprintln(errOut, "Health unavailable: missing or unreadable status snapshot; pass --state-path to the daemon's database.")
		return 1
	}
	result := observability.Health(snapshot, time.Now(), opts.ready)
	if opts.json {
		_ = json.NewEncoder(out).Encode(result)
	} else if result.Healthy {
		fmt.Fprintln(out, "healthy")
	} else {
		fmt.Fprintln(errOut, "unhealthy:", result.Reasons)
	}
	if result.Healthy {
		return 0
	}
	return 1
}
func loadOperational(opts options, overrides map[string]string) (config.Config, error) {
	cfg, err := config.LoadStructural(opts.configPath, overrides)
	if err != nil {
		path := opts.statePath
		if path == "" {
			path = os.Getenv("TRM_STATE_PATH")
		}
		if path == "" {
			return cfg, errors.New("Cannot load diagnostic paths; pass --state-path explicitly (and --log-directory or --input for logs).")
		}
		cfg = config.Defaults()
		cfg.StatePath = path
	}
	if opts.logDirectory != "" {
		cfg.Logging.Directory = opts.logDirectory
		cfg.Logging.FileEnabled = true
	}
	cfg.StatePath, err = filepath.Abs(cfg.StatePath)
	if err != nil {
		return cfg, errors.New("invalid state path")
	}
	return cfg, nil
}

func printCommandHelp(command, sub string, out io.Writer) {
	usage := map[string]string{
		"init":              "init [--defaults] --config PATH\nCreate configuration with a guided setup, or write defaults without network access.",
		"setup":             "setup telegram|slack --config PATH\nConnect a destination, confirm it, optionally send TEST, and save just that channel.",
		"run":               "run [--once] --config PATH\nPoll immediately; continuous runs reload working settings automatically. Ctrl+C stops safely.",
		"config":            "config validate [--structural] --config PATH | config migrate [--apply] --config PATH\nValidate without sending. --structural defers missing service environment references; startup always validates fully. Migration previews unless --apply is supplied.",
		"status":            "status [--json] [--config PATH] [--state-path PATH]\nRead the daemon snapshot without opening its database or requiring channel secrets.",
		"healthcheck":       "healthcheck [--ready] [--json] --state-path PATH\nExit 0 for healthy, 1 for unhealthy. Does not load YAML or contact external services.",
		"test-notification": "test-notification webhook|telegram|slack|all [--dry-run] [--provider SLUG] --config PATH\nSends a marked TEST once. --dry-run only renders a redacted request summary.",
		"check-update":      "check-update [--json] [--include-prerelease] [--config PATH]\nCheck published releases and declared compatibility. Never installs anything.",
		"doctor":            "doctor [--offline] [--json] [--config PATH] [--state-path PATH]\nCheck configuration, local paths and source API. --offline disables network checks.",
		"logs":              "logs export [--since 24h] [--output diagnostics.zip] [--input FILE|-] [--log-directory PATH]\nExport redacted diagnostics locally. Existing archives are never overwritten.",
		"history":           "history list [--provider SLUG] [--since 24h] [--until RFC3339] [--limit 50] [--cursor CURSOR] [--json]\nhistory show|explain --id EVENT_ID [--provider SLUG] [--json]\nRead recorded events and decisions; explain also evaluates active filters. Stopped queries never migrate state.",
		"filters":           "filters preview --candidate-config PATH [--provider SLUG] [--limit 50] [--cursor CURSOR] [--json]\nCompare active and candidate filters on stored real events without changing configuration or sending messages.",
		"deliveries":        "deliveries list [--event-id ID] [--provider SLUG] [--channel CHANNEL] [--status STATUS] [--since 24h] [--until RFC3339] [--limit 50] [--cursor CURSOR] [--json]\ndeliveries show --id ID [--limit 50] [--cursor CURSOR] [--json]\ndeliveries retry --id ID | deliveries retry-failed [--json]\nInspect attempts and retry eligible failures while the monitor keeps running. Stopped retries queue work for the next run.",
		"service":           "service install|start|stop|status|uninstall [--config PATH]\nManage the Windows service from an Administrator terminal; install uses saved settings.",
	}
	if text, ok := usage[command]; ok {
		fmt.Fprintln(out, "Usage: tokenresetsmonitor "+text)
		fmt.Fprintln(out, "Common options: --config PATH --state-path PATH --poll-interval 30m --log-level info")
	} else {
		fmt.Fprint(out, help)
	}
}
