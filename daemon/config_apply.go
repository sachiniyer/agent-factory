package daemon

import (
	"fmt"
	"reflect"
	"sort"

	"github.com/sachiniyer/agent-factory/agentproto"
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
// NOT hot-reload: root_agents/root_agent and branch_prefix (title-reservation
// helpers). The network listener keys used to read m.cfg too; #2480 PR2 made them
// applied-live (livePosture per request; network.listen_addr/network.preview_listen_addr rebind in
// place), so they no longer do.
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
	// take effect on the next daemon start: root_agents / root_agent (their
	// next-daemon-start contract, carved out pending #2216) and branch_prefix (read
	// from the FROZEN startup config in the title-reservation helpers).
	Pending []string
	// Warnings are operator/user-facing notices produced while applying (#2480 PR2):
	// the tokenless-network exposure notice (#2168 — warn, never refuse) and a
	// listener rebind failure (bind-new-before-close kept the OLD listener serving,
	// so the requested network.listen_addr / network.preview_listen_addr did NOT take effect). A save
	// surface shows these so the user learns when a socket key did not apply.
	Warnings []string
	// FailedListenerKeys names the socket keys (network.listen_addr / network.preview_listen_addr)
	// whose live rebind failed, so a save surface can word THAT key's notice as
	// deferred rather than falsely "applied". The auth/CORS keys never appear here —
	// they are live-read and cannot fail to apply.
	FailedListenerKeys []string
}

// keyDiff maps every config key the daemon reads to a predicate reporting whether
// it CHANGED between two configs. Which bucket a changed key lands in — Applied
// (live now) versus Pending (next daemon start) — is NOT decided here: it is
// config.KeyEffectClass, the single source of truth the save-surface notice reads
// too (config/effect.go). So a key cannot be bucketed one way for the daemon and
// described another way to the user. Client-only keys (update_channel, keys, …)
// are absent because the daemon never reads them; their notice is class-driven.
var keyDiff = map[string]func(a, b *config.Config) bool{
	// EffectAppliedLive keys.
	"default_program":   func(a, b *config.Config) bool { return a.DefaultProgram != b.DefaultProgram },
	"program_overrides": func(a, b *config.Config) bool { return !reflect.DeepEqual(a.ProgramOverrides, b.ProgramOverrides) },
	"session_env_passthrough": func(a, b *config.Config) bool {
		return !reflect.DeepEqual(a.SessionEnvPassthrough, b.SessionEnvPassthrough)
	},
	"on_archive_command":             func(a, b *config.Config) bool { return a.OnArchiveCommand != b.OnArchiveCommand },
	"worktree_root":                  func(a, b *config.Config) bool { return a.WorktreeRoot != b.WorktreeRoot },
	"vscode_server_binary":           func(a, b *config.Config) bool { return a.VSCodeServerBinary != b.VSCodeServerBinary },
	"daemon_poll_interval":           func(a, b *config.Config) bool { return a.DaemonPollInterval != b.DaemonPollInterval },
	"log_max_size_mb":                func(a, b *config.Config) bool { return a.LogMaxSizeMB != b.LogMaxSizeMB },
	"log_max_backups":                func(a, b *config.Config) bool { return a.LogMaxBackups != b.LogMaxBackups },
	"limit_auto_resume":              func(a, b *config.Config) bool { return a.LimitAutoResume != b.LimitAutoResume },
	"limit_retry_interval":           func(a, b *config.Config) bool { return a.LimitRetryInterval != b.LimitRetryInterval },
	"limit_patterns":                 func(a, b *config.Config) bool { return !reflect.DeepEqual(a.LimitPatterns, b.LimitPatterns) },
	"global_agent_skills":            func(a, b *config.Config) bool { return a.GlobalAgentSkills != b.GlobalAgentSkills },
	"docker.mount_agent_credentials": func(a, b *config.Config) bool { return a.DockerMountAgentCredentials != b.DockerMountAgentCredentials },
	"ssh.host_key_verification":      func(a, b *config.Config) bool { return a.SSHHostKeyVerification != b.SSHHostKeyVerification },
	"sandbox.ssh":                    func(a, b *config.Config) bool { return a.SandboxSSH != b.SandboxSSH },
	"theme":                          func(a, b *config.Config) bool { return !reflect.DeepEqual(a.Theme, b.Theme) },
	// Network listener keys — applied-live since #2480 PR2: the auth/CORS keys are
	// read per request (livePosture); network.listen_addr / network.preview_listen_addr rebind the
	// socket in place (webListeners.reconcile, below in ApplyConfig).
	"network.listen_addr":            func(a, b *config.Config) bool { return a.ListenAddr != b.ListenAddr },
	"network.preview_listen_addr":    func(a, b *config.Config) bool { return a.PreviewListenAddr != b.PreviewListenAddr },
	"network.require_token":          func(a, b *config.Config) bool { return a.RequireToken != b.RequireToken },
	"network.require_loopback_token": func(a, b *config.Config) bool { return a.RequireLoopbackToken != b.RequireLoopbackToken },
	"network.cors_allowed_origins":   func(a, b *config.Config) bool { return !reflect.DeepEqual(a.CORSAllowedOrigins, b.CORSAllowedOrigins) },
	// EffectNextDaemonStart keys — read once at startup.
	"root_agents":   func(a, b *config.Config) bool { return !reflect.DeepEqual(a.RootAgents, b.RootAgents) },
	"root_agent":    func(a, b *config.Config) bool { return !reflect.DeepEqual(a.RootAgent, b.RootAgent) },
	"branch_prefix": func(a, b *config.Config) bool { return a.BranchPrefix != b.BranchPrefix },
	// debug_pprof: the pprof mount is decided when startHTTPServer builds the unix
	// listener's handler, so a change is reported pending rather than applied.
	"debug_pprof": func(a, b *config.Config) bool { return a.DebugPprof != b.DebugPprof },
}

