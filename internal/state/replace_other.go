//go:build !windows

package state

import "os"

func replaceStatusFile(source, target string) error { return os.Rename(source, target) }
