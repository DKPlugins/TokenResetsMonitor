//go:build !windows

// Package platform integrates the foreground monitor with operating system services.
package platform

import (
	"context"
	"errors"
)

func Run(_ context.Context, _ func(context.Context) error) (bool, error) {
	return false, nil
}

func Service(_ string, _ string, _ string) error {
	return errors.New("native service commands are available on Windows; on Linux use the supplied systemd unit")
}
