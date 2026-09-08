// Package config loads the versioned configuration without exposing secret values in errors.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/fileio"
	"gopkg.in/yaml.v3"
)

const Version = 3

var envReference = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
var methodPattern = regexp.MustCompile(`^[A-Z]+$`)
var headerPattern = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "TokenResetsMonitor", "config.yaml")
}

func Defaults() Config {
	base := filepath.Dir(DefaultPath())
	return Config{
		ConfigVersion: Version, APIBaseURL: "https://tokenresets.com/api/v1",
		PollInterval: "30m", RequestTimeout: "30s", StatePath: filepath.Join(base, "state.db"),
		Providers:  []ProviderFilter{{Slug: "openai-codex"}, {Slug: "anthropic-claude"}},
		EventTypes: []string{"hard_reset"}, MinimumConfidence: "reported", UnknownScope: "include",
		Webhook:       Webhook{Method: "POST", Timeout: "15s", Headers: map[string]string{}},
		Telegram:      Telegram{NotifyChanges: true, Timeout: "15s", APIBaseURL: "https://api.telegram.org"},
		Slack:         Slack{NotifyChanges: true, Timeout: "15s"},
		Reload:        Reload{Enabled: true},
		Observability: Observability{Listen: "127.0.0.1:9090"},
		Updates:       Updates{Enabled: true, Interval: "24h"},
		Logging:       Logging{Level: "info", Format: "text", Directory: filepath.Join(base, "logs"), MaxSizeMB: 10, MaxBackups: 5, MaxAgeDays: 14},
	}
}

func Load(path string, overrides map[string]string) (Config, error) {
	data, err := readConfig(path)
	if err != nil {
		return Config{}, err
	}
	return LoadBytes(data, path, overrides)
}

// LoadBytes resolves the exact snapshot supplied by the caller; it never rereads
// the file. Call Validate before starting or replacing a runtime configuration.
func LoadBytes(data []byte, path string, overrides map[string]string) (Config, error) {
	cfg, err := decodeRaw(data)
	if err != nil {
		return cfg, err
	}
	if err = ApplyEnvironment(&cfg, overrides); err != nil {
		return cfg, err
	}
	return resolvePaths(cfg, path)
}

func decodeRaw(data []byte) (Config, error) {
	cfg := Defaults()
	cfg.ConfigVersion = 0
	if len(data) > 1024*1024 {
		return cfg, errors.New("configuration exceeds 1 MiB")
	}
	if err := decode(data, &cfg); err != nil {
		return cfg, err
	}
	if cfg.ConfigVersion == 1 || cfg.ConfigVersion == 2 {
		cfg.ConfigVersion = Version
	}
	if cfg.ConfigVersion != Version {
		return cfg, errors.New("unsupported config_version; use config migrate for older configurations")
	}
	return cfg, nil
}

// ReadRaw returns unresolved values for setup editing and the original bytes for
// concurrent-edit detection. A v1 file is upgraded in memory without rewriting it.
func ReadRaw(path string) (Config, []byte, error) {
	data, err := readConfig(path)
	if err != nil {
		return Config{}, nil, err
	}
	cfg, err := decodeRaw(data)
	return cfg, data, err
}

// LoadStructural loads operational paths without requiring notification secrets.
// It is suitable for status and diagnostics, never for sending notifications.
func LoadStructural(path string, overrides map[string]string) (Config, error) {
	cfg, _, err := ReadRaw(path)
	if err != nil {
		return cfg, err
	}
	operational := struct {
		StatePath     string        `yaml:"state_path"`
		Logging       Logging       `yaml:"logging"`
		Observability Observability `yaml:"observability"`
		Updates       Updates       `yaml:"updates"`
	}{cfg.StatePath, cfg.Logging, cfg.Observability, cfg.Updates}
	if err = apply(reflect.ValueOf(&operational).Elem(), "", overrides); err != nil {
		return cfg, err
	}
	cfg.StatePath, cfg.Logging, cfg.Observability, cfg.Updates = operational.StatePath, operational.Logging, operational.Observability, operational.Updates
	if cfg.StatePath == "" {
		return cfg, errors.New("state_path cannot be empty")
	}
	return resolvePaths(cfg, path)
}

