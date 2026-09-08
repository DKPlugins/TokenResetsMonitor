package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/DKPlugins/TokenResetsMonitor/internal/buildinfo"
	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/updates"
)

func checkUpdate(ctx context.Context, cfg config.Config, jsonOutput bool, out, errOut io.Writer) int {
	result, err := updates.New(buildinfo.Version, cfg.Updates.IncludePrerelease).Check(ctx)
	if jsonOutput {
		response := struct {
			updates.Result
			Error string `json:"error,omitempty"`
		}{Result: result}
		if err != nil {
			response.Error = err.Error()
		}
		if json.NewEncoder(out).Encode(response) != nil {
			return 1
		}
	} else {
		if result.LatestVersion != "" {
			fmt.Fprintf(out, "Current: %s | Latest: %s | Update available: %t\n%s\nCompatibility: %s\n", result.CurrentVersion, result.LatestVersion, result.UpdateAvailable, result.ReleaseURL, result.Compatibility)
		}
		for _, note := range result.Notes {
			fmt.Fprintln(out, note)
		}
		if err != nil {
			fmt.Fprintln(errOut, err)
		}
	}
	if err != nil {
		return 1
	}
	return 0
}

func writeManifest(out io.Writer) int {
	if _, err := updates.Compare(buildinfo.Version, buildinfo.Version); err != nil {
		return 1
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if encoder.Encode(buildinfo.CurrentManifest()) != nil {
		return 1
	}
	return 0
}
