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
// The whole sequence runs under the config file lock (#1838): the persist
// re-marshals the entire Config, so reading outside the lock would let a
// concurrent `af config set` land between this load and this write and be
// silently reverted by our stale snapshot.
//
// Registering here does not itself spawn an always-on root agent: the daemon
// reads root_agents at startup, so the always-ensure behavior only takes effect
// on the next daemon start. Switching to the repo works immediately regardless,
// because Snapshot/CreateSession resolve any repo path live.
func RegisterRootAgent(repoRoot string) (bool, error) {
	var added bool
	err := withGlobalConfigLock(func() error {
		cfg, err := loadConfigLocked()
		if err != nil {
			return err
		}
		if _, exists := cfg.RootAgents[repoRoot]; exists {
			return nil
		}
		if cfg.RootAgents == nil {
			cfg.RootAgents = map[string]RootAgentConfig{}
		}
		cfg.RootAgents[repoRoot] = RootAgentConfig{}
		if rootAgentSaveRaceHookForTest != nil {
			rootAgentSaveRaceHookForTest()
		}
		if err := saveConfigLocked(cfg); err != nil {
			return err
		}
		added = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return added, nil
}

// rootAgentSaveRaceHookForTest, when non-nil, runs inside the config file-lock
// body of RegisterRootAgent, between the re-read and the persist — the window
// in which an unlocked writer used to be able to land a whole-file write that
// this function's stale snapshot then reverted (#1838). Tests use it to drive a
// concurrent `af config set` into exactly that window and pin that the lock now
// holds it off until the persist completes.
var rootAgentSaveRaceHookForTest func()

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
//
// Like RegisterRootAgent, the whole sequence runs under the config file lock
// (#1838) so a concurrent config writer cannot be reverted by our snapshot. The
// key→repo resolution runs under the lock too: it decides which keys the write
// drops, so resolving it against a pre-lock snapshot could drop an entry a
// racing writer had just added.
func DeregisterRootAgentsForRepo(repoID string) ([]string, error) {
	var removed []string
	err := withGlobalConfigLock(func() error {
		cfg, err := loadConfigLocked()
		if err != nil {
			return err
		}
		removed = nil
		for key := range cfg.RootAgents {
			if rootAgentKeyMatchesRepo(key, repoID) {
				removed = append(removed, key)
			}
		}
		if len(removed) == 0 {
			return nil
		}
		for _, key := range removed {
			delete(cfg.RootAgents, key)
		}
		return saveConfigLocked(cfg)
	})
	if err != nil {
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
