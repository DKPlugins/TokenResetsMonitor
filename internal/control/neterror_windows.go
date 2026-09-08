//go:build windows

package control

import (
	"errors"
	"golang.org/x/sys/windows"
)

// Winsock errors are distinct from Go's synthetic syscall.ECONNREFUSED on Windows.
func connectionRefused(err error) bool { return errors.Is(err, windows.WSAECONNREFUSED) }
