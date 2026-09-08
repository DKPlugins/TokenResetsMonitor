// Package observability exposes cached operational state, never credentials or event bodies.
package observability

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/buildinfo"
	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const prefix = "tokenresetsmonitor_"

type Metrics struct {
	Registry         *prometheus.Registry
	mu               sync.RWMutex
	status           model.Status
	scans            *prometheus.CounterVec
	scanDuration     *prometheus.HistogramVec
	attempts         *prometheus.CounterVec
	deliveryDuration *prometheus.HistogramVec
	providerReady    *prometheus.GaugeVec
	lastSuccess      *prometheus.GaugeVec
	deliveries       *prometheus.GaugeVec
	oldest           *prometheus.GaugeVec
	reloads          *prometheus.CounterVec
	reloadSuccess    prometheus.Gauge
	updateAvailable  prometheus.Gauge
	updateSuccess    prometheus.Gauge
	updateChecked    prometheus.Gauge
}

func New() *Metrics {
	m := &Metrics{Registry: prometheus.NewRegistry()}
	counter := func(name, help string, labels ...string) *prometheus.CounterVec {
		v := prometheus.NewCounterVec(prometheus.CounterOpts{Name: prefix + name, Help: help}, labels)
		m.Registry.MustRegister(v)
		return v
	}
	gauge := func(name, help string, labels ...string) *prometheus.GaugeVec {
		v := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: prefix + name, Help: help}, labels)
		m.Registry.MustRegister(v)
		return v
	}
	histogram := func(name, help string, labels ...string) *prometheus.HistogramVec {
		v := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: prefix + name, Help: help, Buckets: []float64{.05, .1, .5, 1, 5, 15, 30, 60, 300}}, labels)
		m.Registry.MustRegister(v)
		return v
	}
	single := func(name, help string) prometheus.Gauge {
		v := prometheus.NewGauge(prometheus.GaugeOpts{Name: prefix + name, Help: help})
		m.Registry.MustRegister(v)
		return v
	}
	build := gauge("build_info", "Application build information.", "version", "commit")
	build.WithLabelValues(buildinfo.Version, buildinfo.Commit).Set(1)
	m.scans = counter("provider_scans_total", "Completed provider scans by result.", "provider", "result")
	m.scanDuration = histogram("provider_scan_duration_seconds", "Provider scan duration.", "provider")
	m.attempts = counter("delivery_attempts_total", "Notification attempts by channel and result.", "channel", "result")
	m.deliveryDuration = histogram("delivery_duration_seconds", "Notification attempt duration.", "channel")
	m.providerReady = gauge("provider_ready", "Whether the provider has a current successful baseline.", "provider")
	m.lastSuccess = gauge("provider_last_success_timestamp_seconds", "Last complete successful scan, Unix seconds.", "provider")
	m.deliveries = gauge("deliveries", "Durable deliveries by channel and state.", "channel", "status")
	m.oldest = gauge("oldest_pending_age_seconds", "Age of the oldest queued notification.", "channel")
	m.reloads = counter("config_reloads_total", "Configuration reload results.", "result")
	m.reloadSuccess = single("config_last_reload_successful", "Whether the latest configuration evaluation succeeded.")
	m.updateAvailable = single("update_available", "Last known update availability; consult update_check_success and timestamp.")
	m.updateSuccess = single("update_check_success", "Whether the last release check succeeded.")
	m.updateChecked = single("update_last_check_timestamp_seconds", "Time of the latest release check.")
	return m
}

