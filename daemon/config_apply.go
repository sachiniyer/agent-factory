package daemon

import (
	"fmt"
	"reflect"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/task"
)

// Config returns the current hot-reloadable global config (#2480). Each daemon
// operation must call this EXACTLY ONCE at its entry and thread the returned
// snapshot down: a per-use call inside one operation can straddle an ApplyConfig
// swap and observe two config generations, producing an inconsistent result (a
// branch derived from one generation, a worktree path from the next). The frozen
// startup config m.cfg is deliberately separate — it backs only the keys that do
// NOT hot-reload (root_agents, and the network listener keys until PR2).
func (m *Manager) Config() *config.Config {
	return m.live.Load()
}

// ApplyConfigResult reports the outcome of an in-place config apply (#2480), so a
// save surface can tell the user what took effect instead of telling them to
// restart.
type ApplyConfigResult struct {
	// Applied names the changed keys that took effect live — no restart, no
	// session loss.
	Applied []string
	// Pending names changed keys this daemon build cannot yet apply in place: the
	// network listener keys (a follow-up in-process listener reload lands them,
	// PR2) and root_agents (its next-daemon-start contract, carved out pending
	// #2216). They are written to disk and take effect on the next daemon start.
	Pending []string
}

// applyPendingKeys are the changed keys ApplyConfig cannot yet apply in place.
// listen_addr/preview_listen_addr/require_token/require_loopback_token/
// cors_allowed_origins are served by the listener that reads the FROZEN m.cfg
// until the PR2 in-process listener reload; root_agents is carved out (#2216).
var applyPendingKeys = map[string]func(a, b *config.Config) bool{
	"listen_addr":            func(a, b *config.Config) bool { return a.ListenAddr != b.ListenAddr },
	"preview_listen_addr":    func(a, b *config.Config) bool { return a.PreviewListenAddr != b.PreviewListenAddr },
	"require_token":          func(a, b *config.Config) bool { return a.RequireToken != b.RequireToken },
	"require_loopback_token": func(a, b *config.Config) bool { return a.RequireLoopbackToken != b.RequireLoopbackToken },
	"cors_allowed_origins":   func(a, b *config.Config) bool { return !reflect.DeepEqual(a.CORSAllowedOrigins, b.CORSAllowedOrigins) },
	"root_agents":            func(a, b *config.Config) bool { return !reflect.DeepEqual(a.RootAgents, b.RootAgents) },
}

// ApplyConfig re-reads the global config from disk and applies it to the running
// daemon in place (#2480): it swaps the live config so per-op keys pick it up at
// their next op entry, and re-arms the subsystems that snapshot config. It never
// restarts the daemon and never touches a running session. Keys the daemon
// cannot yet hot-apply are reported as Pending, not silently dropped.
//
// NOTE: the config was already written to disk by the caller (a save surface).
// ApplyConfig only makes the running daemon reflect it.
func (m *Manager) ApplyConfig() (ApplyConfigResult, error) {
	newCfg, err := config.LoadConfig()
	if err != nil {
		return ApplyConfigResult{}, fmt.Errorf("reload config: %w", err)
	}
	old := m.Config()

	var result ApplyConfigResult
	for key, changed := range applyPendingKeys {
		if changed(old, newCfg) {
			result.Pending = append(result.Pending, key)
		}
	}
	// Everything else that differs is applied live below.
	result.Applied = appliedLiveKeys(old, newCfg)

	// Swap the live config: per-op keys (default_program, branch_prefix,
	// session_env_passthrough, limit_auto_resume, limit_retry_interval) read it at
	// their next op entry.
	m.live.Store(newCfg)

	// limit_patterns snapshots at construction, so the swap alone would be a silent
	// no-op — rebuild the detector in place.
	det := task.NewLimitDetector(newCfg.LimitPatterns)
	m.limitDetector.Store(&det)

	// vscode_server_binary: the supervisor closure reads Config(), so the swap
	// above already reaches new spawns.

	// TODO(#2480 PR1, next slices): re-arm the poll ticker on a
	// daemon_poll_interval change and reconfigure the logger on a log_* change.

	return result, nil
}

// appliedLiveKeys names the hot-reloadable keys that differ between old and new.
// It is the inverse of applyPendingKeys over the keys #2480 PR1 applies in place.
func appliedLiveKeys(old, next *config.Config) []string {
	var keys []string
	add := func(name string, changed bool) {
		if changed {
			keys = append(keys, name)
		}
	}
	add("default_program", old.DefaultProgram != next.DefaultProgram)
	add("program_overrides", !reflect.DeepEqual(old.ProgramOverrides, next.ProgramOverrides))
	add("branch_prefix", old.BranchPrefix != next.BranchPrefix)
	add("worktree_root", old.WorktreeRoot != next.WorktreeRoot)
	add("session_env_passthrough", !reflect.DeepEqual(old.SessionEnvPassthrough, next.SessionEnvPassthrough))
	add("limit_auto_resume", old.LimitAutoResume != next.LimitAutoResume)
	add("limit_retry_interval", old.LimitRetryInterval != next.LimitRetryInterval)
	add("limit_patterns", !reflect.DeepEqual(old.LimitPatterns, next.LimitPatterns))
	add("vscode_server_binary", old.VSCodeServerBinary != next.VSCodeServerBinary)
	add("daemon_poll_interval", old.DaemonPollInterval != next.DaemonPollInterval)
	add("log_max_size_mb", old.LogMaxSizeMB != next.LogMaxSizeMB)
	add("log_max_backups", old.LogMaxBackups != next.LogMaxBackups)
	return keys
}
