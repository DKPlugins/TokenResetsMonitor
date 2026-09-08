package api

import (
	"context"
	"errors"
	"sync"
)

// ErrScanBudget means the complete provider operation exceeded its byte limit.
var ErrScanBudget = errors.New("API exceeded total scan size limit")

type scanBudgetKey struct{}
type scanBudget struct {
	mu        sync.Mutex
	remaining int
}

// WithScanBudget shares the 64 MiB limit across pagination and detail lookups.
// Reusing an already bounded context never resets its remaining allowance.
func WithScanBudget(ctx context.Context) context.Context {
	if _, ok := ctx.Value(scanBudgetKey{}).(*scanBudget); ok {
		return ctx
	}
	return context.WithValue(ctx, scanBudgetKey{}, &scanBudget{remaining: maxScanBytes})
}
func chargeScan(ctx context.Context, size int) error {
	b, ok := ctx.Value(scanBudgetKey{}).(*scanBudget)
	if !ok {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if size > b.remaining {
		b.remaining = 0
		return ErrScanBudget
	}
	b.remaining -= size
	return nil
}
