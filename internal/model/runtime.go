package model

import "time"

// RuntimeStatus contains operational metadata only; configuration secrets never enter snapshots.
type RuntimeStatus struct {
	Version              string        `json:"version"`
	Commit               string        `json:"commit"`
	ConfigVersion        int           `json:"config_version"`
	StateSchemaVersion   int           `json:"state_schema_version"`
	PollIntervalSeconds  float64       `json:"poll_interval_seconds"`
	ConfigGeneration     uint64        `json:"config_generation"`
	ReloadPending        bool          `json:"reload_pending"`
	LastReloadSuccessful bool          `json:"last_reload_successful"`
	LastReloadAt         *time.Time    `json:"last_reload_at,omitempty"`
	ReloadError          string        `json:"reload_error,omitempty"`
	Updates              *UpdateStatus `json:"updates,omitempty"`
}

type UpdateStatus struct {
	CheckedAt       time.Time `json:"checked_at"`
	CurrentVersion  string    `json:"current_version"`
	LatestVersion   string    `json:"latest_version,omitempty"`
	ReleaseURL      string    `json:"release_url,omitempty"`
	UpdateAvailable bool      `json:"update_available"`
	Compatibility   string    `json:"compatibility"`
	Error           string    `json:"error,omitempty"`
}
