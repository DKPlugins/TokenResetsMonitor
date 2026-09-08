package observability

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
)

func healthySnapshot(now time.Time) model.Status {
	success := now
	return model.Status{Running: true, UpdatedAt: now, Providers: map[string]model.ProviderStatus{"openai-codex": {Ready: true, LastSuccess: &success}}, Channels: map[string]model.ChannelStatus{}, Runtime: &model.RuntimeStatus{PollIntervalSeconds: 60, LastReloadSuccessful: true}}
}

func TestHealthDistinguishesProcessSourceAndRecipientFailures(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		mutate      func(*model.Status)
		live, ready bool
		reason      string
	}{
		{"healthy", func(*model.Status) {}, true, true, ""},
		{"stopped", func(s *model.Status) { s.Running = false }, false, false, "not_running"},
		{"stale-heartbeat", func(s *model.Status) { s.UpdatedAt = now.Add(-61 * time.Second) }, false, false, "stale_heartbeat"},
		{"future-heartbeat", func(s *model.Status) { s.UpdatedAt = now.Add(6 * time.Second) }, false, false, "future_heartbeat"},
		{"source-error", func(s *model.Status) {
			p := s.Providers["openai-codex"]
			p.LastError = "upstream-private-secret"
			s.Providers["openai-codex"] = p
		}, true, false, "provider_not_ready"},
		{"initial-baseline", func(s *model.Status) { s.Providers["openai-codex"] = model.ProviderStatus{} }, true, false, "provider_not_ready"},
		{"stale-source", func(s *model.Status) {
			old := now.Add(-8 * time.Minute)
			p := s.Providers["openai-codex"]
			p.LastSuccess = &old
			s.Providers["openai-codex"] = p
		}, true, false, "provider_not_ready"},
		{"recipient-failure", func(s *model.Status) { s.Failed = 50; s.Channels["telegram"] = model.ChannelStatus{Failed: 50} }, true, true, ""},
		{"no-providers", func(s *model.Status) { s.Providers = map[string]model.ProviderStatus{} }, true, false, "no_active_providers"},
		{"missing-runtime", func(s *model.Status) { s.Runtime = nil }, true, false, "provider_not_ready"},
		{"large-valid-interval", func(s *model.Status) { s.Runtime.PollIntervalSeconds = (250 * 365 * 24 * time.Hour).Seconds() }, true, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := healthySnapshot(now)
			tc.mutate(&s)
			live, ready := Health(s, now, false), Health(s, now, true)
			if live.Healthy != tc.live || ready.Healthy != tc.ready {
				t.Fatalf("incorrect health classification: live=%+v ready=%+v", live, ready)
			}
			encoded, _ := json.Marshal(ready)
			if tc.reason != "" && !strings.Contains(string(encoded), tc.reason) {
				t.Fatalf("missing safe diagnostic code %q: %s", tc.reason, encoded)
			}
			if strings.Contains(string(encoded), "private-secret") {
				t.Fatal("health exposed upstream error text")
			}
		})
	}
	t.Run("sequential-provider-budget", func(t *testing.T) {
		s := healthySnapshot(now)
		success := now.Add(-20 * time.Minute)
		s.Providers = map[string]model.ProviderStatus{}
		for _, name := range []string{"one", "two", "three", "four", "five"} {
			s.Providers[name] = model.ProviderStatus{Ready: true, LastSuccess: &success}
		}
		if !Health(s, now, true).Healthy {
			t.Fatal("healthy sequential scans were rejected before their bounded cycle could finish")
		}
	})
}

func metricValue(t *testing.T, m *Metrics, name string, wanted map[string]string) (float64, bool) {
	t.Helper()
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			match := true
			for key, value := range wanted {
				if labels[key] != value {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			if metric.Counter != nil {
				return metric.Counter.GetValue(), true
			}
			if metric.Gauge != nil {
				return metric.Gauge.GetValue(), true
			}
			if metric.Histogram != nil {
				return float64(metric.Histogram.GetSampleCount()), true
			}
		}
	}
	return 0, false
}

