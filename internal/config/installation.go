package config

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// ValidateInstallation validates all effective literals without requiring the
// installer's environment to contain service credentials. A nonempty deferred
// result is NOT a runnable configuration: full Validate/Load is still required
// under the service account at startup. Nothing is written or exported to env.
func ValidateInstallation(path string, overrides map[string]string) (deferred []string, err error) {
	cfg, _, err := ReadRaw(path)
	if err != nil {
		return nil, err
	}
	pending := map[string]bool{}
	resolve := func(value, key string) (string, error) {
		missing := false
		partial := envReference.ReplaceAllStringFunc(value, func(ref string) string {
			actual, ok := os.LookupEnv(ref[2 : len(ref)-1])
			if ok {
				return actual
			}
			missing = true
			return ref
		})
		if !missing {
			return partial, nil
		}
		pending[installationFieldName(key)] = true
		return installationStandIn(partial, key), nil
	}
	if err := applyResolved(reflect.ValueOf(&cfg).Elem(), "", overrides, resolve); err != nil {
		return nil, err
	}
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	for field := range pending {
		deferred = append(deferred, field)
	}
	sort.Strings(deferred)
	return deferred, nil
}

func installationFieldName(key string) string {
	for _, section := range []string{"telegram", "webhook", "slack", "logging", "observability", "updates"} {
		if strings.HasPrefix(key, section+"_") {
			return section + "." + strings.TrimPrefix(key, section+"_")
		}
	}
	return key
}

// Stand-ins are used only in a throwaway validation value and only for the
// unresolved leaf. They never replace actual saved/runtime credentials or hide
// invalid literals in other fields of the same channel.
func installationStandIn(partial, key string) string {
	defaults := Defaults()
	switch key {
	case "api_base_url", "webhook_url", "telegram_api_base_url", "slack_webhook_url":
		return installationURL(partial)
	case "webhook_headers":
		// Keep literal control characters, so a missing token cannot disguise an
		// invalid Authorization/header value.
		return envReference.ReplaceAllString(partial, "deferred")
	case "webhook_body_template":
		// Missing template fragments can contain valid syntax; defer rendering
		// this leaf, while the channel's URL/method/headers remain validated.
		return ""
	case "webhook_method":
		return envReference.ReplaceAllString(partial, "POST")
	case "telegram_bot_token":
		return envReference.ReplaceAllString(partial, "1:deferred")
	case "telegram_chat_id":
		return envReference.ReplaceAllString(partial, "1")
	case "poll_interval":
		return defaults.PollInterval
	case "request_timeout":
		return defaults.RequestTimeout
	case "telegram_timeout":
		return defaults.Telegram.Timeout
	case "webhook_timeout":
		return defaults.Webhook.Timeout
	case "slack_timeout":
		return defaults.Slack.Timeout
	case "state_path":
		return defaults.StatePath
	case "logging_directory":
		return defaults.Logging.Directory
	case "logging_level":
		return installationEnum(partial, []string{"info", "debug", "warn", "error"})
	case "logging_format":
		return installationEnum(partial, []string{"text", "json"})
	case "observability_listen":
		return defaults.Observability.Listen
	case "updates_interval":
		return defaults.Updates.Interval
	case "minimum_confidence":
		return installationEnum(partial, []string{"reported", "unverified", "probable", "verified", "official"})
	case "unknown_scope":
		return installationEnum(partial, []string{"include", "exclude"})
	}
	if strings.HasPrefix(key, "providers[") {
		replacement := "deferred"
		if strings.HasSuffix(key, ".slug") {
			// Distinct unknown provider IDs must not look like confirmed
			// duplicates during the structural pass.
			replacement = fmt.Sprintf("deferred-%x", []byte(key))
		}
		return envReference.ReplaceAllString(partial, replacement)
	}
	if strings.HasPrefix(key, "event_types[") {
		return envReference.ReplaceAllString(partial, "hard_reset")
	}
	return envReference.ReplaceAllString(partial, "deferred")
}

func installationURL(partial string) string {
	first := true
	return envReference.ReplaceAllStringFunc(partial, func(ref string) string {
		initial := first && strings.HasPrefix(partial, ref)
		first = false
		if initial {
			suffix := strings.TrimPrefix(partial, ref)
			if strings.HasPrefix(suffix, "://") {
				return "https"
			}
			if strings.HasPrefix(suffix, "//") {
				return "https:"
			}
			return "https://deferred.invalid"
		}
		// Numeric substitutions are valid in hosts, ports, paths and tokens;
		// static malformed schemes, spaces, credentials/fragments remain visible.
		return "1"
	})
}

// For enums, retain literal constraints around deferred substitutions rather
// than allowing an impossible prefix/suffix to disappear into a default.
func installationEnum(partial string, allowed []string) string {
	var pattern strings.Builder
	pattern.WriteString("^")
	start := 0
	for _, match := range envReference.FindAllStringIndex(partial, -1) {
		pattern.WriteString(regexp.QuoteMeta(partial[start:match[0]]))
		pattern.WriteString(".*")
		start = match[1]
	}
	pattern.WriteString(regexp.QuoteMeta(partial[start:]))
	pattern.WriteString("$")
	matcher := regexp.MustCompile(pattern.String())
	for _, value := range allowed {
		if matcher.MatchString(value) {
			return value
		}
	}
	return envReference.ReplaceAllString(partial, "")
}
