package buildinfo_test

import (
	"testing"

	"github.com/DKPlugins/TokenResetsMonitor/internal/buildinfo"
	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/state"
)

func TestManifestTracksActualSchemaWriters(t *testing.T) {
	manifest := buildinfo.CurrentManifest()
	if manifest.Config.Current != config.Version || manifest.State.Current != state.SchemaVersion {
		t.Fatalf("published schema metadata drifted: config=%d/%d state=%d/%d", manifest.Config.Current, config.Version, manifest.State.Current, state.SchemaVersion)
	}
}
