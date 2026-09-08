package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
)

type memoryCache map[string]model.CachedResponse

func (m memoryCache) GetCache(key string) (model.CachedResponse, bool, error) {
	value, ok := m[key]
	return value, ok, nil
}
func (m memoryCache) PutCache(key string, value model.CachedResponse) error {
	m[key] = value
	return nil
}

func fixtureEvent(id string) model.Event {
	return model.Event{ID: id, Provider: model.Provider{Slug: "openai-codex", Name: "Codex"}, EventType: "hard_reset", Status: "published", Revision: 2,
		AnnouncedAt: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC), Confidence: model.Confidence{Label: "reported"}, Links: model.Links{HTML: "/events/" + id}}
}
func pageJSON(events []model.Event, cursor string) string {
	data, _ := json.Marshal(events)
	return fmt.Sprintf(`{"data":%s,"pagination":{"has_more":%t,"next_cursor":%q},"meta":{"schema_version":"1.0"}}`, data, cursor != "", cursor)
}

func TestEventsTraversesUnchangedFirstPageAndDiscoversLatePublication(t *testing.T) {
	cache := memoryCache{}
	secondChanged := false
	firstRequests, secondRequests := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/providers/openai-codex/events" || r.URL.Query().Get("limit") != "100" {
			t.Error("wrong provider path or limit")
		}
		if r.URL.Query().Get("cursor") == "" {
			firstRequests++
			w.Header().Set("ETag", `"first"`)
			if r.Header.Get("If-None-Match") == `"first"` {
				w.WriteHeader(304)
				return
			}
			fmt.Fprint(w, pageJSON([]model.Event{fixtureEvent("first")}, "opaque+/=?"))
		} else {
			secondRequests++
			if r.URL.Query().Get("cursor") != "opaque+/=?" {
				t.Error("opaque cursor was modified")
			}
			events := []model.Event{fixtureEvent("old")}
			if secondChanged {
				events = append(events, fixtureEvent("late"))
			}
			fmt.Fprint(w, pageJSON(events, ""))
		}
	}))
	defer server.Close()
	client := New(server.URL+"/api/v1", time.Second, cache)
	first, err := client.Events(context.Background(), "openai-codex")
	if err != nil || len(first) != 2 {
		t.Fatalf("first scan: %v %v", first, err)
	}
	secondChanged = true
	second, err := client.Events(context.Background(), "openai-codex")
	if err != nil || len(second) != 3 || second[2].ID != "late" {
		t.Fatalf("late event lost: %v %v", second, err)
	}
	if firstRequests != 2 || secondRequests != 2 {
		t.Fatalf("not all pages fetched: %d, %d", firstRequests, secondRequests)
	}
	if second[0].Links.HTML != server.URL+"/events/first" {
		t.Fatal("relative event link was not resolved")
	}
}

func Test304WithoutCachedBodyRetriesUnconditionally(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.WriteHeader(304)
			return
		}
		if r.Header.Get("If-None-Match") != "" {
			t.Error("fallback still conditional")
		}
		fmt.Fprint(w, pageJSON([]model.Event{}, ""))
	}))
	defer server.Close()
	key := server.URL + "/providers/openai-codex/events?limit=100"
	client := New(server.URL, time.Second, memoryCache{key: {ETag: `"missing"`}})
	if _, err := client.Events(context.Background(), "openai-codex"); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("got %d requests", requests)
	}
}

func TestIncompleteScanReturnsNoPartialSnapshot(t *testing.T) {
	for _, name := range []string{"wrong_provider", "zero_revision", "missing_date", "missing_id", "new_schema", "missing_pagination", "repeated_cursor", "second_page_failure"} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				e := fixtureEvent("first")
				cursor := ""
				switch name {
				case "wrong_provider":
					e.Provider.Slug = "anthropic-claude"
				case "zero_revision":
					e.Revision = 0
				case "missing_date":
					e.AnnouncedAt = time.Time{}
				case "missing_id":
					e.ID = ""
				case "repeated_cursor":
					cursor = "repeat"
					if r.URL.Query().Get("cursor") != "" {
						e.ID = "second"
					}
				case "second_page_failure":
					cursor = "next"
					if r.URL.Query().Get("cursor") != "" {
						w.WriteHeader(503)
						return
					}
				}
				body := pageJSON([]model.Event{e}, cursor)
				if name == "new_schema" {
					body = strings.ReplaceAll(body, `"1.0"`, `"2.0"`)
				}
				if name == "missing_pagination" {
					body = `{"data":[],"meta":{"schema_version":"1.0"}}`
				}
				fmt.Fprint(w, body)
			}))
			defer server.Close()
			events, err := New(server.URL, time.Second, nil).Events(context.Background(), "openai-codex")
			if err == nil || events != nil {
				t.Fatalf("unsafe partial scan: %+v %v", events, err)
			}
		})
	}
}

func TestErrorRedactionAndRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "123")
		w.WriteHeader(429)
		fmt.Fprint(w, "private endpoint secret-token")
	}))
	defer server.Close()
	_, err := New(server.URL+"/secret-token", time.Second, nil).Catalog(context.Background())
	var apiError *Error
	if !errors.As(err, &apiError) || !apiError.Retryable || apiError.RetryAfter != 123*time.Second {
		t.Fatalf("wrong classification: %v", err)
	}
	if strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), server.URL) {
		t.Fatal("unsafe error")
	}
}

func TestDetailAndConfirmedRetraction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch r.URL.Path {
		case "/providers":
			data = []model.Provider{{Slug: "openai-codex", Name: "Codex"}}
		case "/providers/openai-codex":
			data = model.Provider{Slug: "openai-codex", Name: "Codex", Plans: []model.NamedValue{{Slug: "any-paid", IsWildcard: true}}}
		case "/events/retracted":
			e := fixtureEvent("retracted")
			e.Status = "retracted"
			data = e
		default:
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data, "meta": map[string]string{"schema_version": "1.0"}})
	}))
	defer server.Close()
	client := New(server.URL, time.Second, nil)
	if providers, err := client.Catalog(context.Background()); err != nil || providers[0].Slug != "openai-codex" {
		t.Fatalf("catalog: %v %v", providers, err)
	}
	if detail, err := client.Detail(context.Background(), "openai-codex"); err != nil || !detail.Plans[0].IsWildcard {
		t.Fatalf("detail: %v %v", detail, err)
	}
	if event, err := client.Event(context.Background(), "retracted"); err != nil || event.Status != "retracted" {
		t.Fatalf("retraction: %v %v", event, err)
	}
	_, err := client.Event(context.Background(), "missing")
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 {
		t.Fatalf("404 classification: %v", err)
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://example.invalid/secret")
		w.WriteHeader(302)
	}))
	defer server.Close()
	_, err := New(server.URL, time.Second, nil).Catalog(context.Background())
	var e *Error
	if !errors.As(err, &e) || e.StatusCode != 302 || e.Retryable {
		t.Fatalf("redirect: %v", err)
	}
}

func TestRetryAfterFormats(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	if got := ParseRetryAfter(now.Add(time.Minute).Format(http.TimeFormat), now); got != time.Minute {
		t.Fatal(got)
	}
	for _, value := range []string{"-4", "garbage", ""} {
		if ParseRetryAfter(value, now) != 0 {
			t.Fatal(value)
		}
	}
	if got := ParseRetryAfter("9223372036854775807", now); got != 7*24*time.Hour {
		t.Fatal(got)
	}
}
