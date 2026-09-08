package config

import "reflect"

// FilterSpec is safe to persist or send to the local control service. It never
// contains transport settings, destinations, or credentials.
type FilterSpec struct {
	Providers         []ProviderFilter `yaml:"providers" json:"providers"`
	EventTypes        []string         `yaml:"event_types" json:"event_types"`
	MinimumConfidence string           `yaml:"minimum_confidence" json:"minimum_confidence"`
	UnknownScope      string           `yaml:"unknown_scope" json:"unknown_scope"`
}

func (c Config) FilterSpec() FilterSpec {
	return FilterSpec{c.Providers, c.EventTypes, c.MinimumConfidence, c.UnknownScope}
}

func (f FilterSpec) Config() Config {
	c := Defaults()
	c.Providers, c.EventTypes = f.Providers, f.EventTypes
	c.MinimumConfidence, c.UnknownScope = f.MinimumConfidence, f.UnknownScope
	return c
}

func (f FilterSpec) Validate() error { return Validate(f.Config()) }

// LoadFilterSpec evaluates only filter environment variables and references.
// A preview must not require credentials used by an unrelated destination.
func LoadFilterSpec(path string) (FilterSpec, error) {
	c, _, err := ReadRaw(path)
	if err != nil {
		return FilterSpec{}, err
	}
	f := c.FilterSpec()
	if err := apply(reflect.ValueOf(&f).Elem(), "", nil); err != nil {
		return f, err
	}
	return f, f.Validate()
}

// LoadManagement resolves only the location and filters needed for local
// history commands. Sending and offline retries still require a full Load.
func LoadManagement(path string, overrides map[string]string) (Config, error) {
	c, err := LoadStructural(path, overrides)
	if err != nil {
		return c, err
	}
	f := c.FilterSpec()
	if err := apply(reflect.ValueOf(&f).Elem(), "", overrides); err != nil {
		return c, err
	}
	if err := f.Validate(); err != nil {
		return c, err
	}
	c.Providers, c.EventTypes = f.Providers, f.EventTypes
	c.MinimumConfidence, c.UnknownScope = f.MinimumConfidence, f.UnknownScope
	return c, nil
}
