package config

// DeregisterRootAgentsForRepo removes every root_agents opt-in that resolves to
// repoID and persists the result, returning the config keys it removed. It is
// the durable half of "delete a project" (#1735), so an emptied project does not
// linger in the picker as a zero-session opt-in after its sessions are
// archived, and does not respawn an always-on root on the next daemon start.
//
// A root_agents key is a repo path "as written" (a leading ~ is expanded), which
// may be a subdirectory or a non-canonical spelling of the repo root, so a key
// matches when its RESOLVED main-repo id equals repoID; when the path no longer
// resolves to a git repo (moved/removed), it falls back to hashing the expanded,
// cleaned path so a stale entry for a gone repo can still be swept. The write is
// load-modify-persist over the whole global config (no other key clobbered) and
// idempotent: no matching key is a clean no-op returning nil, nil.
//
// The whole sequence runs under the config file lock (#1838) so a concurrent
// config writer cannot be reverted by our snapshot. The key→repo resolution
// runs under the lock too: it decides which keys the write drops, so resolving
// it against a pre-lock snapshot could drop an entry a racing writer had just
// added.
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
	return RepoIDForRecordedRoot(expanded) == repoID
}
