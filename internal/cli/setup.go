package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/notify"
	"golang.org/x/term"
)

type setupPrompter struct {
	input  io.Reader
	reader *bufio.Reader
	out    io.Writer
}

func newSetupPrompter(in io.Reader, out io.Writer) *setupPrompter {
	return &setupPrompter{input: in, reader: bufio.NewReader(in), out: out}
}
func (p *setupPrompter) ask(question, defaultValue string) (string, error) {
	if defaultValue != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", question, defaultValue)
	} else {
		fmt.Fprintf(p.out, "%s: ", question)
	}
	value, err := p.reader.ReadString('\n')
	if err != nil && !(err == io.EOF && value != "") {
		return "", errors.New("setup input ended; no configuration was written (use init --defaults for unattended setup)")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		value = defaultValue
	}
	return value, nil
}
func (p *setupPrompter) secret(question, current string) (string, error) {
	suffix := ""
	if current != "" {
		suffix = " (Enter keeps the current value)"
	}
	fmt.Fprintf(p.out, "%s%s: ", question, suffix)
	var value string
	if file, ok := p.input.(*os.File); ok && term.IsTerminal(int(file.Fd())) && p.reader.Buffered() == 0 {
		data, err := term.ReadPassword(int(file.Fd()))
		fmt.Fprintln(p.out)
		if err != nil {
			return "", errors.New("cannot read hidden secret input; use an environment reference")
		}
		value = strings.TrimSpace(string(data))
		clear(data)
	} else {
		line, err := p.reader.ReadString('\n')
		if err != nil && !(err == io.EOF && line != "") {
			return "", errors.New("setup input ended; no configuration was written")
		}
		value = strings.TrimSpace(line)
	}
	if value == "" {
		value = current
	}
	return value, nil
}
func (p *setupPrompter) yes(question string, defaultYes bool) (bool, error) {
	fallback := "no"
	if defaultYes {
		fallback = "yes"
	}
	for {
		value, err := p.ask(question+" (yes/no)", fallback)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(value) {
		case "yes", "y":
			return true, nil
		case "no", "n":
			return false, nil
		}
		fmt.Fprintln(p.out, "Please enter yes or no.")
	}
}

func setupChannel(ctx context.Context, opts options, channel string, in io.Reader, out io.Writer) error {
	if channel != "telegram" && channel != "slack" {
		return errors.New("setup requires telegram or slack")
	}
	cfg, original, err := config.ReadRaw(opts.configPath)
	if err != nil {
		return err
	}
	prompt := newSetupPrompter(in, out)
	fmt.Fprintln(out, "Configure notifications. Secrets are hidden during terminal input; environment references stay references in the file.")
	switch channel {
	case "telegram":
		if err = configureTelegram(ctx, prompt, &cfg.Telegram); err != nil {
			return err
		}
		err = config.SaveChannel(opts.configPath, channel, cfg.Telegram, original)
	case "slack":
		if err = configureSlack(ctx, prompt, &cfg.Slack); err != nil {
			return err
		}
		err = config.SaveChannel(opts.configPath, channel, cfg.Slack, original)
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "Settings saved; the previous configuration is backed up beside the file. A running monitor applies them when automatic reload is enabled.")
	fmt.Fprintln(out, "Keep referenced environment variables available to the service/container. Changing its environment requires restarting that service/container.")
	return nil
}

