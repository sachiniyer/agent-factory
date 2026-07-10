package config

// RegisterRootAgent opts repoRoot into the global root_agents list with the
// default agent profile, unless it is already present. It is the persistence
// side of the TUI's in-place "+ Add project…" flow (#1461): root_agents is the
// existing, durable "af has seen this repo" store, so a project added at runtime
// still appears in the picker on the next launch.
//
// The write is load-modify-persist over the whole global config so no other key
// is clobbered, and idempotent: a repo already registered is a no-op that
// reports added=false, so the caller can avoid a needless config rewrite. The
// key is the repo's absolute main-worktree root (callers resolve it via
// RepoFromPath first), matching how EnsureRootAgents resolves the map keys.
//
// Registering here does not itself spawn an always-on root agent: the daemon
// reads root_agents at startup, so the always-ensure behavior only takes effect
// on the next daemon start. Switching to the repo works immediately regardless,
// because Snapshot/CreateSession resolve any repo path live.
func RegisterRootAgent(repoRoot string) (bool, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return false, err
	}
	if _, exists := cfg.RootAgents[repoRoot]; exists {
		return false, nil
	}
	if cfg.RootAgents == nil {
		cfg.RootAgents = map[string]RootAgentConfig{}
	}
	cfg.RootAgents[repoRoot] = RootAgentConfig{}
	if err := SaveConfig(cfg); err != nil {
		return false, err
	}
	return true, nil
}