func TestMetricsCountersSurviveSnapshotsWithoutLeakingSecrets(t *testing.T) {
	m := New()
	m.ObserveScan("openai-codex", time.Second, true)
	m.ObserveScan("openai-codex", 2*time.Second, false)
	m.ObserveDelivery("webhook", model.DeliveryResult{Success: true, Duration: time.Second})
	m.ObserveDelivery("telegram", model.DeliveryResult{Retryable: true, StatusCode: 429, Error: "telegram-token-secret", Duration: 2 * time.Second})
	m.Reload("invalid_config")
	now := time.Now()
	s := healthySnapshot(now)
	oldest := now.Add(-time.Minute)
	s.Channels = map[string]model.ChannelStatus{"telegram": {Pending: 2, Failed: 1, OldestPending: &oldest}}
	s.Runtime.ReloadError = "configuration-secret"
	s.Runtime.Updates = &model.UpdateStatus{CheckedAt: now, UpdateAvailable: true, Error: "release-private-url"}
	m.SetStatus(s)
	for _, check := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"tokenresetsmonitor_provider_scans_total", map[string]string{"provider": "openai-codex", "result": "success"}, 1},
		{"tokenresetsmonitor_provider_scans_total", map[string]string{"provider": "openai-codex", "result": "error"}, 1},
		{"tokenresetsmonitor_delivery_attempts_total", map[string]string{"channel": "telegram", "result": "retryable_error"}, 1},
		{"tokenresetsmonitor_delivery_duration_seconds", map[string]string{"channel": "telegram"}, 1},
		{"tokenresetsmonitor_deliveries", map[string]string{"channel": "telegram", "status": "pending"}, 2},
		{"tokenresetsmonitor_config_reloads_total", map[string]string{"result": "invalid_config"}, 1},
		{"tokenresetsmonitor_update_check_success", nil, 0},
	} {
		if got, found := metricValue(t, m, check.name, check.labels); !found || got != check.want {
			t.Fatalf("%s %v = %v, found=%v; want %v", check.name, check.labels, got, found, check.want)
		}
	}
	age, found := metricValue(t, m, "tokenresetsmonitor_oldest_pending_age_seconds", map[string]string{"channel": "telegram"})
	if !found || age < 60 || age > 65 {
		t.Fatal("oldest pending age is incorrect", age, found)
	}
	s = healthySnapshot(time.Now())
	s.Channels = map[string]model.ChannelStatus{"slack": {Pending: 1}}
	m.SetStatus(s)
	if _, found := metricValue(t, m, "tokenresetsmonitor_deliveries", map[string]string{"channel": "telegram"}); found {
		t.Fatal("snapshot retained a removed channel gauge")
	}
	if got, found := metricValue(t, m, "tokenresetsmonitor_delivery_attempts_total", map[string]string{"channel": "telegram", "result": "retryable_error"}); !found || got != 1 {
		t.Fatal("configuration snapshot reset delivery counters", got, found)
	}
	recorder := httptest.NewRecorder()
	m.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatal("metrics endpoint failed", recorder.Code, recorder.Body.String())
	}
	for _, secret := range []string{"telegram-token-secret", "configuration-secret", "release-private-url"} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Fatal("metrics exposed diagnostic details", secret)
		}
	}
}

func TestHealthHTTPMethodsAndSafeResponses(t *testing.T) {
	m := New()
	s := healthySnapshot(time.Now())
	p := s.Providers["openai-codex"]
	p.LastError = "private-upstream-url"
	s.Providers["openai-codex"] = p
	m.SetStatus(s)
	handler := m.Handler()
	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{http.MethodGet, "/livez", 200}, {http.MethodGet, "/readyz", 503}, {http.MethodHead, "/readyz", 503}, {http.MethodPost, "/metrics", 405}, {http.MethodGet, "/missing", 404},
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
		if recorder.Code != tc.code {
			t.Fatalf("%s %s returned %d, want %d", tc.method, tc.path, recorder.Code, tc.code)
		}
		if tc.method == http.MethodHead && recorder.Body.Len() != 0 {
			t.Fatal("HEAD returned a response body")
		}
		if strings.Contains(recorder.Body.String(), "private-upstream-url") {
			t.Fatal("HTTP health exposed source errors")
		}
		if tc.code == 405 && recorder.Header().Get("Allow") != "GET, HEAD" {
			t.Fatal("unsupported method omitted the allowed methods")
		}
	}
}

func TestConcurrentSnapshotsAndScrapes(t *testing.T) {
	m := New()
	handler := m.Handler()
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for i := 0; i < 30; i++ {
			m.SetStatus(healthySnapshot(time.Now()))
			m.ObserveScan("openai-codex", time.Millisecond, true)
		}
	}()
	go func() {
		defer workers.Done()
		for i := 0; i < 30; i++ {
			r := httptest.NewRecorder()
			handler.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			if r.Code != 200 {
				t.Errorf("concurrent scrape failed: %d", r.Code)
			}
		}
	}()
	workers.Wait()
	value, found := metricValue(t, m, "tokenresetsmonitor_provider_scans_total", map[string]string{"provider": "openai-codex", "result": "success"})
	if !found || value != 30 {
		t.Fatal("concurrent publication lost scan counters", value, found)
	}
}
