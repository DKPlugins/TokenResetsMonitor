package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/DKPlugins/TokenResetsMonitor/internal/api"
	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
)

func initialize(ctx context.Context, opts options, overrides map[string]string, in io.Reader, out io.Writer) error {
	if _, e := os.Lstat(opts.configPath); e == nil {
		return errors.New("configuration already exists; it was not overwritten")
	}
	cfg := config.Defaults()
	if e := config.ApplyEnvironment(&cfg, overrides); e != nil {
		return e
	}
	// Runtime environment credentials must never be materialized into a newly
	// generated file. Composite header overrides stay in the environment.
	credentialRefs := []struct {
		name   string
		target *string
	}{
		{"TRM_WEBHOOK_URL", &cfg.Webhook.URL}, {"TRM_WEBHOOK_BODY_TEMPLATE", &cfg.Webhook.BodyTemplate},
		{"TRM_TELEGRAM_BOT_TOKEN", &cfg.Telegram.BotToken}, {"TRM_TELEGRAM_CHAT_ID", &cfg.Telegram.ChatID},
		{"TRM_SLACK_WEBHOOK_URL", &cfg.Slack.WebhookURL},
	}
	for _, ref := range credentialRefs {
		if _, ok := os.LookupEnv(ref.name); ok {
			*ref.target = "${" + ref.name + "}"
		}
	}
	if _, ok := os.LookupEnv("TRM_WEBHOOK_HEADERS"); ok {
		cfg.Webhook.Headers = map[string]string{}
	}
	if opts.defaults {
		validation := cloneConfig(cfg)
		if e := config.ApplyEnvironment(&validation, overrides); e != nil {
			return e
		}
		if e := config.Validate(validation); e != nil {
			return e
		}
		if e := config.WriteNew(opts.configPath, cfg); e != nil {
			return e
		}
		fmt.Fprintln(out, "Default configuration created. Notifications are disabled unless enabled through overrides.")
		fmt.Fprintln(out, "Environment credentials remain references or runtime overrides; make them available to the service account.")
		return nil
	}
	prompt := newSetupPrompter(in, out)
	ask := prompt.ask
	fmt.Fprintln(out, "TokenResetsMonitor setup. This monitors public announcements, not your personal account limits.")
	client := api.New(cfg.APIBaseURL, cfg.HTTPTimeout(), nil)
	catalog, e := client.Catalog(ctx)
	if e != nil {
		fmt.Fprintln(out, "Provider catalog is unavailable. Enter provider slugs manually; defaults are Codex and Claude.")
	} else {
		fmt.Fprintln(out, "Available providers:")
		for _, p := range catalog {
			fmt.Fprintf(out, "  %s - %s\n", p.Slug, p.Name)
		}
	}
	var selected []string
	for _, p := range cfg.Providers {
		selected = append(selected, p.Slug)
	}
	value, e := ask("Provider slugs (comma-separated)", strings.Join(selected, ","))
	if e != nil {
		return e
	}
	cfg.Providers = nil
	for _, slug := range splitList(value) {
		p := config.ProviderFilter{Slug: slug}
		if detail, err := client.Detail(ctx, slug); err == nil {
			fmt.Fprintf(out, "%s options:\n", slug)
			for _, dimension := range []struct {
				name   string
				values []string
			}{{"Plans", names(detail.Plans)}, {"Products", names(detail.Products)}, {"Windows", names(detail.Windows)}} {
				fmt.Fprintf(out, "  %s: %s\n", dimension.name, strings.Join(dimension.values, ", "))
			}
		} else {
			fmt.Fprintln(out, "Provider details unavailable; filters can still be entered manually.")
		}
		plans, err := ask(slug+" plans (comma-separated; * = all)", "*")
		if err != nil {
			return err
		}
		p.Plans = splitList(plans)
		products, err := ask(slug+" products (* = all)", "*")
		if err != nil {
			return err
		}
		p.Products = splitList(products)
		windows, err := ask(slug+" windows (* = all)", "*")
		if err != nil {
			return err
		}
		p.Windows = splitList(windows)
		cfg.Providers = append(cfg.Providers, p)
	}
	if cfg.PollInterval, e = ask("Polling interval", cfg.PollInterval); e != nil {
		return e
	}
	if value, e = ask("Event types (hard_reset, banked_reset_grant, quota_policy_change, temporary_multiplier, scheduled_reset)", strings.Join(cfg.EventTypes, ",")); e != nil {
		return e
	}
	cfg.EventTypes = splitList(value)
	if cfg.MinimumConfidence, e = ask("Minimum confidence (unverified, probable, reported, verified, official)", cfg.MinimumConfidence); e != nil {
		return e
	}
	if cfg.UnknownScope, e = ask("Unknown scope (include or exclude)", cfg.UnknownScope); e != nil {
		return e
	}
	if value, e = ask("Enable webhook? (yes/no)", "no"); e != nil {
		return e
	}
	cfg.Webhook.Enabled = isYes(value)
	if cfg.Webhook.Enabled {
		fmt.Fprintln(out, "Use ${ENV_NAME} references for secret URLs or authorization values; they remain references in the file.")
		if cfg.Webhook.URL, e = prompt.secret("Webhook URL or environment reference", cfg.Webhook.URL); e != nil {
			return e
		}
		if cfg.Webhook.Method, e = ask("HTTP method", cfg.Webhook.Method); e != nil {
			return e
		}
		if value, e = prompt.secret("Authorization header (blank = none)", ""); e != nil {
			return e
		}
		if value != "" {
			if cfg.Webhook.Headers == nil {
				cfg.Webhook.Headers = map[string]string{}
			}
			cfg.Webhook.Headers["Authorization"] = value
		}
		if cfg.Webhook.BodyTemplate, e = ask("Body template (blank = standard JSON)", ""); e != nil {
			return e
		}
	}
	if value, e = ask("Enable Telegram? (yes/no)", "no"); e != nil {
		return e
	}
	cfg.Telegram.Enabled = isYes(value)
	if cfg.Telegram.Enabled {
		if e = configureTelegram(ctx, prompt, &cfg.Telegram); e != nil {
			return e
		}
	}
	if value, e = ask("Enable Slack? (yes/no)", "no"); e != nil {
		return e
	}
	cfg.Slack.Enabled = isYes(value)
	if cfg.Slack.Enabled {
		if e = configureSlack(ctx, prompt, &cfg.Slack); e != nil {
			return e
		}
	}

	if value, e = ask("Enable rotating file logs? (yes/no)", "no"); e != nil {
		return e
	}
	cfg.Logging.FileEnabled = isYes(value)
	if cfg.Logging.FileEnabled {
		if cfg.Logging.Directory, e = ask("Log directory", cfg.Logging.Directory); e != nil {
			return e
		}
	}
	if cfg.StatePath, e = ask("State database path", cfg.StatePath); e != nil {
		return e
	}
	// Validate a deep copy so expanded credentials never replace references written by the wizard.
	validation := cloneConfig(cfg)
	if e = config.ApplyEnvironment(&validation, nil); e != nil {
		return e
	}
	if e = config.Validate(validation); e != nil {
		return e
	}
	if e = config.WriteNew(opts.configPath, cfg); e != nil {
		return e
	}
	fmt.Fprintln(out, "Configuration created. The first complete scan establishes history without sending old events.")
	fmt.Fprintln(out, "Use config validate, then test-notification all to test configured destinations, or run to start monitoring.")
	return nil
}

func cloneConfig(cfg config.Config) config.Config {
	copy := cfg
	copy.Webhook.Headers = map[string]string{}
	for k, v := range cfg.Webhook.Headers {
		copy.Webhook.Headers[k] = v
	}
	copy.EventTypes = slices.Clone(cfg.EventTypes)
	copy.Providers = slices.Clone(cfg.Providers)
	for i := range copy.Providers {
		copy.Providers[i].Plans = slices.Clone(cfg.Providers[i].Plans)
		copy.Providers[i].Products = slices.Clone(cfg.Providers[i].Products)
		copy.Providers[i].Windows = slices.Clone(cfg.Providers[i].Windows)
	}
	return copy
}

func splitList(s string) []string {
	if s == "" || s == "*" {
		return nil
	}
	var result []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			result = append(result, v)
		}
	}
	return result
}
func isYes(s string) bool { return strings.EqualFold(s, "yes") || strings.EqualFold(s, "y") }
func names(values []model.NamedValue) []string {
	var result []string
	for _, v := range values {
		result = append(result, v.Slug)
	}
	return result
}
