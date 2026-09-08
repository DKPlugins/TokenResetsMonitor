//go:build !windows

package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestClientRefusesSharedDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	server, err := Start(path, func(context.Context, Request) (any, error) { t.Error("unsafe descriptor used"); return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if err := os.Chmod(DescriptorPath(path), 0644); err != nil {
		t.Fatal(err)
	}
	var result any
	err = Call(context.Background(), path, Request{Operation: "history.list"}, &result)
	if err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatalf("shared token file should be rejected, not fall back: %v", err)
	}
}
