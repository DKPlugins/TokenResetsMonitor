//go:build windows

// Package fileio opens snapshots without blocking replacement or log rotation.
package fileio

import (
	"os"

	"golang.org/x/sys/windows"
)

// OpenSnapshot permits the writer to replace, rename, or remove the path while
// this reader retains access to the opened file. The caller owns the handle.
func OpenSnapshot(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}
