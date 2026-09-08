//go:build windows

package platform

import (
	"context"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

func TestServiceStopCancelsMonitorBeforeReportingStopped(t *testing.T) {
	requests := make(chan svc.ChangeRequest)
	statuses := make(chan svc.Status, 8)
	canceled := make(chan struct{})
	completed := make(chan uint32, 1)
	h := handler{parent: context.Background(), run: func(ctx context.Context) error {
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}}
	go func() {
		_, code := h.Execute(nil, requests, statuses)
		completed <- code
	}()
	expectStatus(t, statuses, svc.StartPending)
	expectStatus(t, statuses, svc.Running)
	requests <- svc.ChangeRequest{Cmd: svc.Interrogate}
	expectStatus(t, statuses, svc.Running)
	requests <- svc.ChangeRequest{Cmd: svc.Stop}
	expectStatus(t, statuses, svc.StopPending)
	select {
	case code := <-completed:
		if code != 0 {
			t.Fatalf("normal cancellation returned exit code %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("service did not stop")
	}
	select {
	case <-canceled:
	default:
		t.Fatal("service stopped before the monitor observed cancellation")
	}
}

func expectStatus(t *testing.T, statuses <-chan svc.Status, expected svc.State) {
	t.Helper()
	select {
	case status := <-statuses:
		if status.State != expected {
			t.Fatalf("got state %d, want %d", status.State, expected)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("service did not publish status")
	}
}