func (m *Metrics) ObserveScan(provider string, elapsed time.Duration, success bool) {
	if m == nil {
		return
	}
	result := "error"
	if success {
		result = "success"
	}
	m.scans.WithLabelValues(provider, result).Inc()
	m.scanDuration.WithLabelValues(provider).Observe(elapsed.Seconds())
}
func (m *Metrics) ObserveDelivery(channel string, r model.DeliveryResult) {
	if m == nil {
		return
	}
	result := "permanent_error"
	if r.Success {
		result = "success"
	} else if r.Retryable {
		result = "retryable_error"
	}
	m.attempts.WithLabelValues(channel, result).Inc()
	m.deliveryDuration.WithLabelValues(channel).Observe(r.Duration.Seconds())
}
func (m *Metrics) Reload(result string) {
	if m != nil {
		m.reloads.WithLabelValues(result).Inc()
	}
}
func (m *Metrics) SetStatus(s model.Status) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status = s
	m.providerReady.Reset()
	m.lastSuccess.Reset()
	m.deliveries.Reset()
	m.oldest.Reset()
	now := time.Now()
	for name, p := range s.Providers {
		v := 0.
		if providerHealthy(p, s, now) {
			v = 1
		}
		m.providerReady.WithLabelValues(name).Set(v)
		ts := 0.
		if p.LastSuccess != nil {
			ts = float64(p.LastSuccess.Unix())
		}
		m.lastSuccess.WithLabelValues(name).Set(ts)
	}
	for channel, c := range s.Channels {
		for state, value := range map[string]int{"pending": c.Pending, "failed": c.Failed, "delivered": c.Delivered, "canceled": c.Canceled} {
			m.deliveries.WithLabelValues(channel, state).Set(float64(value))
		}
		age := 0.
		if c.OldestPending != nil {
			age = max(0, now.Sub(*c.OldestPending).Seconds())
		}
		m.oldest.WithLabelValues(channel).Set(age)
	}
	if s.Runtime != nil {
		v := 0.
		if s.Runtime.LastReloadSuccessful {
			v = 1
		}
		m.reloadSuccess.Set(v)
		if u := s.Runtime.Updates; u != nil {
			m.updateChecked.Set(float64(u.CheckedAt.Unix()))
			v = 0
			if u.Error == "" {
				v = 1
				available := 0.
				if u.UpdateAvailable {
					available = 1
				}
				m.updateAvailable.Set(available)
			}
			m.updateSuccess.Set(v)
		}
	}
}

type HealthResult struct {
	Healthy bool     `json:"healthy"`
	Reasons []string `json:"reasons"`
}

func Health(s model.Status, now time.Time, ready bool) HealthResult {
	r := HealthResult{Reasons: []string{}}
	if !s.Running {
		r.Reasons = append(r.Reasons, "not_running")
	}
	if s.UpdatedAt.IsZero() || now.Sub(s.UpdatedAt) > 60*time.Second {
		r.Reasons = append(r.Reasons, "stale_heartbeat")
	}
	if s.UpdatedAt.After(now.Add(5 * time.Second)) {
		r.Reasons = append(r.Reasons, "future_heartbeat")
	}
	if ready {
		if len(s.Providers) == 0 {
			r.Reasons = append(r.Reasons, "no_active_providers")
		}
		for _, p := range s.Providers {
			if !providerHealthy(p, s, now) {
				r.Reasons = append(r.Reasons, "provider_not_ready")
				break
			}
		}
	}
	r.Healthy = len(r.Reasons) == 0
	return r
}
func providerHealthy(p model.ProviderStatus, s model.Status, now time.Time) bool {
	if !p.Ready || p.LastError != "" || p.LastSuccess == nil || s.Runtime == nil || s.Runtime.PollIntervalSeconds <= 0 {
		return false
	}
	age := now.Sub(*p.LastSuccess).Seconds()
	budget := 2*s.Runtime.PollIntervalSeconds + float64(len(s.Providers))*300
	return age >= -5 && age <= budget
}
func (m *Metrics) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{Timeout: 5 * time.Second, MaxRequestsInFlight: 4}))
	health := func(ready bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			m.mu.RLock()
			s := m.status
			m.mu.RUnlock()
			result := Health(s, time.Now(), ready)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			if !result.Healthy {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			if r.Method != http.MethodHead {
				_ = json.NewEncoder(w).Encode(result)
			}
		}
	}
	mux.HandleFunc("/livez", health(false))
	mux.HandleFunc("/readyz", health(true))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		mux.ServeHTTP(w, r)
	})
}
