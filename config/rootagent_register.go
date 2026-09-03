package config

// DeregisterRootAgentsForRepo removes every root_agents opt-in that resolves to
// ANY of repoIDs and persists the result, returning the config keys it removed.
// It is the durable half of "delete a project" (#1735), so an emptied project
// does not linger in the picker as a zero-session opt-in after its sessions are
// archived, and does not respawn an always-on root on the next daemon start.
//
// A root_agents key is a repo path "as written" (a leading ~ is expanded), which
// may be a subdirectory or a non-canonical spelling of the repo root, so a key
// matches when its RESOLVED main-repo id equals one of repoIDs; when the path no
// longer resolves to a git repo (moved/removed), it falls back to hashing the
// expanded, cleaned path so a stale entry for a gone repo can still be swept.
// The write is load-modify-persist over the whole global config (no other key
// clobbered) and idempotent: no matching key is a clean no-op returning nil, nil.
//
// The whole sequence runs under the config file lock (#1838) so a concurrent
// config writer cannot be reverted by our snapshot. The key→repo resolution
// runs under the lock too: it decides which keys the write drops, so resolving
// it against a pre-lock snapshot could drop an entry a racing writer had just
// added.
func DeregisterRootAgentsForRepo(repoIDs ...string) ([]string, error) {
	var removed []string
	err := withGlobalConfigLock(func(locked lockedTarget) error {
		var err error
		removed, err = deregisterRootAgentsLocked(locked, repoIDs)
		return err
	})
	if err != nil {
		return nil, err
	}
	return removed, nil
}

// deregisterRootAgentsLocked is the body above minus the lock, so it can be
// driven against a pinned target directly — the shape applyGlobalUnset and
// migrateConfigFile already have. Callers must hold the config file lock.
func deregisterRootAgentsLocked(locked lockedTarget, repoIDs []string) ([]string, error) {
	cfg, err := loadConfigLocked(locked)
	if err != nil {
		return nil, err
	}
	var removed []string
	// One read-modify-write for EVERY identity: a re-attributed project
	// carries two (real and derived recorded-path), and sweeping them as
	// separate writes could remove one opt-in and then fail, breaking the
	// caller's nothing-was-changed guarantee (#3299 review round 7).
	for key := range cfg.RootAgents {
		for _, repoID := range repoIDs {
			if rootAgentKeyMatchesRepo(key, repoID) {
				removed = append(removed, key)
				break
			}
		}
	}
	if len(removed) == 0 {
		// The nothing-to-remove answer has to be about the file this lock
		// covers, and it is the one answer that never reaches the write that
		// would say so. It is also the costliest to get wrong: DeleteProject
		// reads nil here as "the durable cleanup succeeded" and goes on to
		// delete the project, so an opt-in surviving in the config the link now
		// names respawns that project on the next daemon start — the exact
		// outcome its own error message promises cannot happen
		// (daemon/deleteproject.go, #3696 review).
		if err := locked.confirm(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	for _, key := range removed {
		delete(cfg.RootAgents, key)
	}
	if err := saveConfigLocked(locked, cfg); err != nil {
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
