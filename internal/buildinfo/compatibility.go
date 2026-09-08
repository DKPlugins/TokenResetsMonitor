package buildinfo

// SchemaRange describes source schemas this release reads and the schema it writes.
type SchemaRange struct {
	Minimum int `json:"minimum"`
	Current int `json:"current"`
}

// Manifest declares schema and platform compatibility, not runtime availability.
type Manifest struct {
	ManifestVersion int         `json:"manifest_version"`
	Version         string      `json:"version"`
	Config          SchemaRange `json:"config"`
	State           SchemaRange `json:"state"`
	WebhookSchema   int         `json:"webhook_schema"`
	APISchemaMajor  int         `json:"api_schema_major"`
	Platforms       []string    `json:"platforms"`
}

func CurrentManifest() Manifest {
	return Manifest{ManifestVersion: 1, Version: Version,
		Config: SchemaRange{Minimum: 1, Current: 3}, State: SchemaRange{Minimum: 0, Current: 3},
		WebhookSchema: 1, APISchemaMajor: 1,
		Platforms: []string{"linux/amd64", "linux/arm64", "windows/amd64"}}
}
