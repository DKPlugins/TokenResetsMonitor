//go:build !windows

// Package fileio opens snapshots without blocking replacement or log rotation.
package fileio

import "os"

func OpenSnapshot(path string) (*os.File, error) { return os.Open(path) }
