package daemon

import (
	"fmt"
	"reflect"
	"sort"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/task"
)

// Config returns the current hot-reloadable global config (#2480). Each daemon
// operation must call this EXACTLY ONCE at its entry and thread the returned
// snapshot down: a per-use call inside one operation can straddle an ApplyConfig
// swap and observe two config generations, producing an inconsistent result (a
// branch derived from one generation, a worktree path from the next). The frozen
// startup config m.cfg is deliberately separate — it backs only the keys that do
// NOT hot-reload: root_agents/root_agent, branch_prefix (title-reservation
// helpers), and the network listener keys until PR2.
func (m *Manager) Config() *config.Config {
	if c := m.live.Load(); c != nil {
		return c
	}
	// A manager built directly as &Manager{cfg: …} (some tests, and any path that
	// skips newManagerShellForDaemon) never seeds live; fall back to the frozen
	// startup config so Config() is never nil.
	return m.cfg
}

// ApplyConfigResult reports the outcome of an in-place config apply (#2480), so a
// save surface can tell the user what took effect instead of telling them to
// restart.
type ApplyConfigResult struct {
	// Applied names the changed keys that took effect live — no restart, no
	// session loss.
	Applied []string
	// Pending names changed keys this daemon build reads only at startup, so they
	// take effect on the next daemon start: the network listener keys (a follow-up
	// in-process listener reload lands them, PR2), root_agents (its
	// next-daemon-start contract, carved out pending #2216), and branch_prefix
	// (read from the FROZEN startup config in the title-reservation helpers).
	Pending []string
}

// keyDiff maps every config key the daemon reads to a predicate reporting whether
// it CHANGED between two configs. Which bucket a changed key lands in — Applied
// (live now) versus Pending (next daemon start) — is NOT decided here: it is
// config.KeyEffectClass, the single source of truth the save-surface notice reads
// too (config/effect.go). So a key cannot be bucketed one way for the daemon and
// described another way to the user. Client-side keys (update_channel, theme, …)
// are absent because the daemon never reads them; their notice is class-driven.
var keyDiff = map[string]func(a, b *config.Config) bool{
	// EffectAppliedLive keys.
	"default_program":   func(a, b *config.Config) bool { return a.DefaultProgram != b.DefaultProgram },
	"program_overrides": func(a, b *config.Config) bool { return !reflect.DeepEqual(a.ProgramOverrides, b.ProgramOverrides) },
	"session_env_passthrough": func(a, b *config.Config) bool {
		return !reflect.DeepEqual(a.SessionEnvPassthrough, b.SessionEnvPassthrough)
	},
	"worktree_root":                  func(a, b *config.Config) bool { return a.WorktreeRoot != b.WorktreeRoot },
	"vscode_server_binary":           func(a, b *config.Config) bool { return a.VSCodeServerBinary != b.VSCodeServerBinary },
	"daemon_poll_interval":           func(a, b *config.Config) bool { return a.DaemonPollInterval != b.DaemonPollInterval },
	"log_max_size_mb":                func(a, b *config.Config) bool { return a.LogMaxSizeMB != b.LogMaxSizeMB },
	"log_max_backups":                func(a, b *config.Config) bool { return a.LogMaxBackups != b.LogMaxBackups },
	"limit_auto_resume":              func(a, b *config.Config) bool { return a.LimitAutoResume != b.LimitAutoResume },
	"limit_retry_interval":           func(a, b *config.Config) bool { return a.LimitRetryInterval != b.LimitRetryInterval },
	"limit_patterns":                 func(a, b *config.Config) bool { return !reflect.DeepEqual(a.LimitPatterns, b.LimitPatterns) },
	"global_agent_skills":            func(a, b *config.Config) bool { return a.GlobalAgentSkills != b.GlobalAgentSkills },
	"docker_mount_agent_credentials": func(a, b *config.Config) bool { return a.DockerMountAgentCredentials != b.DockerMountAgentCredentials },
	"ssh_host_key_verification":      func(a, b *config.Config) bool { return a.SSHHostKeyVerification != b.SSHHostKeyVerification },
	// EffectNextDaemonStart keys — read once at startup.
	"listen_addr":            func(a, b *config.Config) bool { return a.ListenAddr != b.ListenAddr },
	"preview_listen_addr":    func(a, b *config.Config) bool { return a.PreviewListenAddr != b.PreviewListenAddr },
	"require_token":          func(a, b *config.Config) bool { return a.RequireToken != b.RequireToken },
	"require_loopback_token": func(a, b *config.Config) bool { return a.RequireLoopbackToken != b.RequireLoopbackToken },
	"cors_allowed_origins":   func(a, b *config.Config) bool { return !reflect.DeepEqual(a.CORSAllowedOrigins, b.CORSAllowedOrigins) },
	"root_agents":            func(a, b *config.Config) bool { return !reflect.DeepEqual(a.RootAgents, b.RootAgents) },
	"root_agent":             func(a, b *config.Config) bool { return !reflect.DeepEqual(a.RootAgent, b.RootAgent) },
	"branch_prefix":          func(a, b *config.Config) bool { return a.BranchPrefix != b.BranchPrefix },
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

	// Bucket every changed key by its effect class (config.KeyEffectClass, the same
	// source the save-surface notice reads). Sorted so the reported order is stable
	// across map iterations.
	var result ApplyConfigResult
	for key, changed := range keyDiff {
		if !changed(old, newCfg) {
			continue
		}
		switch config.KeyEffectClass(key) {
		case config.EffectAppliedLive:
			result.Applied = append(result.Applied, key)
		case config.EffectNextDaemonStart:
			result.Pending = append(result.Pending, key)
		}
	}
	sort.Strings(result.Applied)
	sort.Strings(result.Pending)

	// Swap the live config: per-op keys (default_program, session_env_passthrough,
	// limit_auto_resume, limit_retry_interval, …) read it at their next op entry.
	// branch_prefix rides along in the swapped config but is read from the frozen
	// m.cfg in the title-reservation helpers, so it stays Pending, not Applied.
	m.live.Store(newCfg)

	// limit_patterns snapshots at construction, so the swap alone would be a silent
	// no-op — rebuild the detector in place.
	det := task.NewLimitDetector(newCfg.LimitPatterns)
	m.limitDetector.Store(&det)

	// vscode_server_binary: the supervisor closure reads Config(), so the swap
	// above already reaches new spawns.

	// daemon_poll_interval: re-arm the poll ticker in place. Non-blocking; the
	// buffered channel collapses a burst of applies to one ticker reset.
	if old.DaemonPollInterval != newCfg.DaemonPollInterval {
		select {
		case m.pollReloadCh <- struct{}{}:
		default:
		}
	}
	// log_max_size_mb / log_max_backups: reconfigure the active log file in place.
	if old.LogMaxSizeMB != newCfg.LogMaxSizeMB || old.LogMaxBackups != newCfg.LogMaxBackups {
		log.ReconfigureRotation()
	}

	return result, nil
}
