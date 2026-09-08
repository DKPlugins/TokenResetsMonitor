// Package model defines the versioned data shared by the monitor and its adapters.
package model

import "time"

type NamedValue struct {
	Slug       string `json:"slug" yaml:"slug"`
	Name       string `json:"name" yaml:"name"`
	IsWildcard bool   `json:"is_wildcard,omitempty" yaml:"is_wildcard,omitempty"`
}

type Provider struct {
	Slug         string       `json:"slug"`
	Name         string       `json:"name"`
	Products     []NamedValue `json:"products,omitempty"`
	Plans        []NamedValue `json:"plans,omitempty"`
	Windows      []NamedValue `json:"windows,omitempty"`
	SourceHealth string       `json:"source_health,omitempty"`
}

type Scope struct {
	Products []string `json:"products"`
	Plans    []string `json:"plans"`
	Windows  []string `json:"windows"`
	Evidence string   `json:"scope_evidence"`
}

type Confidence struct {
	Label   string   `json:"label"`
	Score   float64  `json:"score"`
	Reasons []string `json:"reasons,omitempty"`
}

type Links struct {
	HTML string `json:"html"`
	Self string `json:"self"`
}

type Event struct {
	ID                  string     `json:"id"`
	Slug                string     `json:"slug"`
	Provider            Provider   `json:"provider"`
	EventType           string     `json:"event_type"`
	Status              string     `json:"status"`
	Title               string     `json:"title"`
	Summary             string     `json:"summary"`
	AnnouncedAt         time.Time  `json:"announced_at"`
	PublishedAt         *time.Time `json:"published_at"`
	EffectiveAt         *time.Time `json:"effective_at"`
	ExpectedBy          *time.Time `json:"expected_by,omitempty"`
	ObservedEffectiveAt *time.Time `json:"observed_effective_at,omitempty"`
	ExpiresAt           *time.Time `json:"expires_at,omitempty"`
	Revision            int        `json:"revision"`
	Scope               Scope      `json:"scope"`
	Confidence          Confidence `json:"confidence"`
	Links               Links      `json:"links"`
}

type Notification struct {
	SchemaVersion int       `json:"schema_version"`
	ID            string    `json:"notification_id"`
	DetectedAt    time.Time `json:"detected_at"`
	Test          bool      `json:"test"`
	Event         Event     `json:"event"`
}

type CachedResponse struct {
	ETag string `json:"etag"`
	Body []byte `json:"body"`
}

type DeliveryResult struct {
	Success     bool
	Retryable   bool
	RateLimited bool
	RetryAfter  time.Duration
	StatusCode  int
	Error       string
	Duration    time.Duration
}

type Preview struct {
	Channel     string   `json:"channel"`
	Method      string   `json:"method"`
	Destination string   `json:"destination"`
	HeaderNames []string `json:"header_names"`
	BodyBytes   int      `json:"body_bytes"`
}

type ProviderStatus struct {
	Ready         bool       `json:"ready"`
	LastSuccess   *time.Time `json:"last_success,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	Pending       int        `json:"pending"`
	Failed        int        `json:"failed"`
	OldestPending *time.Time `json:"oldest_pending,omitempty"`
}

// ChannelStatus contains operational counts only, never recipient credentials.
type ChannelStatus struct {
	Pending       int        `json:"pending"`
	Failed        int        `json:"failed"`
	Delivered     int        `json:"delivered"`
	Canceled      int        `json:"canceled"`
	OldestPending *time.Time `json:"oldest_pending,omitempty"`
	CooldownUntil *time.Time `json:"cooldown_until,omitempty"`
}

type Status struct {
	UpdatedAt time.Time                 `json:"updated_at"`
	Running   bool                      `json:"running"`
	Providers map[string]ProviderStatus `json:"providers"`
	Channels  map[string]ChannelStatus  `json:"channels,omitempty"`
	Runtime   *RuntimeStatus            `json:"runtime,omitempty"`
	Pending   int                       `json:"pending"`
	Failed    int                       `json:"failed"`
}
