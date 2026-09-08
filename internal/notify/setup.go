package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/DKPlugins/TokenResetsMonitor/internal/api"
	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
)

// SetupError exposes only safe classifications, never Telegram descriptions or
// the credential-bearing request URL.
type SetupError struct {
	Code       int
	RetryAfter time.Duration
}

func (e *SetupError) Error() string {
	switch e.Code {
	case 401:
		return "Telegram token was rejected; obtain the token from BotFather again"
	case 403:
		return "Telegram access was denied; check bot membership and send permissions"
	case 409:
		return "another application is receiving this bot's updates; use a dedicated bot or enter the chat ID manually"
	case 429:
		return "Telegram rate limit reached; wait before trying setup again"
	default:
		return fmt.Sprintf("Telegram setup request failed (code %d)", e.Code)
	}
}

type TelegramSetup struct {
	cfg    config.Telegram
	client *http.Client
}
type TelegramBot struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}
type TelegramDestination struct {
	ChatID   string
	ThreadID int64
	Label    string
}

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

func NewTelegramSetup(cfg config.Telegram) (*TelegramSetup, error) {
	if cfg.BotToken == "" || strings.ContainsAny(cfg.BotToken, "/\\?#\r\n\t ") {
		return nil, errors.New("Telegram bot token is missing or invalid")
	}
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = "https://api.telegram.org"
	}
	u, err := url.Parse(cfg.APIBaseURL)
	if err != nil || !validURL(cfg.APIBaseURL) || u.RawQuery != "" {
		return nil, errors.New("Telegram API base URL is invalid")
	}
	return &TelegramSetup{cfg: cfg, client: &http.Client{Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *TelegramSetup) call(ctx context.Context, method string, params any, result any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return errors.New("cannot encode Telegram setup request")
	}
	endpoint := strings.TrimRight(c.cfg.APIBaseURL, "/") + "/bot" + c.cfg.BotToken + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("cannot create Telegram setup request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "TokenResetsMonitor/setup")
	res, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Telegram setup network request failed; check connectivity")
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxBodyBytes+1))
	if err != nil || len(data) > maxBodyBytes {
		return errors.New("Telegram setup response could not be read safely")
	}
	var envelope struct {
		OK         bool            `json:"ok"`
		Result     json.RawMessage `json:"result"`
		ErrorCode  int             `json:"error_code"`
		Parameters struct {
			RetryAfter json.Number `json:"retry_after"`
		} `json:"parameters"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return errors.New("Telegram setup returned invalid JSON")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 || !envelope.OK {
		code := envelope.ErrorCode
		if code == 0 {
			code = res.StatusCode
		}
		after := api.ParseRetryAfter(res.Header.Get("Retry-After"), time.Now())
		if d := api.ParseRetryAfter(string(envelope.Parameters.RetryAfter), time.Now()); d > after {
			after = d
		}
		return &SetupError{Code: code, RetryAfter: after}
	}
	if len(envelope.Result) == 0 || json.Unmarshal(envelope.Result, result) != nil {
		return errors.New("Telegram setup returned an invalid result")
	}
	return nil
}

func (c *TelegramSetup) Bot(ctx context.Context) (TelegramBot, error) {
	var bot TelegramBot
	if err := c.call(ctx, "getMe", struct{}{}, &bot); err != nil {
		return bot, err
	}
	if bot.ID == 0 || !usernamePattern.MatchString(bot.Username) {
		return bot, errors.New("Telegram returned an invalid bot identity")
	}
	return bot, nil
}

func (c *TelegramSetup) HasWebhook(ctx context.Context) (bool, error) {
	var info struct {
		URL *string `json:"url"`
	}
	if err := c.call(ctx, "getWebhookInfo", struct{}{}, &info); err != nil {
		return false, err
	}
	if info.URL == nil {
		return false, errors.New("Telegram returned an invalid webhook status")
	}
	return *info.URL != "", nil
}

// Discover acknowledges updates as it advances the offset, so the CLI explains
// that automatic discovery requires a dedicated bot. It never removes a webhook,
// drops pending updates, or changes allowed_updates.
func (c *TelegramSetup) Discover(ctx context.Context, nonce, username string) (TelegramDestination, error) {
	var offset int64
	for {
		var updates []struct {
			ID          int64         `json:"update_id"`
			Message     *setupMessage `json:"message"`
			ChannelPost *setupMessage `json:"channel_post"`
		}
		err := c.call(ctx, "getUpdates", struct {
			Offset  int64 `json:"offset,omitempty"`
			Timeout int   `json:"timeout"`
			Limit   int   `json:"limit"`
		}{offset, 20, 100}, &updates)
		if err != nil {
			return TelegramDestination{}, err
		}
		for _, update := range updates {
			if update.ID >= offset {
				offset = update.ID + 1
			}
			message := update.Message
			if message == nil {
				message = update.ChannelPost
			}
			if message == nil || message.Chat.ID == 0 || (message.From != nil && message.From.IsBot) {
				continue
			}
			words := strings.Fields(message.Text)
			if len(words) != 2 || words[1] != nonce {
				continue
			}
			command := words[0]
			if command != "/start" && command != "/start@"+username && command != "/trm_connect" && command != "/trm_connect@"+username {
				continue
			}
			title := message.Chat.Title
			if title == "" {
				title = message.Chat.FirstName
			}
			if title == "" {
				title = message.Chat.Type
			}
			return TelegramDestination{ChatID: strconv.FormatInt(message.Chat.ID, 10), ThreadID: message.ThreadID, Label: safeSetupLabel(title)}, nil
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return TelegramDestination{}, ctx.Err()
		case <-timer.C:
		}
	}
}

type setupMessage struct {
	Text     string `json:"text"`
	ThreadID int64  `json:"message_thread_id"`
	From     *struct {
		IsBot bool `json:"is_bot"`
	} `json:"from"`
	Chat struct {
		ID        int64  `json:"id"`
		Type      string `json:"type"`
		Title     string `json:"title"`
		FirstName string `json:"first_name"`
	} `json:"chat"`
}

func safeSetupLabel(s string) string {
	return truncate(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return -1
		}
		return r
	}, s), 120)
}
