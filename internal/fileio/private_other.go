//go:build !windows

package fileio

import (
	"os"
	"path/filepath"
	"syscall"
)

func CreatePrivate(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
}

// Retain the service's owner/group access when an administrator edits its file.
// Never introduce world access through replacement.
func ReplacePrivate(temp, target string) error {
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if err := os.Chown(temp, int(stat.Uid), int(stat.Gid)); err != nil {
			return err
		}
	}
	if err := os.Chmod(temp, info.Mode().Perm()&0660|0600); err != nil {
		return err
	}
	if err := os.Rename(temp, target); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(target))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
