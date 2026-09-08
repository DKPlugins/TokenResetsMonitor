//go:build windows

package logging

import (
	"bytes"
	"compress/gzip"
	"io"
	"path/filepath"
	"testing"

	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/fileio"
)

func TestOpenSnapshotDoesNotBlockWindowsRotationOrPruning(t *testing.T) {
	cfg := config.Defaults()
	cfg.Logging.Directory = t.TempDir()
	cfg.Logging.MaxSizeMB = 1
	cfg.Logging.MaxBackups = 1
	rotator, err := NewRotator(cfg.Logging)
	if err != nil {
		t.Fatal(err)
	}
	defer rotator.Close()
	first := bytes.Repeat([]byte("a"), 600000)
	second := bytes.Repeat([]byte("b"), 600000)
	if _, err = rotator.Write(first); err != nil {
		t.Fatal(err)
	}
	active, err := fileio.OpenSnapshot(rotator.path())
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	activeSnapshot := fileSnapshot(active)
	// Hold the reader throughout rename, compression and removal of the original
	// file. Ordinary Windows os.Open blocks the rotation at this point.
	if _, err = rotator.Write(second); err != nil {
		t.Fatalf("live reader blocked rotation: %v", err)
	}
	archives, err := filepath.Glob(filepath.Join(cfg.Logging.Directory, "*.jsonl.gz"))
	if err != nil || len(archives) != 1 {
		t.Fatalf("first rotation did not create its archive: %v %v", archives, err)
	}
	archive, err := fileio.OpenSnapshot(archives[0])
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	archiveSnapshot := fileSnapshot(archive)
	// The next rotation must also be able to prune a compressed archive that an
	// exporter has opened, while both old readers retain their original bytes.
	if _, err = rotator.Write(second); err != nil {
		t.Fatalf("archive reader blocked retention: %v", err)
	}
	data, err := io.ReadAll(activeSnapshot)
	if err != nil || !bytes.Equal(data, first) {
		t.Fatalf("held active snapshot changed after rotation: bytes=%d error=%v", len(data), err)
	}
	gz, err := gzip.NewReader(archiveSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	data, err = io.ReadAll(gz)
	gz.Close()
	if err != nil || !bytes.Equal(data, first) {
		t.Fatalf("held archive snapshot changed after pruning: bytes=%d error=%v", len(data), err)
	}
	active.Close()
	archive.Close()
	archives, err = filepath.Glob(filepath.Join(cfg.Logging.Directory, "*.jsonl.gz"))
	if err != nil || len(archives) != 1 {
		t.Fatalf("archive retention was not restored: %v %v", archives, err)
	}
	current, err := fileio.OpenSnapshot(rotator.path())
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	data, err = io.ReadAll(current)
	if err != nil || !bytes.Equal(data, second) {
		t.Fatalf("new log writes failed: bytes=%d error=%v", len(data), err)
	}
}
