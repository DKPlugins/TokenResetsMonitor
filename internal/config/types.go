package config

import "time"

type Config struct {
	ConfigVersion     int              `yaml:"config_version" json:"config_version"`
	APIBaseURL        string           `yaml:"api_base_url" json:"api_base_url"`
	PollInterval      string           `yaml:"poll_interval" json:"poll_interval"`
	RequestTimeout    string           `yaml:"request_timeout" json:"request_timeout"`
	StatePath         string           `yaml:"state_path" json:"state_path"`
	Providers         []ProviderFilter `yaml:"providers" json:"providers"`
	EventTypes        []string         `yaml:"event_types" json:"event_types"`
	MinimumConfidence string           `yaml:"minimum_confidence" json:"minimum_confidence"`
	UnknownScope      string           `yaml:"unknown_scope" json:"unknown_scope"`
	Webhook           Webhook          `yaml:"webhook" json:"webhook"`
	Telegram          Telegram         `yaml:"telegram" json:"telegram"`
	Slack             Slack            `yaml:"slack" json:"slack"`
	Reload            Reload           `yaml:"reload" json:"reload"`
	Observability     Observability    `yaml:"observability" json:"observability"`
	Updates           Updates          `yaml:"updates" json:"updates"`
	Logging           Logging          `yaml:"logging" json:"logging"`
}

type ProviderFilter struct {
	Slug     string   `yaml:"slug" json:"slug"`
	Plans    []string `yaml:"plans" json:"plans"`
	Products []string `yaml:"products" json:"products"`
	Windows  []string `yaml:"windows" json:"windows"`
}

type Webhook struct {
	Enabled      bool              `yaml:"enabled" json:"enabled"`
	URL          string            `yaml:"url" json:"-"`
	Method       string            `yaml:"method" json:"method"`
	Headers      map[string]string `yaml:"headers" json:"-"`
	BodyTemplate string            `yaml:"body_template" json:"-"`
	Timeout      string            `yaml:"timeout" json:"timeout"`
}

type Telegram struct {
	Enabled             bool   `yaml:"enabled" json:"enabled"`
	BotToken            string `yaml:"bot_token" json:"-"`
	ChatID              string `yaml:"chat_id" json:"-"`
	MessageThreadID     int64  `yaml:"message_thread_id" json:"message_thread_id"`
	DisableNotification bool   `yaml:"disable_notification" json:"disable_notification"`
	Timeout             string `yaml:"timeout" json:"timeout"`
	APIBaseURL          string `yaml:"api_base_url" json:"-"`
}

type Logging struct {
	Level       string `yaml:"level" json:"level"`
	Format      string `yaml:"format" json:"format"`
	FileEnabled bool   `yaml:"file_enabled" json:"file_enabled"`
	Directory   string `yaml:"directory" json:"directory"`
	MaxSizeMB   int    `yaml:"max_size_mb" json:"max_size_mb"`
	MaxBackups  int    `yaml:"max_backups" json:"max_backups"`
	MaxAgeDays  int    `yaml:"max_age_days" json:"max_age_days"`
}

type Slack struct {
	Enabled    bool   `yaml:"enabled" json:"enabled"`
	WebhookURL string `yaml:"webhook_url" json:"-"`
	Timeout    string `yaml:"timeout" json:"timeout"`
}
type Reload struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
}
type Observability struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Listen  string `yaml:"listen" json:"listen"`
}
type Updates struct {
	Enabled           bool   `yaml:"enabled" json:"enabled"`
	Interval          string `yaml:"interval" json:"interval"`
	IncludePrerelease bool   `yaml:"include_prerelease" json:"include_prerelease"`
}

func (c Config) PollDuration() time.Duration { d, _ := time.ParseDuration(c.PollInterval); return d }
func (c Config) HTTPTimeout() time.Duration  { d, _ := time.ParseDuration(c.RequestTimeout); return d }
func (c Config) EnabledChannels() []string {
	var channels []string
	if c.Webhook.Enabled {
		channels = append(channels, "webhook")
	}
	if c.Telegram.Enabled {
		channels = append(channels, "telegram")
	}
	if c.Slack.Enabled {
		channels = append(channels, "slack")
	}
	return channels
}
