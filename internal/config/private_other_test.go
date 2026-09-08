//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestReplacementPreservesServiceOwnershipAndGroupRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := WriteNewBytes(path, []byte("config_version: 1\n")); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(path, 65534, 65534); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(path, true); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode().Perm() != 0640 {
		t.Fatalf("service lost group read access: %o", after.Mode().Perm())
	}
	a, b := before.Sys().(*syscall.Stat_t), after.Sys().(*syscall.Stat_t)
	if a.Uid != b.Uid || a.Gid != b.Gid {
		t.Fatal("service file ownership changed")
	}
}
