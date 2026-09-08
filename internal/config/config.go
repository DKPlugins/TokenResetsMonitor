// Package config loads the versioned configuration without exposing secret values in errors.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"
)

const Version = 1

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
		Webhook:  Webhook{Method: "POST", Timeout: "15s", Headers: map[string]string{}},
		Telegram: Telegram{Timeout: "15s", APIBaseURL: "https://api.telegram.org"},
		Logging:  Logging{Level: "info", Format: "text", Directory: filepath.Join(base, "logs"), MaxSizeMB: 10, MaxBackups: 5, MaxAgeDays: 14},
	}
}

func Load(path string, overrides map[string]string) (Config, error) {
	cfg := Defaults()
	cfg.ConfigVersion = 0 // A missing version is legacy input and requires explicit migration.
	data, err := readConfig(path)
	if err != nil {
		return cfg, err
	}
	if err = decode(data, &cfg); err != nil {
		return cfg, err
	}
	if cfg.ConfigVersion != Version {
		return cfg, errors.New("unsupported config_version; use config migrate for older configurations")
	}
	if err = ApplyEnvironment(&cfg, overrides); err != nil {
		return cfg, err
	}
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
	f, err := os.Open(path)
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

func apply(v reflect.Value, prefix string, overrides map[string]string) error {
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
			if err := apply(field, key, overrides); err != nil {
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
		if err := expand(field, key); err != nil {
			return err
		}
	}
	return nil
}

func expand(v reflect.Value, key string) error {
	switch v.Kind() {
	case reflect.String:
		var missing bool
		value := envReference.ReplaceAllStringFunc(v.String(), func(ref string) string {
			value, ok := os.LookupEnv(ref[2 : len(ref)-1])
			if !ok {
				missing = true
			}
			return value
		})
		if missing {
			return fmt.Errorf("unset environment reference in %s", key)
		}
		v.SetString(value)
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			if err := expand(v.Index(i), key); err != nil {
				return err
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if err := expand(v.Field(i), key); err != nil {
				return err
			}
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			copy := reflect.New(iter.Value().Type()).Elem()
			copy.Set(iter.Value())
			if err := expand(copy, key); err != nil {
				return err
			}
			v.SetMapIndex(iter.Key(), copy)
		}
	}
	return nil
}

func Validate(cfg Config) error {
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
	for name, value := range map[string]string{"request_timeout": cfg.RequestTimeout, "webhook.timeout": cfg.Webhook.Timeout, "telegram.timeout": cfg.Telegram.Timeout} {
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
		if cfg.Webhook.BodyTemplate != "" {
			_, err := template.New("body").Funcs(template.FuncMap{"json": func(v any) (string, error) { b, e := json.Marshal(v); return string(b), e }}).Parse(cfg.Webhook.BodyTemplate)
			if err != nil {
				return errors.New("invalid webhook.body_template (content omitted)")
			}
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
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
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

// Migrate keeps the original bytes in a backup and only rewrites known v0 data explicitly.
func Migrate(path string, applyChanges bool) (string, error) {
	data, e := readConfig(path)
	if e != nil {
		return "", errors.New("cannot read configuration")
	}
	var cfg Config
	if e = decode(data, &cfg); e != nil {
		return "", e
	}
	if cfg.ConfigVersion > Version || cfg.ConfigVersion < 0 {
		return "", errors.New("unsupported configuration version")
	}
	if cfg.ConfigVersion == Version {
		return "Configuration is already at version 1.", nil
	}
	if !applyChanges {
		return "Would migrate config_version 0 to 1; all existing keys and environment references are preserved. Use --apply to create a backup and write the result.", nil
	}
	var doc yaml.Node
	if e = yaml.Unmarshal(data, &doc); e != nil {
		return "", errors.New("invalid configuration")
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return "", errors.New("configuration must be a mapping")
	}
	m := doc.Content[0]
	found := false
	for i := 0; i < len(m.Content); i += 2 {
		if m.Content[i].Value == "config_version" {
			m.Content[i+1].Value = "1"
			m.Content[i+1].Tag = "!!int"
			found = true
		}
	}
	if !found {
		m.Content = append([]*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!str", Value: "config_version"}, {Kind: yaml.ScalarNode, Tag: "!!int", Value: "1"}}, m.Content...)
	}
	out, e := yaml.Marshal(&doc)
	if e != nil {
		return "", errors.New("cannot encode migrated configuration")
	}
	backup := path + ".backup-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	if e = WriteNewBytes(backup, data); e != nil {
		return "", e
	}
	temp, e := os.CreateTemp(filepath.Dir(path), ".config-migration-*")
	if e != nil {
		return "", errors.New("cannot create migration output")
	}
	name := temp.Name()
	defer os.Remove(name)
	if e = temp.Chmod(0600); e == nil {
		_, e = temp.Write(out)
	}
	if e == nil {
		e = temp.Sync()
	}
	ce := temp.Close()
	if e != nil || ce != nil {
		return "", errors.New("cannot write migration output")
	}
	if e = os.Rename(name, path); e != nil {
		return "", errors.New("cannot replace configuration; backup was preserved")
	}
	return "Migrated configuration to version 1; original saved in a sibling .backup file.", nil
}
