package config

import "path/filepath"

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

// DeregisterRootAgentsForRepo removes every root_agents opt-in that resolves to
// repoID and persists the result, returning the config keys it removed. It is
// the durable half of "delete a project" (#1735): the inverse of
// RegisterRootAgent, so an emptied project does not linger in the picker as a
// zero-session opt-in after its sessions are archived, and does not respawn an
// always-on root on the next daemon start.
//
// A root_agents key is a repo path "as written" (a leading ~ is expanded), which
// may be a subdirectory or a non-canonical spelling of the repo root, so a key
// matches when its RESOLVED main-repo id equals repoID; when the path no longer
// resolves to a git repo (moved/removed), it falls back to hashing the expanded,
// cleaned path so a stale entry for a gone repo can still be swept. The write is
// load-modify-persist over the whole global config (no other key clobbered) and
// idempotent: no matching key is a clean no-op returning nil, nil.
func DeregisterRootAgentsForRepo(repoID string) ([]string, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	var removed []string
	for key := range cfg.RootAgents {
		if rootAgentKeyMatchesRepo(key, repoID) {
			removed = append(removed, key)
		}
	}
	if len(removed) == 0 {
		return nil, nil
	}
	for _, key := range removed {
		delete(cfg.RootAgents, key)
	}
	if err := SaveConfig(cfg); err != nil {
		return nil, err
	}
	return removed, nil
}

// rootAgentKeyMatchesRepo reports whether a root_agents config key names the
// repo identified by repoID. It prefers resolving the key to its main-repo id
// (so a subdirectory or worktree spelling still matches), and falls back to
// hashing the expanded/cleaned path when the repo no longer resolves.
func rootAgentKeyMatchesRepo(key, repoID string) bool {
	expanded := ExpandTilde(key)
	if repo, err := RepoFromPath(expanded); err == nil {
		return repo.ID == repoID
	}
	return RepoIDFromRoot(filepath.Clean(expanded)) == repoID
}