// ApplyConfig re-reads the global config from disk and applies it to the running
// daemon in place (#2480): it swaps the live config so per-op keys pick it up at
// their next op entry, and re-arms the subsystems that snapshot config. It never
// restarts the daemon and never touches a running session. Keys the daemon
// cannot yet hot-apply are reported as Pending, not silently dropped.
//
// NOTE: the config was already written to disk by the caller (a save surface).
// ApplyConfig only makes the running daemon reflect it.
// withoutKeys returns keys minus drop, preserving order. Small n (at most the two
// listener keys), so a linear scan beats building a set.
func withoutKeys(keys, drop []string) []string {
	if len(drop) == 0 {
		return keys
	}
	kept := keys[:0]
	for _, k := range keys {
		remove := false
		for _, d := range drop {
			if k == d {
				remove = true
				break
			}
		}
		if !remove {
			kept = append(kept, k)
		}
	}
	return kept
}

// ApplyTheme advances only the daemon-owned palette from the global config. A
// plain `af` launch calls this after resolving a hand edit: applying the complete
// file there would also mutate listeners/auth and could hide a failed rebind from
// the user who merely opened the TUI. The event is an invalidation signal; open
// web clients fetch GetTheme after receiving it.
func (m *Manager) ApplyTheme() (bool, error) {
	m.configApplyMu.Lock()
	defer m.configApplyMu.Unlock()

	newCfg, err := config.LoadConfig()
	if err != nil {
		return false, fmt.Errorf("reload config theme: %w", err)
	}
	old := m.Config()
	if old == nil {
		return false, fmt.Errorf("reload config theme: daemon config is unavailable")
	}
	if old.Theme == newCfg.Theme {
		return false, nil
	}

	next := *old
	next.Theme = newCfg.Theme
	m.live.Store(&next)
	m.publishEvent(agentproto.EventThemeChanged, nil)
	return true, nil
}

