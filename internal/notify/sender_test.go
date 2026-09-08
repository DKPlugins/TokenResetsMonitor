package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
)

func webhookConfig(endpoint string) config.Config {
	return config.Config{Webhook: config.Webhook{URL: endpoint, Method: "POST", Timeout: "1s"}}
}

func TestWebhookUsesConfiguredRequestAndAuthoritativeProtocolHeaders(t *testing.T) {
	n := TestNotification("openai-codex")
	n.Event.Title = `quote " and newline` + "\n" + "你好"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "PUT" || r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("Idempotency-Key") != n.ID || r.Header.Get("X-TokenResetsMonitor-Test") != "true" {
			t.Errorf("wrong method or headers: %s %+v", r.Method, r.Header)
		}
		var body struct {
			Title string `json:"title"`
			Test  bool   `json:"test"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Title != n.Event.Title || !body.Test {
			t.Errorf("template JSON escaping: %+v %v", body, err)
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	cfg := webhookConfig(server.URL)
	cfg.Webhook.Method = "PUT"
	cfg.Webhook.Headers = map[string]string{"Authorization": "Bearer secret", "Idempotency-Key": "wrong", "X-TokenResetsMonitor-Test": "false"}
	cfg.Webhook.BodyTemplate = `{"title":{{json .Event.Title}},"test":{{json .Test}}}`
	sender := New(cfg)
	for i := 0; i < 2; i++ {
		if result := sender.Send(context.Background(), "webhook", n); !result.Success {
			t.Fatalf("send failed: %+v", result)
		}
	}
	if requests != 2 {
		t.Fatalf("Send retries internally: %d requests", requests)
	}
}

func TestDefaultWebhookPayloadAndProductionMarker(t *testing.T) {
	n := TestNotification("")
	n.Test = false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got model.Notification
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		if got.ID != n.ID || got.SchemaVersion != 1 || got.Event.Revision != 1 || got.Test {
			t.Errorf("wrong versioned payload: %+v", got)
		}
		if r.Header.Get("X-TokenResetsMonitor-Test") != "" {
			t.Error("production marked as test")
		}
		w.WriteHeader(200)
		fmt.Fprint(w, "response content is deliberately ignored")
	}))
	defer server.Close()
	cfg := webhookConfig(server.URL)
	cfg.Webhook.Headers = map[string]string{"X-TokenResetsMonitor-Test": "true"}
	if got := New(cfg).Send(context.Background(), "webhook", n); !got.Success {
		t.Fatal(got)
	}
}

func TestPreviewIsOfflineAndRedactsAllSensitiveComponents(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++ }))
	defer server.Close()
	cfg := webhookConfig(server.URL + "/secret-path?token=private-query")
	cfg.Webhook.Headers = map[string]string{"Authorization": "private-header"}
	cfg.Webhook.BodyTemplate = `{"private":"private-body","event":{{json .Event.ID}}}`
	n := TestNotification("")
	preview, err := New(cfg).Preview("webhook", n)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(preview)
	for _, secret := range []string{"secret-path", "private-query", "private-header", "private-body"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("preview leaked %s", secret)
		}
	}
	if requests != 0 || preview.Method != "POST" || preview.BodyBytes == 0 {
		t.Fatalf("preview must render without network: %+v calls=%d", preview, requests)
	}
	cfg.Telegram = config.Telegram{BotToken: "123:private-token", ChatID: "private-chat", APIBaseURL: server.URL}
	preview, err = New(cfg).Preview("telegram", n)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(preview)
	if strings.Contains(string(encoded), "private-") || requests != 0 {
		t.Fatal("Telegram preview leaked credentials or used network")
	}
}

func TestMalformedTemplatesAndHeadersAreRejectedOfflineWithoutSecrets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*config.Config)
	}{
		{"parse", func(c *config.Config) { c.Webhook.BodyTemplate = `{{ secret-sensitive` }},
		{"execute", func(c *config.Config) { c.Webhook.BodyTemplate = `{{.SecretSensitive}}` }},
		{"header_name", func(c *config.Config) { c.Webhook.Headers = map[string]string{"Secret Sensitive": "credential"} }},
		{"header_value", func(c *config.Config) {
			c.Webhook.Headers = map[string]string{"Authorization": "secret-sensitive\r\nInjected: value"}
		}},
		{"transport_override", func(c *config.Config) { c.Webhook.Headers = map[string]string{"Content-Length": "300"} }},
		{"method", func(c *config.Config) { c.Webhook.Method = "invalid method secret-sensitive" }},
		{"url", func(c *config.Config) { c.Webhook.URL = "https://secret-sensitive:password@example.com" }},
		{"oversized", func(c *config.Config) { c.Webhook.BodyTemplate = strings.Repeat("x", maxBodyBytes+1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := webhookConfig("https://example.invalid/secret-sensitive")
			tc.change(&c)
			_, err := New(c).Preview("webhook", TestNotification(""))
			if err == nil {
				t.Fatal("invalid request accepted")
			}
			if strings.Contains(strings.ToLower(err.Error()), "secret-sensitive") {
				t.Fatalf("error leak: %v", err)
			}
		})
	}
}

func TestDeliveryStatusClassificationAndNoRedirect(t *testing.T) {
	for _, status := range []int{200, 204, 301, 302, 400, 401, 404, 408, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Retry-After", "7")
				w.Header().Set("Location", "https://example.invalid/secret")
				w.WriteHeader(status)
				fmt.Fprint(w, "secret-body")
			}))
			defer server.Close()
			result := New(webhookConfig(server.URL+"/secret-path")).Send(context.Background(), "webhook", TestNotification(""))
			if result.Success != (status >= 200 && status < 300) || result.Retryable != (status == 429 || status >= 500) || result.StatusCode != status || requests != 1 {
				t.Fatalf("wrong result %+v; attempts %d", result, requests)
			}
			if strings.Contains(result.Error, "secret") {
				t.Fatal("response leaked")
			}
			if !result.Success && result.RetryAfter != 7*time.Second {
				t.Fatal("Retry-After lost")
			}
		})
	}
}

func TestTimeoutAndCanceledContextAreSanitized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()
	cfg := webhookConfig(server.URL + "/secret-token")
	cfg.Webhook.Timeout = "10ms"
	result := New(cfg).Send(context.Background(), "webhook", TestNotification(""))
	if !result.Retryable || !strings.Contains(result.Error, "timed out") || strings.Contains(result.Error, "secret-token") {
		t.Fatalf("timeout: %+v", result)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result = New(cfg).Send(ctx, "webhook", TestNotification(""))
	if !result.Retryable || !strings.Contains(result.Error, "canceled") {
		t.Fatalf("cancel: %+v", result)
	}
}

func TestTelegramRequestAndResponseSemantics(t *testing.T) {
	for _, tc := range []struct {
		name, body     string
		status         int
		success, retry bool
		after          time.Duration
	}{
		{"ok", `{"ok":true,"result":{"message_id":1}}`, 200, true, false, 0},
		{"rejected", `{"ok":false,"error_code":400,"description":"secret-token"}`, 200, false, false, 0},
		{"body_rate_limit", `{"ok":false,"error_code":429,"parameters":{"retry_after":12}}`, 200, false, true, 12 * time.Second},
		{"http_rate_limit", `{"ok":false,"error_code":429,"parameters":{"retry_after":12}}`, 429, false, true, 12 * time.Second},
		{"server", `{"ok":false,"error_code":500}`, 500, false, true, 0},
		{"invalid", `secret-token invalid`, 200, false, true, 0},
		{"missing_ok", `{}`, 200, false, true, 0},
		{"false_http_ok", `{"ok":true}`, 400, false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.URL.Path != "/bot123:secret-token/sendMessage" || r.Method != "POST" {
					t.Errorf("wrong Telegram target %s", r.URL.Path)
				}
				var got struct {
					ChatID          string `json:"chat_id"`
					Text            string `json:"text"`
					MessageThreadID int64  `json:"message_thread_id"`
					Disable         bool   `json:"disable_notification"`
				}
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Error(err)
				}
				if got.ChatID != "-12345" || got.MessageThreadID != 99 || !got.Disable || !strings.Contains(got.Text, "TEST") || !strings.Contains(got.Text, "unknown") || !strings.Contains(got.Text, "Detected:") {
					t.Errorf("wrong Telegram payload %+v", got)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			cfg := config.Config{Telegram: config.Telegram{BotToken: "123:secret-token", ChatID: "-12345", MessageThreadID: 99, DisableNotification: true, APIBaseURL: server.URL, Timeout: "1s"}}
			result := New(cfg).Send(context.Background(), "telegram", TestNotification(""))
			if result.Success != tc.success || result.Retryable != tc.retry || result.RetryAfter != tc.after || requests != 1 {
				t.Fatalf("result %+v", result)
			}
			if strings.Contains(result.Error, "secret-token") {
				t.Fatal("token leaked in error")
			}
		})
	}
}

func TestSyntheticIDsAreUniqueAndLongTelegramMessagesAreBounded(t *testing.T) {
	n := TestNotification("anthropic-claude")
	if n.ID == TestNotification("anthropic-claude").ID || !n.Test || n.Event.Provider.Slug != "anthropic-claude" {
		t.Fatal("invalid test identity")
	}
	n.Event.Title = strings.Repeat("😀", 5000)
	n.Event.Scope.Plans = []string{strings.Repeat("max", 3000)}
	text := telegramText(n)
	if len(text) > 3800 || !strings.Contains(text, "TEST") || !strings.Contains(text, "Public announcement") {
		t.Fatal("oversized text")
	}
	if !json.Valid([]byte(fmt.Sprintf(`{"text":%q}`, text))) {
		t.Fatal("broken unicode")
	}
}

func TestWebhookIgnoresUnneededResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		io.Copy(w, strings.NewReader(strings.Repeat("x", maxBodyBytes+1)))
	}))
	defer server.Close()
	if result := New(webhookConfig(server.URL)).Send(context.Background(), "webhook", TestNotification("")); !result.Success {
		t.Fatal(result)
	}
}