func resolvePaths(cfg Config, path string) (Config, error) {
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return cfg, errors.New("invalid configuration directory")
	}
	if !filepath.IsAbs(cfg.StatePath) {
		cfg.StatePath = filepath.Join(base, cfg.StatePath)
	}
	if !filepath.IsAbs(cfg.Logging.Directory) {
		cfg.Logging.Directory = filepath.Join(base, cfg.Logging.Directory)
	}
	return cfg, nil
}

func readConfig(path string) ([]byte, error) {
	f, err := fileio.OpenSnapshot(path)
	if err != nil {
		return nil, errors.New("cannot read configuration; run init or specify --config")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil {
		return nil, errors.New("cannot read configuration")
	}
	if len(data) > 1024*1024 {
		return nil, errors.New("configuration exceeds 1 MiB")
	}
	return data, nil
}

func decode(data []byte, cfg *Config) error {
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(cfg); err != nil {
		return errors.New("invalid YAML or unknown configuration field (values omitted)")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("configuration must contain exactly one YAML document")
	}
	return nil
}

// ApplyEnvironment applies TRM_* and explicit overrides, then expands ${NAME}
// only inside parsed string values. Collections are replaced, not merged.
// Unset references are errors, not silently empty credentials.
func ApplyEnvironment(cfg *Config, overrides map[string]string) error {
	if err := apply(reflect.ValueOf(cfg).Elem(), "", overrides); err != nil {
		return err
	}
	if cfg.ConfigVersion != Version {
		return errors.New("unsupported config_version")
	}
	return nil
}

type stringResolver func(value, key string) (string, error)

func apply(v reflect.Value, prefix string, overrides map[string]string) error {
	return applyResolved(v, prefix, overrides, resolveStrict)
}

func applyResolved(v reflect.Value, prefix string, overrides map[string]string, resolve stringResolver) error {
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		field := v.Field(i)
		name := strings.Split(t.Field(i).Tag.Get("yaml"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		key := name
		if prefix != "" {
			key = prefix + "_" + name
		}
		if field.Kind() == reflect.Struct {
			if err := applyResolved(field, key, overrides, resolve); err != nil {
				return err
			}
			continue
		}
		raw, exists := os.LookupEnv("TRM_" + strings.ToUpper(key))
		if value, ok := overrides[key]; ok {
			raw, exists = value, true
		}
		if exists {
			if field.Kind() == reflect.String {
				field.SetString(raw)
			} else {
				fresh := reflect.New(field.Type())
				decoder := yaml.NewDecoder(strings.NewReader(raw))
				decoder.KnownFields(true)
				if err := decoder.Decode(fresh.Interface()); err != nil {
					return fmt.Errorf("invalid override for %s (value omitted)", key)
				}
				var extra any
				if err := decoder.Decode(&extra); err != io.EOF {
					return fmt.Errorf("override for %s must contain one YAML value", key)
				}
				field.Set(fresh.Elem())
			}
		}
		if err := expandResolved(field, key, resolve); err != nil {
			return err
		}
	}
	return nil
}

func resolveStrict(value, key string) (string, error) {
	missing := false
	expanded := envReference.ReplaceAllStringFunc(value, func(ref string) string {
		value, ok := os.LookupEnv(ref[2 : len(ref)-1])
		if !ok {
			missing = true
		}
		return value
	})
	if missing {
		return "", fmt.Errorf("unset environment reference in %s", key)
	}
	return expanded, nil
}

func expandResolved(v reflect.Value, key string, resolve stringResolver) error {
	switch v.Kind() {
	case reflect.String:
		value, err := resolve(v.String(), key)
		if err != nil {
			return err
		}
		v.SetString(value)
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			if err := expandResolved(v.Index(i), fmt.Sprintf("%s[%d]", key, i), resolve); err != nil {
				return err
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			name := strings.Split(v.Type().Field(i).Tag.Get("yaml"), ",")[0]
			if err := expandResolved(v.Field(i), key+"."+name, resolve); err != nil {
				return err
			}
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			copy := reflect.New(iter.Value().Type()).Elem()
			copy.Set(iter.Value())
			if err := expandResolved(copy, key, resolve); err != nil {
				return err
			}
			v.SetMapIndex(iter.Key(), copy)
		}
	}
	return nil
}

func Validate(cfg Config) error {
	if cfg.History.RetentionDays < 0 || cfg.History.RetentionDays > 36500 {
		return errors.New("history.retention_days must be between 0 and 36500")
	}
	if cfg.ConfigVersion != Version {
		return errors.New("unsupported config_version")
	}
	if !validURL(cfg.APIBaseURL) {
		return errors.New("api_base_url must be an absolute HTTP(S) URL")
	}
	if u, _ := url.Parse(cfg.APIBaseURL); u.RawQuery != "" {
		return errors.New("api_base_url cannot contain a query")
	}
	if d, e := time.ParseDuration(cfg.PollInterval); e != nil || d < time.Minute {
		return errors.New("poll_interval must be at least 1m")
	}
	for name, value := range map[string]string{"request_timeout": cfg.RequestTimeout, "webhook.timeout": cfg.Webhook.Timeout, "telegram.timeout": cfg.Telegram.Timeout, "slack.timeout": cfg.Slack.Timeout} {
		if d, e := time.ParseDuration(value); e != nil || d <= 0 || d > 10*time.Minute {
			return fmt.Errorf("%s must be positive and at most 10m", name)
		}
	}
	if cfg.StatePath == "" {
		return errors.New("state_path cannot be empty")
	}
	if len(cfg.Providers) == 0 {
		return errors.New("at least one provider is required")
	}
	seen := map[string]bool{}
	for _, p := range cfg.Providers {
		if !slugPattern.MatchString(p.Slug) || seen[p.Slug] {
			return errors.New("provider slugs must be valid and unique")
		}
		seen[p.Slug] = true
		for _, values := range [][]string{p.Plans, p.Products, p.Windows} {
			for _, value := range values {
				if !slugPattern.MatchString(value) {
					return errors.New("scope filters must contain valid slugs")
				}
			}
		}
	}
	if len(cfg.EventTypes) == 0 {
		return errors.New("event_types cannot be empty")
	}
	for _, e := range cfg.EventTypes {
		if !regexp.MustCompile(`^[a-z][a-z0-9_]*$`).MatchString(e) {
			return errors.New("invalid event_types value")
		}
	}
	switch cfg.MinimumConfidence {
	case "unverified", "probable", "reported", "verified", "official":
	default:
		return errors.New("invalid minimum_confidence")
	}
	if cfg.UnknownScope != "include" && cfg.UnknownScope != "exclude" {
		return errors.New("unknown_scope must be include or exclude")
	}
	if cfg.Webhook.Enabled {
		if err := ValidateChannel(cfg, "webhook"); err != nil {
			return err
		}
	}
	if cfg.Telegram.Enabled {
		if err := ValidateChannel(cfg, "telegram"); err != nil {
			return err
		}
	}
	if cfg.Slack.Enabled {
		if err := ValidateChannel(cfg, "slack"); err != nil {
			return err
		}
	}
	if _, port, err := net.SplitHostPort(cfg.Observability.Listen); err != nil {
		return errors.New("observability.listen must be a host:port address")
	} else if n, e := strconv.Atoi(port); e != nil || n < 1 || n > 65535 {
		return errors.New("observability.listen port must be between 1 and 65535")
	}
	if d, e := time.ParseDuration(cfg.Updates.Interval); e != nil || d < time.Hour || d > 7*24*time.Hour {
		return errors.New("updates.interval must be between 1h and 168h")
	}
	switch cfg.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		return errors.New("invalid logging.level")
	}
	if cfg.Logging.Format != "json" && cfg.Logging.Format != "text" {
		return errors.New("logging.format must be text or json")
	}
	if cfg.Logging.Directory == "" || cfg.Logging.MaxSizeMB < 1 || cfg.Logging.MaxBackups < 1 || cfg.Logging.MaxAgeDays < 1 {
		return errors.New("logging directory and positive rotation limits are required")
	}
	return nil
}

func ValidateChannel(cfg Config, channel string) error {
	switch channel {
	case "webhook":
		if !validURL(cfg.Webhook.URL) {
			return errors.New("webhook.url must be an absolute HTTP(S) URL")
		}
		if !methodPattern.MatchString(cfg.Webhook.Method) {
			return errors.New("webhook.method must be an uppercase HTTP method")
		}
		for name, value := range cfg.Webhook.Headers {
			if !headerPattern.MatchString(name) || !validHeaderValue(value) {
				return errors.New("invalid webhook header (value omitted)")
			}
			switch strings.ToLower(name) {
			case "host", "content-length", "transfer-encoding", "connection", "idempotency-key", "x-tokenresetsmonitor-test":
				return errors.New("webhook header overrides a reserved transport header")
			}
		}
		if err := ValidateTemplate(cfg.Webhook.BodyTemplate); err != nil {
			return err
		}

	case "telegram":
		if cfg.Telegram.BotToken == "" || cfg.Telegram.ChatID == "" {
			return errors.New("telegram.bot_token and telegram.chat_id are required")
		}
		if strings.ContainsAny(cfg.Telegram.BotToken, "/\r\n\t ?#") {
			return errors.New("invalid telegram.bot_token")
		}
		if !validURL(cfg.Telegram.APIBaseURL) {
			return errors.New("invalid telegram.api_base_url")
		}
		if u, _ := url.Parse(cfg.Telegram.APIBaseURL); u.RawQuery != "" {
			return errors.New("telegram.api_base_url cannot contain a query")
		}
		if cfg.Telegram.MessageThreadID < 0 {
			return errors.New("telegram.message_thread_id must be nonnegative")
		}
	case "slack":
		if !validURL(cfg.Slack.WebhookURL) {
			return errors.New("slack.webhook_url must be an absolute HTTP(S) URL without credentials or fragment")
		}
	default:
		return errors.New("unknown notification channel")
	}
	return nil
}

func validHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if (value[i] < 32 && value[i] != '\t') || value[i] == 127 {
			return false
		}
	}
	return true
}

