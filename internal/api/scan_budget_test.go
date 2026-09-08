package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
)

// Padding exercises actual response-byte accounting without retaining enormous
// event objects. Every individual response is smaller than the per-page limit.
func scanBudgetPage(endpoint string) []byte {
	u, _ := url.Parse(endpoint)
	page, _ := strconv.Atoi(u.Query().Get("cursor"))
	data := []byte(pageJSON([]model.Event{fixtureEvent("event-" + strconv.Itoa(page))}, strconv.Itoa(page+1)))
	return append(data, bytes.Repeat([]byte(" "), maxResponseBytes-1024-len(data))...)
}

type scanBudgetCache struct{}

func (scanBudgetCache) GetCache(key string) (model.CachedResponse, bool, error) {
	return model.CachedResponse{ETag: "cached-page", Body: scanBudgetPage(key)}, true, nil
}
func (scanBudgetCache) PutCache(string, model.CachedResponse) error { return nil }

func TestScanAggregateBudgetIncludesCachedPages(t *testing.T) {
	for _, cached := range []bool{false, true} {
		name := "network"
		if cached {
			name = "cached-304"
		}
		t.Run(name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if cached {
					if r.Header.Get("If-None-Match") != "cached-page" {
						t.Error("cached request is missing its validator")
					}
					w.WriteHeader(http.StatusNotModified)
					return
				}
				_, _ = w.Write(scanBudgetPage(r.URL.String()))
			}))
			defer server.Close()
			var cache Cache
			if cached {
				cache = scanBudgetCache{}
			}
			events, err := New(server.URL, 10*time.Second, cache).Events(context.Background(), "openai-codex")
			if err == nil || !strings.Contains(err.Error(), "total scan size limit") || events != nil {
				t.Fatalf("aggregate budget returned a partial snapshot: events=%d err=%v", len(events), err)
			}
			if requests != maxScanBytes/(maxResponseBytes-1024)+1 {
				t.Fatalf("scan continued beyond aggregate response budget: %d requests", requests)
			}
		})
	}
}

func TestOversizedCachedResponseRetriesWithoutValidator(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if r.Header.Get("If-None-Match") != "" {
			t.Error("unconditional retry retained the cache validator")
		}
		_, _ = w.Write([]byte(pageJSON([]model.Event{fixtureEvent("fresh")}, "")))
	}))
	defer server.Close()
	body := []byte(pageJSON([]model.Event{fixtureEvent("oversized-cached")}, ""))
	body = append(body, bytes.Repeat([]byte(" "), maxResponseBytes+1-len(body))...)
	cache := memoryCache{server.URL + "/providers/openai-codex/events?limit=100": {ETag: "old", Body: body}}
	events, err := New(server.URL, time.Second, cache).Events(context.Background(), "openai-codex")
	if err != nil || requests != 2 || len(events) != 1 || events[0].ID != "fresh" {
		t.Fatalf("oversized cache bypassed the response limit: events=%+v requests=%d err=%v", events, requests, err)
	}
}

func TestScanBudgetSharedAcrossHistoryAndDetailRequests(t *testing.T) {
	page := []byte(pageJSON([]model.Event{fixtureEvent("shared")}, ""))
	for _, cached := range []bool{false, true} {
		t.Run(strconv.FormatBool(cached), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if cached {
					w.WriteHeader(http.StatusNotModified)
					return
				}
				_, _ = w.Write(page)
			}))
			defer server.Close()
			cache := memoryCache{}
			for _, endpoint := range []string{"/providers/openai-codex/events?limit=100", "/events/shared"} {
				cache[server.URL+endpoint] = model.CachedResponse{ETag: "v1", Body: page}
			}
			var saved Cache
			if cached {
				saved = cache
			}
			client := New(server.URL, time.Second, saved)
			ctx := WithScanBudget(context.Background())
			// A small remainder exercises the same boundary without a second 64 MiB fixture.
			ctx.Value(scanBudgetKey{}).(*scanBudget).remaining = len(page)*2 - 1
			if _, err := client.Events(ctx, "openai-codex"); err != nil {
				t.Fatal(err)
			}
			if _, err := client.Event(WithScanBudget(ctx), "shared"); !errors.Is(err, ErrScanBudget) {
				t.Fatalf("detail escaped shared history budget: %v", err)
			}
		})
	}
}
