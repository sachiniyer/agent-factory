package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/log"
)

// inRepoHashFileName is the per-repo state file holding the sha256 of the
// last in-repo config content that was announced in the log. Observability
// only — it gates nothing.
const inRepoHashFileName = "inrepo-config-hash"

// legacyDeprecationLogged dedupes the legacy-location deprecation warning to
// once per repo per process.
var legacyDeprecationLogged sync.Map

// ResolvedConfig is the effective configuration for one repository: app
// defaults overlaid by the global ~/.agent-factory/config.json overlaid by
// the repo's own .agent-factory/config.json. Every consumer of per-repo
// configuration (programs, remote hooks, post-worktree commands) must go
// through ResolveConfig rather than reading LoadConfig/LoadRepoConfig
// directly, so the precedence and scoping rules apply uniformly.
type ResolvedConfig struct {
	// Config carries the effective app-level fields. DefaultProgram and
	// ProgramOverrides may have been overridden/merged from the in-repo
	// file; the global-only fields (AutoYes, DaemonPollInterval,
	// BranchPrefix, DetachKeys) always come from the global config because
	// LoadInRepoConfig rejects them per-repo.
	Config

	// PostWorktreeCommands are the effective post-worktree hooks: the
	// in-repo value when its key is present (even empty), otherwise the
	// legacy ~/.agent-factory/repos/<id>/config.json value.
	PostWorktreeCommands []string
	// RemoteHooks is the effective remote hook backend config, with the
	// same in-repo-then-legacy resolution as PostWorktreeCommands. Command
	// values that were relative filesystem paths have been rewritten to
	// absolute paths under repoRoot (#834); consumers can exec them without
	// caring about the process cwd.
	RemoteHooks *RemoteHooks
}

// ResolveConfig returns the effective configuration for the repository
// rooted at repoRoot (the main worktree root, as returned by
// config.RepoFromPath / CurrentRepo).
//
// Precedence is app defaults -> global config -> in-repo config, merged
// field-wise: an in-repo field overrides the lower layers only when set.
// program_overrides merges key-wise — an in-repo entry wins per agent,
// global entries without an in-repo counterpart still apply.
//
// remote_hooks and post_worktree_commands are in-repo-only going forward;
// values still in the legacy per-repo location keep working for one release
// (with a deprecation warning) and are shadowed whenever the in-repo file
// sets the same key.
func ResolveConfig(repoRoot string) (*ResolvedConfig, error) {
	global, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	res := &ResolvedConfig{Config: *global}
	// The overrides map is merged into below; copy it so a resolved config
	// never aliases (and can never mutate) the loaded global config.
	res.ProgramOverrides = make(map[string]string, len(global.ProgramOverrides))
	for k, v := range global.ProgramOverrides {
		res.ProgramOverrides[k] = v
	}

	repoID := RepoIDFromRoot(repoRoot)
	legacy, err := LoadRepoConfig(repoID)
	if err != nil {
		return nil, err
	}
	res.PostWorktreeCommands = legacy.PostWorktreeCommands
	res.RemoteHooks = legacy.RemoteHooks

	inRepo, raw, err := LoadInRepoConfig(repoRoot)
	if err != nil {
		return nil, err
	}
	if inRepo != nil {
		if inRepo.DefaultProgram != "" {
			res.DefaultProgram = inRepo.DefaultProgram
		}
		for k, v := range inRepo.ProgramOverrides {
			res.ProgramOverrides[k] = v
		}
		if inRepo.IsSet("post_worktree_commands") {
			res.PostWorktreeCommands = inRepo.PostWorktreeCommands
		}
		if inRepo.IsSet("remote_hooks") {
			res.RemoteHooks = inRepo.RemoteHooks
		}
		logInRepoConfigLoaded(repoID, repoRoot, inRepo, raw)
	}

	// Rewrite relative hook command paths to absolute against repoRoot
	// (#834). This is the single chokepoint for the rewrite: every exec of a
	// hook command — launch/list/attach/delete/terminal, startup import,
	// restore liveness, preview — receives its RemoteHooks from ResolveConfig, so
	// resolving here covers them all. repoRoot is the main worktree root, so
	// sessions in linked worktrees resolve hooks against the repository whose
	// config file was loaded, never against a worktree path. The rewrite
	// applies to the legacy-location value too, so both sources behave
	// identically.
	if res.RemoteHooks != nil {
		res.RemoteHooks = res.RemoteHooks.resolveCommandPaths(repoRoot)
	}

	warnLegacyRepoConfig(repoID, repoRoot, legacy, inRepo)
	return res, nil
}

// logInRepoConfigLoaded emits one INFO line the first time a command-bearing
// in-repo config is seen for a repo, and again whenever its content hash
// changes. The last-announced hash is tracked in the per-repo state dir
// purely to decide when to re-log; it gates nothing.
func logInRepoConfigLoaded(repoID, repoRoot string, inRepo *InRepoConfig, raw []byte) {
	fields := inRepo.CommandBearingFields()
	if len(fields) == 0 {
		return
	}
	hash := InRepoConfigHash(raw)
	dir, err := repoStateDir(repoID)
	if err != nil {
		log.WarningLog.Printf("failed to resolve state dir for repo %s: %v", repoID, err)
		return
	}
	hashPath := filepath.Join(dir, inRepoHashFileName)
	if prev, err := os.ReadFile(hashPath); err == nil && strings.TrimSpace(string(prev)) == hash {
		return
	}
	log.InfoLog.Printf("loaded in-repo config for %s: %s (sha256 %s)", repoRoot, strings.Join(fields, ", "), hash[:8])
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.WarningLog.Printf("failed to create repo state dir %s: %v", dir, err)
		return
	}
	if err := AtomicWriteFile(hashPath, []byte(hash+"\n"), 0644); err != nil {
		log.WarningLog.Printf("failed to record in-repo config hash for %s: %v", repoRoot, err)
	}
}

// warnLegacyRepoConfig logs (once per repo per process) when fields from the
// deprecated legacy location are still in effect — i.e. present there and not
// shadowed by the in-repo file. The legacy location keeps working for one
// release so existing setups don't break mid-upgrade (#800).
func warnLegacyRepoConfig(repoID, repoRoot string, legacy *RepoConfig, inRepo *InRepoConfig) {
	var fields []string
	if len(legacy.PostWorktreeCommands) > 0 && !inRepo.IsSet("post_worktree_commands") {
		fields = append(fields, "post_worktree_commands")
	}
	if legacy.RemoteHooks != nil && !inRepo.IsSet("remote_hooks") {
		fields = append(fields, "remote_hooks")
	}
	if len(fields) == 0 {
		return
	}
	if _, loaded := legacyDeprecationLogged.LoadOrStore(repoID, true); loaded {
		return
	}
	// Derive the legacy path from the same resolver the read uses
	// (repoConfigPath -> repoStateDir -> GetConfigDir) so the warning names
	// the real file even when AGENT_FACTORY_HOME relocates the config dir
	// (#890). If resolution fails, still warn but omit the now-unknown source
	// path rather than printing a wrong one.
	_, legacyPath, err := repoConfigPath(repoID)
	if err != nil {
		log.WarningLog.Printf("deprecated: %s is still read from the legacy per-repo config location; move it to %s — the legacy location stops working in a future release",
			strings.Join(fields, ", "), InRepoConfigPath(repoRoot))
		return
	}
	log.WarningLog.Printf("deprecated: %s is still read from %s; move it to %s — the legacy location stops working in a future release",
		strings.Join(fields, ", "), prettyHomePath(legacyPath), InRepoConfigPath(repoRoot))
}
