package config

import (
	"path/filepath"
	"sort"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
)

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
	err := withGlobalConfigLock(func() error {
		cfg, err := loadConfigLocked()
		if err != nil {
			return err
		}
		removed = nil
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
	// CANONICAL role (#3530): the question is "does this key name that
	// repository", so an unresolvable key is compared by hashing the path the
	// same way a real identity is derived. Inventing a namespaced id here would
	// make a stale entry for a gone repo unsweepable.
	cleaned := filepath.Clean(expanded)
	if RepoIDFromRoot(cleaned) == repoID {
		return true
	}
	// …and again through the key's CANONICAL spelling (#3530 review id
	// 3918120733). The caller derives its id from a path the registry
	// resolved, while this key was written by a human through whatever symlink
	// they had — `/private/var/...` against `/var/...` on macOS — so the
	// lexical hashes differ and a stale opt-in survives a delete that reported
	// success. Additive: a key that matched before still matches, and identity
	// DERIVATION is untouched, because an id is defined by the exact recorded
	// string and canonicalizing there would re-key durable state.
	return RepoIDFromRoot(pathutil.ResolveForCompare(cleaned)) == repoID
}

// LegacyRootAgentForRecordedRoot returns the root_agents entry spelled as a
// registered project's RECORDED root (and the matched key), while that root
// does not resolve (#3530 review ids 3916912933, 3917294309, 3917756780).
//
// A root_agents key is a path, and rootAgentKeyMatchesRepo falls back to
// hashing one it cannot resolve — which is nobody's identity once a registered
// project is addressed by the identity it RECORDED rather than by its path.
// Master matched such an entry by accident, because it addressed that project
// BY the path hash; both the daemon's verdict and `af config get --explain`
// have to ask for it deliberately now, and they share this so the running
// daemon and the explanation of the next start cannot disagree.
//
// Two steps, and neither may be collapsed into the other. The key is found by
// its SPELLING, with no resolver involved: hashing the path and asking a
// resolver matches a repository main-rooted there, whose identity IS that hash,
// so the occupant's opt-in would be returned for this project. Then the
// recorded root is resolved ONCE: if it resolves, the entry belongs to whatever
// is there now — the ensure sweep will create under that identity, not this
// one — so it is not this project's answer.
func LegacyRootAgentForRecordedRoot(global *Config, recordedRoot string) (*RootAgentConfig, string) {
	if global == nil || recordedRoot == "" {
		return nil, ""
	}
	cleaned := filepath.Clean(recordedRoot)
	// Both sides go through ResolveForCompare, because the spellings genuinely
	// differ: a root_agents key is written by a human — through whatever
	// symlink they had — while the record stores the path registration
	// resolved. Comparing Clean-ed strings makes those unequal wherever the
	// temp or working root is itself a symlink, which is macOS `/var` ->
	// `/private/var` every time (#2110's rule; caught by CI's darwin job on
	// #3530). The recorded root does not exist in the case this function is
	// FOR, so plain EvalSymlinks cannot be used on either side.
	target := pathutil.ResolveForCompare(cleaned)
	// Sorted for the same reason LegacyRootAgentForRepo sorts: inspection, the
	// daemon lookup and the ensure pass must agree on one winner when two
	// spellings name the same root.
	keys := make([]string, 0, len(global.RootAgents))
	for key := range global.RootAgents {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	matched := ""
	for _, key := range keys {
		if pathutil.ResolveForCompare(filepath.Clean(ExpandTilde(key))) == target {
			matched = key
			break
		}
	}
	if matched == "" {
		return nil, ""
	}
	// The probe must ANSWER before its outcome may be acted on (#3530 review id
	// 3918379034, #3500's rule): a killed or unstartable git establishes
	// nothing, and a repository occupying this path owns the key — reporting
	// the stale project's opt-in then promises a root the ensure sweep will
	// only ever create for the occupant. Uncertainty withholds the fallback.
	if _, err := RepoFromPath(cleaned); err == nil || RepoProbeUnanswered(err) {
		return nil, ""
	}
	entry := global.RootAgents[matched]
	return &entry, matched
}
