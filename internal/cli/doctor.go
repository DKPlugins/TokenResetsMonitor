package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/api"
	"github.com/DKPlugins/TokenResetsMonitor/internal/buildinfo"
	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/fileio"
	"github.com/DKPlugins/TokenResetsMonitor/internal/logging"
	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
)

type doctorCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message"`
}
type doctorReport struct {
	Version string        `json:"version"`
	Online  bool          `json:"online"`
	Checks  []doctorCheck `json:"checks"`
}

func doctor(ctx context.Context, cfg config.Config, jsonOutput, online bool, out, errOut io.Writer, configProblems ...error) int {
	report := doctorReport{Version: buildinfo.Version, Online: online}
	failed := false
	add := func(name, status, message string) {
		report.Checks = append(report.Checks, doctorCheck{Name: name, Status: status, Message: message})
		if status == "error" {
			failed = true
		}
	}
	redactor := logging.NewRedactor(cfg)
	configurationOK := true
	for _, err := range configProblems {
		if err != nil {
			add("configuration", "error", redactor.Text(err.Error()))
			configurationOK = false
		}
	}
	if err := config.Validate(cfg); err != nil {
		if configurationOK {
			add("configuration", "error", redactor.Text(err.Error()))
		}
		configurationOK = false
	} else if configurationOK {
		add("configuration", "ok", "Configuration is valid for this binary.")
	}
	manifest := buildinfo.CurrentManifest()
	platform := runtime.GOOS + "/" + runtime.GOARCH
	platformKnown := false
	for _, p := range manifest.Platforms {
		if p == platform {
			platformKnown = true
		}
	}
	if platformKnown {
		add("platform", "ok", "Release binaries support "+platform+".")
	} else {
		add("platform", "warning", "No official release binary is declared for "+platform+".")
	}
	if cfg.StatePath != "" {
		checkDirectory(filepath.Dir(cfg.StatePath), "state_directory", add)
		inspectSnapshot(cfg.StatePath, manifest, add)
	}
	if cfg.Logging.FileEnabled {
		checkDirectory(cfg.Logging.Directory, "logging_directory", add)
	}
	if len(cfg.EnabledChannels()) == 0 {
		add("notifications", "warning", "No notification channel is enabled. Monitoring can run without sending messages.")
	} else {
		add("notifications", "ok", "Enabled destinations are configured. Doctor never sends test notifications.")
	}
	if online && configurationOK {
		checkAPIContract(ctx, cfg, add)
	} else if !online {
		add("api_contract", "skipped", "Offline mode: source API and release servers were not contacted.")
	} else {
		add("api_contract", "skipped", "Correct configuration errors before checking the source API.")
	}
	if jsonOutput {
		if json.NewEncoder(out).Encode(report) != nil {
			return 1
		}
	} else {
		for _, check := range report.Checks {
			fmt.Fprintf(out, "%s: %s — %s\n", check.Name, check.Status, check.Message)
		}
		fmt.Fprintln(out, "Checks are read-only. Directory inspection does not prove write access for a different service account.")
	}
	if failed {
		return 1
	}
	return 0
}

func checkDirectory(path, name string, add func(string, string, string)) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		add(name, "warning", "Directory does not exist yet; the daemon will need permission to create it.")
		return
	}
	if err != nil || !info.IsDir() {
		add(name, "error", "Directory is inaccessible or is not a directory.")
		return
	}
	add(name, "ok", "Directory exists. No permission-changing or write test was performed.")
}

func inspectSnapshot(statePath string, manifest buildinfo.Manifest, add func(string, string, string)) {
	file, err := fileio.OpenSnapshot(statePath + ".status.json")
	if err != nil {
		add("state_schema", "warning", "No readable daemon snapshot; state schema is unknown. Start the daemon or inspect a stopped backup.")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		add("state_schema", "error", "Status snapshot is unreadable or exceeds its size limit.")
		return
	}
	var snapshot struct {
		model.Status
		Runtime struct {
			ConfigVersion      int `json:"config_version"`
			StateSchemaVersion int `json:"state_schema_version"`
		} `json:"runtime"`
	}
	if json.Unmarshal(data, &snapshot) != nil {
		add("state_schema", "error", "Status snapshot is invalid JSON.")
		return
	}
	age := time.Since(snapshot.UpdatedAt)
	if age > time.Minute || age < -5*time.Second || !snapshot.Running {
		add("daemon", "warning", "Daemon snapshot is stopped, stale, or has an invalid timestamp; it does not prove current compatibility.")
	} else {
		add("daemon", "ok", "The daemon has a recent running snapshot.")
	}
	if snapshot.Runtime.StateSchemaVersion == 0 {
		add("state_schema", "warning", "This snapshot does not declare the state schema. No database lock or migration was attempted.")
		return
	}
	if snapshot.Runtime.StateSchemaVersion > manifest.State.Current || snapshot.Runtime.StateSchemaVersion < manifest.State.Minimum {
		add("state_schema", "error", "The snapshot declares a state schema this binary cannot read.")
		return
	}
	if age > time.Minute || age < -5*time.Second || !snapshot.Running {
		add("state_schema", "warning", "The last recorded state schema is supported, but current database compatibility is unverified.")
		return
	}
	add("state_schema", "ok", "The running daemon declares a supported state schema. Its database was not opened.")
}

func checkAPIContract(ctx context.Context, cfg config.Config, add func(string, string, string)) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	timeout := cfg.HTTPTimeout()
	if timeout > 15*time.Second {
		timeout = 15 * time.Second
	}
	client := api.New(cfg.APIBaseURL, timeout, nil)
	catalog, err := client.Catalog(ctx)
	if err != nil {
		add("api_contract", "error", "Source API catalog could not be read with the supported schema; check connectivity, API URL, and source status.")
		return
	}
	known := map[string]bool{}
	for _, provider := range catalog {
		known[provider.Slug] = true
	}
	for _, wanted := range cfg.Providers {
		if !known[wanted.Slug] {
			add("api_contract", "error", "A configured provider is absent from the source catalog.")
			return
		}
		detail, err := client.Detail(ctx, wanted.Slug)
		if err != nil {
			add("api_contract", "error", "A configured provider's scope catalog could not be validated.")
			return
		}
		if _, err := client.Events(ctx, wanted.Slug); err != nil {
			add("api_contract", "error", "A configured provider event endpoint does not satisfy the supported payload/pagination contract or could not be reached.")
			return
		}
		for _, dimension := range []struct {
			chosen []string
			known  []model.NamedValue
		}{{wanted.Plans, detail.Plans}, {wanted.Products, detail.Products}, {wanted.Windows, detail.Windows}} {
			values := map[string]bool{}
			for _, value := range dimension.known {
				values[value.Slug] = true
			}
			for _, chosen := range dimension.chosen {
				if !values[chosen] {
					add("api_contract", "warning", "A configured scope filter is absent from the current source catalog; review provider filters.")
					return
				}
			}
		}
	}
	add("api_contract", "ok", "The source accepts the supported API schema, configured providers, and complete event pagination. No history, delivery, or API cache was written.")
}