func configureTelegram(ctx context.Context, p *setupPrompter, cfg *config.Telegram) error {
	fmt.Fprintln(p.out, "Create a dedicated bot at https://t.me/BotFather using /newbot, then paste its token or an environment reference.")
	if _, ok := os.LookupEnv("TRM_TELEGRAM_BOT_TOKEN"); ok {
		cfg.BotToken = "${TRM_TELEGRAM_BOT_TOKEN}"
		fmt.Fprintln(p.out, "TRM_TELEGRAM_BOT_TOKEN overrides the file; update that environment variable to rotate the active token.")
	}
	value, err := p.secret("Bot token or ${ENV_NAME}", cfg.BotToken)
	if err != nil {
		return err
	}
	cfg.BotToken = value
	runtime, err := config.ResolveChannel(config.Config{Telegram: *cfg}, "telegram")
	if err != nil {
		return err
	}
	client, err := notify.NewTelegramSetup(runtime.Telegram)
	if err != nil {
		return err
	}
	setupCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	bot, err := client.Bot(setupCtx)
	if err != nil {
		return err
	}
	fmt.Fprintf(p.out, "Connected to @%s.\n", bot.Username)
	hasWebhook, err := client.HasWebhook(setupCtx)
	if err != nil {
		return err
	}
	automatic := false
	if hasWebhook {
		fmt.Fprintln(p.out, "This bot has an active webhook. It is left intact; enter the destination manually or create a dedicated bot.")
	} else {
		fmt.Fprintln(p.out, "Automatic discovery reads and acknowledges this bot's pending updates. Use a dedicated bot.")
		automatic, err = p.yes("Detect the chat automatically", true)
		if err != nil {
			return err
		}
	}
	if automatic {
		var random [16]byte
		_, _ = rand.Read(random[:])
		nonce := "trm_" + hex.EncodeToString(random[:])
		fmt.Fprintf(p.out, "Private chat: https://t.me/%s?start=%s\n", bot.Username, nonce)
		fmt.Fprintf(p.out, "Group: https://t.me/%s?startgroup=%s\n", bot.Username, nonce)
		fmt.Fprintf(p.out, "For a forum topic, send /trm_connect@%s %s inside that topic. Waiting up to five minutes...\n", bot.Username, nonce)
		destination, discoverErr := client.Discover(setupCtx, nonce, bot.Username)
		if discoverErr == nil {
			fmt.Fprintf(p.out, "Found %s (chat %s, topic %d).\n", destination.Label, destination.ChatID, destination.ThreadID)
			accepted, err := p.yes("Use this destination", true)
			if err != nil {
				return err
			}
			if accepted {
				cfg.ChatID, cfg.MessageThreadID = destination.ChatID, destination.ThreadID
			} else {
				automatic = false
			}
		} else {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintf(p.out, "Automatic discovery did not finish: %s. You can enter the chat ID manually.\n", discoverErr)
			automatic = false
		}
	}
	if !automatic {
		cfg.ChatID, err = p.ask("Chat ID (numeric ID or @channel username)", cfg.ChatID)
		if err != nil {
			return err
		}
		thread, err := p.ask("Forum topic ID (0 for no topic)", strconv.FormatInt(cfg.MessageThreadID, 10))
		if err != nil {
			return err
		}
		id, err := strconv.ParseInt(thread, 10, 64)
		if err != nil || id < 0 {
			return errors.New("topic ID must be a nonnegative integer")
		}
		cfg.MessageThreadID = id
		accepted, err := p.yes("Use chat "+safePromptText(cfg.ChatID)+" with topic "+strconv.FormatInt(id, 10), true)
		if err != nil {
			return err
		}
		if !accepted {
			return errors.New("setup canceled; no configuration was written")
		}
	}
	cfg.Enabled = true
	runtime, err = config.ResolveChannel(config.Config{Telegram: *cfg}, "telegram")
	if err != nil {
		return err
	}
	if err := config.ValidateChannel(runtime, "telegram"); err != nil {
		return err
	}
	if runtime.Telegram.ChatID != cfg.ChatID || runtime.Telegram.MessageThreadID != cfg.MessageThreadID {
		fmt.Fprintln(p.out, "TRM_TELEGRAM_CHAT_ID or TRM_TELEGRAM_MESSAGE_THREAD_ID overrides this destination at runtime.")
		accepted, err := p.yes("Keep the environment override", false)
		if err != nil {
			return err
		}
		if !accepted {
			return errors.New("setup canceled; update the environment override and retry")
		}
	}
	return offerSetupTest(ctx, p, runtime, "telegram")
}

func configureSlack(ctx context.Context, p *setupPrompter, cfg *config.Slack) error {
	fmt.Fprintln(p.out, "Create a Slack app, enable Incoming Webhooks and choose a channel:")
	fmt.Fprintln(p.out, "https://docs.slack.dev/messaging/sending-messages-using-incoming-webhooks/")
	if _, ok := os.LookupEnv("TRM_SLACK_WEBHOOK_URL"); ok {
		cfg.WebhookURL = "${TRM_SLACK_WEBHOOK_URL}"
		fmt.Fprintln(p.out, "TRM_SLACK_WEBHOOK_URL overrides the file; update it to change the active destination.")
	}
	value, err := p.secret("Incoming webhook URL or ${ENV_NAME}", cfg.WebhookURL)
	if err != nil {
		return err
	}
	cfg.WebhookURL = value
	cfg.Enabled = true
	runtime, err := config.ResolveChannel(config.Config{Slack: *cfg}, "slack")
	if err != nil {
		return err
	}
	if err := config.ValidateChannel(runtime, "slack"); err != nil {
		return err
	}
	accepted, err := p.yes("Save this Slack destination", true)
	if err != nil {
		return err
	}
	if !accepted {
		return errors.New("setup canceled; no configuration was written")
	}
	return offerSetupTest(ctx, p, runtime, "slack")
}
func offerSetupTest(ctx context.Context, p *setupPrompter, cfg config.Config, channel string) error {
	send, err := p.yes("Send a TEST notification now", false)
	if err != nil {
		return err
	}
	if !send {
		return nil
	}
	result := notify.New(cfg).Send(ctx, channel, notify.TestNotification(""))
	if !result.Success {
		return fmt.Errorf("TEST failed: %s; settings were not written", result.Error)
	}
	fmt.Fprintln(p.out, "TEST delivered.")
	return nil
}
func safePromptText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return -1
		}
		return r
	}, s)
}
