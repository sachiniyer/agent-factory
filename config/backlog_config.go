package config

// BacklogConfig is the [backlog] table of config.toml.
type BacklogConfig struct {
	// Prices adds or replaces the API price of a Claude model, keyed by model
	// id, for metering backlog runs.
	Prices map[string]ModelPrice `json:"prices,omitempty" toml:"prices,omitempty"`
}

// ModelPrice is a model's API price in US dollars per million tokens. A zero
// cache price is derived from Input.
type ModelPrice struct {
	Input        float64 `json:"input" toml:"input"`
	CacheWrite5m float64 `json:"cache_write_5m,omitempty" toml:"cache_write_5m,omitempty"`
	CacheWrite1h float64 `json:"cache_write_1h,omitempty" toml:"cache_write_1h,omitempty"`
	CacheRead    float64 `json:"cache_read,omitempty" toml:"cache_read,omitempty"`
	Output       float64 `json:"output" toml:"output"`
}