func (m *Manager) ApplyConfig() (ApplyConfigResult, error) {
	m.configApplyMu.Lock()
	defer m.configApplyMu.Unlock()

	newCfg, err := config.LoadConfig()
	if err != nil {
		return ApplyConfigResult{}, fmt.Errorf("reload config: %w", err)
	}
	old := m.Config()
	themeChanged := old != nil && old.Theme != newCfg.Theme

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
	// branch_prefix rides along in the swapped config, but its runtime consumers
	// read frozen m.cfg so an unrelated apply cannot advance that generation behind
	// the next-start notice. GetTheme deliberately reads this live snapshot.
	m.live.Store(newCfg)
	if themeChanged {
		m.publishEvent(agentproto.EventThemeChanged, nil)
	}

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

	// Network listener keys (#2480 PR2). The auth/CORS keys already apply — their
	// handlers read the swapped live config per request, so network.require_token /
	// network.require_loopback_token / network.cors_allowed_origins take effect on the next request
	// with no action here, and DECOUPLED from any rebind (a security tightening lands
	// whether or not a socket rebind succeeds). Only a network.listen_addr / network.preview_listen_addr
	// change is a socket operation: reconcile rebinds it bind-new-before-close. A
	// rebind that fails keeps the OLD listener serving and is reported deferred with
	// the reason — never silently dropped.
	if m.webListeners != nil {
		if failed, rerr := m.webListeners.reconcile(newCfg); rerr != nil {
			result.Warnings = append(result.Warnings, rerr.Error())
			result.FailedListenerKeys = append(result.FailedListenerKeys, failed...)
			// And take them OUT of Applied (#3030). Applied is populated by effect
			// CLASS, before the rebind is attempted, so a key that failed to bind was
			// still being reported as live — the one field an operator reads to answer
			// "did my change take effect" saying yes while the daemon serves the old
			// address.
			//
			// Reporting it in Warnings and FailedListenerKeys is not a substitute:
			// a consumer that renders Applied is rendering a claim, and a claim that
			// contradicts the two fields beside it is worse than one that is merely
			// incomplete. Three outcomes, not two — applied, deferred to the next
			// start, and attempted-but-not-in-effect, which is what these keys are.
			result.Applied = withoutKeys(result.Applied, failed)
		}
	}
	// Turning network.require_token OFF voids the premise every outstanding sandbox callback
	// credential was issued under (#3012 review).
	//
	// mintSandboxCallback REFUSES to issue one while network.require_token is false, on the
	// grounds that a scoped credential against a listener that authenticates nobody
	// "manufactures the appearance of a boundary that nothing enforces". That check
	// runs once, at provision time — and per the block above, an auth key applies
	// live with no rebind, so this relaxation silently converts every already-issued
	// credential into exactly the thing that refusal exists to prevent: authGate
	// short-circuits on the tokenless posture before it ever consults the registry,
	// so the scope stops being enforced for callers that had one.
	//
	// Revoked and WARNED rather than refused. Refusing would make an operator's own
	// config unwritable because of sessions they may not remember provisioning, and
	// this repo does not hold a config key hostage to runtime state. But af must
	// also stop claiming a scope it is no longer enforcing, so the credentials go
	// and the operator is told what they just widened.
	//
	// This does NOT restore the boundary, and the warning says so: with network.require_token
	// false the control plane answers every caller anonymously, so a sandbox does not
	// need a credential to reach it. Only re-enabling the key restores it.
	if old.RequireToken && !newCfg.RequireToken {
		if n := m.sandboxTokens.revokeAll(); n > 0 {
			warning := fmt.Sprintf("network.require_token is now false: revoked %d sandbox callback credential(s), because a scoped credential enforces nothing against a listener that authenticates nobody. Those sessions lose callback. NOTE: this does not re-isolate them — the control plane now answers unauthenticated callers, provisioned sandboxes included; re-enable network.require_token to restore the boundary", n)
			log.WarningLog.Printf("%s", warning)
			result.Warnings = append(result.Warnings, warning)
		}
	}
	// The tokenless-network exposure notice, surfaced at SAVE time so a user who
	// makes the control API reachable without a token is told once — warned, never
	// refused (#2168 Phase 0 / config/authposture.go).
	if notice := config.ListenerExposureNotice(newCfg); notice != "" {
		result.Warnings = append(result.Warnings, notice)
	}

	return result, nil
}
