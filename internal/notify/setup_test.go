package notify

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

	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
)

func TestTelegramSetupMatchesOnlyFreshNonceAndPreservesSubscriptions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Error("setup did not POST")
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			fmt.Fprint(w, `{"ok":true,"result":{"id":1,"username":"fixture_bot"}}`)
		case strings.HasSuffix(r.URL.Path, "/getWebhookInfo"):
			fmt.Fprint(w, `{"ok":true,"result":{"url":""}}`)
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			var params map[string]any
			if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
				t.Error(err)
			}
			if _, ok := params["allowed_updates"]; ok {
				t.Error("setup changed bot subscriptions")
			}
			if offset, ok := params["offset"].(float64); ok && offset < 0 {
				t.Error("setup dropped backlog")
			}
			fmt.Fprint(w, `{"ok":true,"result":[
              {"update_id":1,"message":{"text":"/start wrong-nonce","chat":{"id":12}}},
              {"update_id":2,"message":{"text":"/start fixture-nonce","from":{"is_bot":true},"chat":{"id":13}}},
              {"update_id":3,"message":{"text":"/trm_connect@fixture_bot fixture-nonce","message_thread_id":42,"chat":{"id":-123,"type":"supergroup","title":"safe\u001b[2J"}}}
            ]}`)
		default:
			t.Errorf("unexpected mutation %s", r.URL.Path)
			http.Error(w, "bad method", 400)
		}
	}))
	defer server.Close()
	client, err := NewTelegramSetup(config.Telegram{BotToken: "123:secret", APIBaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	bot, err := client.Bot(context.Background())
	if err != nil || bot.Username != "fixture_bot" {
		t.Fatalf("getMe: %v", err)
	}
	hook, err := client.HasWebhook(context.Background())
	if err != nil || hook {
		t.Fatalf("webhook state: %v", err)
	}
	destination, err := client.Discover(context.Background(), "fixture-nonce", bot.Username)
	if err != nil || destination.ChatID != "-123" || destination.ThreadID != 42 || strings.Contains(destination.Label, "\x1b") {
		t.Fatalf("wrong destination %+v, %v", destination, err)
	}
}

func TestTelegramSetupConflictAndCancellationAreSanitized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		fmt.Fprint(w, `{"ok":false,"error_code":409,"description":"123:secret private endpoint"}`)
	}))
	defer server.Close()
	client, _ := NewTelegramSetup(config.Telegram{BotToken: "123:secret", APIBaseURL: server.URL})
	_, err := client.Discover(context.Background(), "nonce", "bot")
	var conflict *SetupError
	if !errors.As(err, &conflict) || conflict.Code != 409 || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe conflict: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.Discover(ctx, "nonce", "bot")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestSlackPayloadAndDeliveryClassification(t *testing.T) {
	for _, tc := range []struct {
		status         int
		body           string
		success, retry bool
	}{
		{200, "ok", true, false}, {201, "ok", false, false}, {204, "", false, false}, {429, "rate_limited", false, true}, {500, "server_error", false, true},
		{400, "invalid_payload secret", false, false}, {200, "unexpected", false, true},
	} {
		t.Run(fmt.Sprint(tc.status, tc.body), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload struct {
					Text   string `json:"text"`
					Mrkdwn bool   `json:"mrkdwn"`
				}
				if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&payload) != nil {
					t.Error("bad Slack request")
				}
				if payload.Mrkdwn || strings.Contains(payload.Text, "<!channel>") || !strings.Contains(payload.Text, "TEST") {
					t.Errorf("unsafe text %q", payload.Text)
				}
				if tc.status == 429 {
					w.Header().Set("Retry-After", "12")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			cfg := config.Defaults()
			cfg.Slack = config.Slack{Enabled: true, WebhookURL: server.URL + "/secret", Timeout: "1s"}
			n := TestNotification("")
			n.Event.Title = "<!channel> & announcement"
			preview, err := New(cfg).Preview("slack", n)
			if err != nil || preview.Destination != "[redacted]" {
				t.Fatalf("preview: %+v %v", preview, err)
			}
			result := New(cfg).Send(context.Background(), "slack", n)
			if result.Success != tc.success || result.Retryable != tc.retry || strings.Contains(result.Error, "secret") {
				t.Fatalf("result: %+v", result)
			}
			if tc.status == 429 && (!result.RateLimited || result.RetryAfter != 12*time.Second) {
				t.Fatal("Slack cooldown not preserved")
			}
		})
	}
}
