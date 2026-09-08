// Package notify renders and sends notifications. Test and production deliveries
// intentionally share this implementation and never log request bodies or tokens.
package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/DKPlugins/TokenResetsMonitor/internal/api"
	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
)

const maxBodyBytes = 1 << 20

type Sender struct{ cfg config.Config }

func New(cfg config.Config) *Sender { return &Sender{cfg: cfg} }

// TestNotification creates a synthetic event without reading state or the API.
func TestNotification(provider string) model.Notification {
	var random [16]byte
	// crypto/rand.Read never returns an error on supported Go platforms.
	_, _ = rand.Read(random[:])
	id := "test-" + hex.EncodeToString(random[:])
	now := time.Now().UTC()
	if provider == "" {
		provider = "openai-codex"
	}
	return model.Notification{SchemaVersion: 1, ID: id, DetectedAt: now, Test: true, Event: model.Event{
		ID: id, Slug: id, Provider: model.Provider{Slug: provider, Name: provider}, EventType: "hard_reset", Status: "published",
		Title: "TEST: TokenResetsMonitor notification", Summary: "Synthetic delivery test. This is not an actual reset announcement.",
		AnnouncedAt: now, PublishedAt: &now, Revision: 1,
		Scope:      model.Scope{Products: []string{}, Plans: []string{}, Windows: []string{}, Evidence: "unknown"},
		Confidence: model.Confidence{Label: "reported", Score: 0.5}, Links: model.Links{HTML: "https://tokenresets.com/"},
	}}
}

type rendered struct {
	method   string
	endpoint string
	headers  http.Header
	body     []byte
	timeout  time.Duration
}

func duration(value string) (time.Duration, error) {
	if value == "" {
		return 30 * time.Second, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, errors.New("notification timeout must be a positive duration")
	}
	return d, nil
}

func validURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil && u.Fragment == ""
}

func (s *Sender) render(channel string, n model.Notification) (rendered, error) {
	var r rendered
	if n.ID == "" || strings.ContainsAny(n.ID, "\r\n") {
		return r, errors.New("notification identifier is invalid")
	}
	r.headers = make(http.Header)
	r.headers.Set("Content-Type", "application/json")
	r.headers.Set("User-Agent", "TokenResetsMonitor/1")
	var err error
	switch channel {
	case "webhook":
		c := s.cfg.Webhook
		if !validURL(c.URL) {
			return r, errors.New("webhook URL must be an absolute HTTP(S) URL without user info or fragment")
		}
		r.endpoint, r.method = c.URL, c.Method
		if r.method == "" {
			r.method = http.MethodPost
		}
		r.timeout, err = duration(c.Timeout)
		if err != nil {
			return r, err
		}
		for key, value := range c.Headers {
			if !validHeaderName(key) || !validHeaderValue(value) {
				return r, errors.New("webhook contains an invalid HTTP header")
			}
			switch strings.ToLower(key) {
			case "host", "content-length", "transfer-encoding", "connection":
				return r, errors.New("webhook headers cannot override HTTP transport headers")
			}
			r.headers.Set(key, value)
		}
		// Protocol headers remain authoritative even with custom user headers.
		r.headers.Set("Idempotency-Key", n.ID)
		if n.Test {
			r.headers.Set("X-TokenResetsMonitor-Test", "true")
		} else {
			r.headers.Del("X-TokenResetsMonitor-Test")
		}
		if c.BodyTemplate == "" {
			r.body, err = json.Marshal(n)
		} else {
			r.body, err = config.RenderTemplate(c.BodyTemplate, n)
			if err != nil {
				return r, err
			}
		}
	case "telegram":
		c := s.cfg.Telegram
		if c.BotToken == "" || c.ChatID == "" {
			return r, errors.New("Telegram bot token and chat ID are required")
		}
		if strings.ContainsAny(c.BotToken, "/?#\r\n\t ") {
			return r, errors.New("Telegram bot token has an invalid format")
		}
		base := c.APIBaseURL
		if base == "" {
			base = "https://api.telegram.org"
		}
		if !validURL(base) {
			return r, errors.New("Telegram API base URL is invalid")
		}
		u, _ := url.Parse(base)
		if u.RawQuery != "" {
			return r, errors.New("Telegram API base URL must not contain a query")
		}
		r.endpoint = strings.TrimRight(base, "/") + "/bot" + c.BotToken + "/sendMessage"
		r.method = http.MethodPost
		r.timeout, err = duration(c.Timeout)
		if err != nil {
			return r, err
		}
		if c.MessageThreadID < 0 {
			return r, errors.New("Telegram message thread ID must not be negative")
		}
		r.body, err = json.Marshal(struct {
			ChatID              string `json:"chat_id"`
			Text                string `json:"text"`
			MessageThreadID     int64  `json:"message_thread_id,omitempty"`
			DisableNotification bool   `json:"disable_notification"`
			LinkPreviewOptions  struct {
				IsDisabled bool `json:"is_disabled"`
			} `json:"link_preview_options"`
		}{ChatID: c.ChatID, Text: telegramText(n), MessageThreadID: c.MessageThreadID, DisableNotification: c.DisableNotification,
			LinkPreviewOptions: struct {
				IsDisabled bool `json:"is_disabled"`
			}{true}})
	case "slack":
		if err := renderSlack(s.cfg.Slack, n, &r); err != nil {
			return r, err
		}
	default:
		return r, errors.New("unknown notification channel")
	}
	if err != nil {
		return r, errors.New("cannot encode notification body")
	}
	if len(r.body) > maxBodyBytes {
		return r, errors.New("notification body exceeds size limit")
	}
	if _, err := http.NewRequest(r.method, r.endpoint, nil); err != nil {
		return r, errors.New("notification HTTP method or URL is invalid")
	}
	return r, nil
}

func validHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if (value[i] < 32 && value[i] != '\t') || value[i] == 127 {
			return false
		}
	}
	return true
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", r) {
			continue
		}
		return false
	}
	return true
}

func telegramText(n model.Notification) string {
	e := n.Event
	prefix := "TokenResetsMonitor"
	if n.Test {
		prefix += " — TEST"
	}
	date := func(t *time.Time) string {
		if t == nil {
			return "unknown"
		}
		return t.UTC().Format(time.RFC3339)
	}
	scope := func(values []string) string {
		if len(values) == 0 {
			return "unknown"
		}
		return truncate(strings.Join(values, ", "), 160)
	}
	text := fmt.Sprintf("%s\n%s\nProvider: %s\nEvent: %s\nConfidence: %s\nProducts: %s\nPlans: %s\nWindows: %s\nAnnounced: %s\nPublished: %s\nEffective: %s\nDetected: %s\n\nPublic announcement; this does not confirm the limits of your account. Unknown scope means the source did not specify it.\n%s",
		prefix, truncate(e.Title, 400), truncate(e.Provider.Name, 100), truncate(e.EventType, 80), truncate(e.Confidence.Label, 40),
		scope(e.Scope.Products), scope(e.Scope.Plans), scope(e.Scope.Windows), e.AnnouncedAt.UTC().Format(time.RFC3339),
		date(e.PublishedAt), date(e.EffectiveAt), n.DetectedAt.UTC().Format(time.RFC3339), truncate(e.Links.HTML, 1000))
	return truncate(text, 3800)
}

// Byte bounds keep the text comfortably below Telegram's UTF-16 character limit.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	end := limit - len("…")
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "…"
}

// Preview renders exactly the request Send uses, while hiding the entire path,
// query, user info, header values, destination chat and request body.
func (s *Sender) Preview(channel string, n model.Notification) (model.Preview, error) {
	r, err := s.render(channel, n)
	if err != nil {
		return model.Preview{}, err
	}
	u, _ := url.Parse(r.endpoint)
	names := make([]string, 0, len(r.headers))
	for name := range r.headers {
		names = append(names, name)
	}
	sort.Strings(names)
	destination := u.Scheme + "://" + u.Host + "/[redacted]"
	if channel == "slack" {
		destination = "[redacted]"
	}
	return model.Preview{Channel: channel, Method: r.method, Destination: destination, HeaderNames: names, BodyBytes: len(r.body)}, nil
}

// Send performs exactly one attempt. Retry scheduling belongs to the monitor.
func (s *Sender) Send(ctx context.Context, channel string, n model.Notification) (result model.DeliveryResult) {
	started := time.Now()
	defer func() { result.Duration = time.Since(started) }()
	r, err := s.render(channel, n)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	req, err := http.NewRequestWithContext(ctx, r.method, r.endpoint, bytes.NewReader(r.body))
	if err != nil {
		result.Error = "cannot create notification request"
		return result
	}
	req.Header = r.headers
	client := &http.Client{Timeout: r.timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		result.Retryable = true
		result.Error = "notification network request failed"
		var netErr net.Error
		if errors.Is(ctx.Err(), context.Canceled) {
			result.Error = "notification request canceled"
		}
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
			result.Error = "notification request timed out"
		}
		return result
	}
	defer res.Body.Close()
	result.StatusCode = res.StatusCode
	result.RateLimited = res.StatusCode == http.StatusTooManyRequests
	result.RetryAfter = api.ParseRetryAfter(res.Header.Get("Retry-After"), time.Now())
	if channel == "slack" {
		decodeSlack(res.Body, &result)
		return result
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		result.Retryable = res.StatusCode == 429 || res.StatusCode >= 500
		result.Error = fmt.Sprintf("notification endpoint returned HTTP %d", res.StatusCode)
		if channel == "telegram" {
			decodeTelegram(res.Body, &result, false)
		}
		return result
	}
	if channel == "telegram" {
		decodeTelegram(res.Body, &result, true)
		return result
	}

	// A 2xx is success even if the response body is empty, oversized or malformed.
	// Do not let an irrelevant response body convert accepted delivery into retry.
	result.Success = true
	return result
}

func decodeTelegram(body io.Reader, result *model.DeliveryResult, httpSuccess bool) {
	var response struct {
		OK         *bool `json:"ok"`
		ErrorCode  int   `json:"error_code"`
		Parameters struct {
			RetryAfter json.Number `json:"retry_after"`
		} `json:"parameters"`
	}
	data, err := io.ReadAll(io.LimitReader(body, maxBodyBytes+1))
	if err != nil || len(data) > maxBodyBytes || json.Unmarshal(data, &response) != nil || response.OK == nil {
		if httpSuccess {
			result.Error = "Telegram returned an invalid response"
			result.Retryable = true
		}
		return
	}
	if response.ErrorCode == 429 {
		result.RateLimited = true
		result.Retryable = true
	}
	if response.Parameters.RetryAfter != "" {
		if d := api.ParseRetryAfter(string(response.Parameters.RetryAfter), time.Now()); d > result.RetryAfter {
			result.RetryAfter = d
		}
	}
	if !httpSuccess {
		return
	}
	if *response.OK {
		result.Success = true
		return
	}
	result.Error = fmt.Sprintf("Telegram rejected notification (error code %d)", response.ErrorCode)
	result.Retryable = response.ErrorCode == 429 || response.ErrorCode >= 500
}