func validURL(value string) bool {
	u, e := url.Parse(value)
	return e == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.Fragment == "" && u.User == nil
}

func WriteNew(path string, cfg Config) error {
	data, e := yaml.Marshal(cfg)
	if e != nil {
		return errors.New("cannot encode configuration")
	}
	return WriteNewBytes(path, data)
}

func WriteNewBytes(path string, data []byte) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return errors.New("cannot create configuration directory")
	}
	f, e := fileio.CreatePrivate(path)
	if e != nil {
		return errors.New("cannot create configuration; destination may already exist")
	}
	success := false
	defer func() {
		f.Close()
		if !success {
			os.Remove(path)
		}
	}()
	_, e = f.Write(data)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil || ce != nil {
		return errors.New("cannot write configuration")
	}
	success = true
	return nil
}

// Migrate preserves the original bytes and upgrades only the version field.
func Migrate(path string, applyChanges bool) (string, error) {
	data, err := readConfig(path)
	if err != nil {
		return "", err
	}
	var cfg Config
	if err := decode(data, &cfg); err != nil {
		return "", err
	}
	if cfg.ConfigVersion < 0 || cfg.ConfigVersion > Version {
		return "", errors.New("unsupported configuration version")
	}
	if cfg.ConfigVersion == Version {
		return fmt.Sprintf("Configuration is already at version %d.", Version), nil
	}
	if !applyChanges {
		return fmt.Sprintf("Would migrate config_version %d to %d; existing keys, comments and environment references are preserved. Use --apply to create a backup and write the result.", cfg.ConfigVersion, Version), nil
	}
	out, err := patchDocument(data, "", nil)
	if err != nil {
		return "", err
	}
	if err := writeUpdated(path, data, out); err != nil {
		return "", err
	}
	return fmt.Sprintf("Migrated configuration to version %d; original saved in a sibling .backup file.", Version), nil
}

// ResolveChannel applies environment precedence to one destination without requiring
// unrelated channel secrets (used by setup and channel diagnostics).
func ResolveChannel(cfg Config, channel string) (Config, error) {
	var value any
	switch channel {
	case "telegram":
		value = &cfg.Telegram
	case "slack":
		value = &cfg.Slack
	case "webhook":
		value = &cfg.Webhook
	default:
		return cfg, errors.New("unknown notification channel")
	}
	err := apply(reflect.ValueOf(value).Elem(), channel, nil)
	return cfg, err
}
