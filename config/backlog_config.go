package config

// BacklogConfig is the [backlog] table of config.toml.
type BacklogConfig struct {
	// PlanUsage turns on plan-usage metering for the backlog: af adds a status
	// line tap to the Claude sessions it launches (so Claude reports its plan
	// usage to af) and reads the Claude transcripts of the accounts it meters.
	// Off by default; when off, none of that happens and the runner has no plan
	// budget to gate on. Global config only: a repository cannot turn it on.
	PlanUsage bool `json:"plan_usage,omitempty" toml:"plan_usage,omitempty"`
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
