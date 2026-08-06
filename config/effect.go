package config

import (
	"fmt"
	"strings"
)

// EffectClass says WHEN a change to a config key takes effect. #2480 makes this a
// per-key fact rather than the old uniform "restart to apply": a save surface
// (`af config set`, the TUI pane, the web form) must report the honest answer for
// the key the user just wrote instead of one canned sentence. The whole point of
// #2480 is that af stop claiming a restart is needed when it is not — and, just as
// important, stop implying a running daemon acted on a key the daemon never reads.
type EffectClass int

const (
	// EffectUnknown is the zero value: a key with no classification. Every manifest
	// key must map to one of the classes below (TestEveryManifestKeyHasAnEffectClass),
	// so Unknown means a key was added without deciding when its change takes effect.
	EffectUnknown EffectClass = iota
	// EffectAppliedLive: a running daemon honours the new value WITHOUT a restart —
	// either immediately (its own poll cadence, log rotation, or usage-limit
	// matchers) or on its next daemon-driven operation (the next session or worktree
	// it creates reads the live config). Manager.ApplyConfig is what makes this true
	// (daemon/config_apply.go); with no daemon running there is nothing to apply, so
	// EffectNotice downgrades the message to the next daemon start.
	EffectAppliedLive
	// EffectNextDaemonStart: the daemon reads the key, but only at startup, so a
	// change waits for the next daemon start. root_agents / root_agent (their
	// next-daemon-start contract, #2216) and branch_prefix (read from the FROZEN
	// startup config in the title-reservation helpers — deliberately not threaded
	// live, so it is reported here rather than falsely claimed applied) are these.
	// (The network listener keys used to be here; #2480 PR2 made them applied-live.)
	EffectNextDaemonStart
	// EffectNextAfLaunch: nothing a running daemon does with the key changes what
	// the user just asked to change — af's own CLI or TUI does, on its next launch
	// (auto_update and update_channel are read by the updater; theme, keys, and
	// detach_keys by the TUI). A notice for these must NOT imply a running daemon
	// applied the change, because the thing the key controls is not the daemon's to
	// apply.
	//
	// auto_update and update_channel are the near miss: since #2212 the daemon's
	// release check reads both from its live config, so flipping auto_update off
	// does stop the daemon checking within a wake. But that check only REPORTS a
	// release — the installer is still af's, at launch — so next-af-launch remains
	// the honest answer for what the user set, and it errs toward promising less
	// than happens rather than claiming a daemon acted. The slice that lets the
	// daemon install owns re-deciding this.
	EffectNextAfLaunch
)

// keyEffectClasses is the ONE place a config key's effect timing is decided. The
// daemon's applied-vs-pending buckets derive from it (daemon/config_apply.go, and
// TestApplyBucketsAgreeWithEffectClasses pins the agreement), so the daemon and
// the save-surface notice cannot disagree about when a key takes effect.
//
// Every settable manifest key appears here; TestEveryManifestKeyHasAnEffectClass
// fails if one is added without a decision.
var keyEffectClasses = map[string]EffectClass{
	// Applied live — a running daemon honours these without a restart, either in its
	// own behaviour or on the next session/worktree it creates.
	"default_program":                EffectAppliedLive,
	"program_overrides":              EffectAppliedLive,
	"session_env_passthrough":        EffectAppliedLive,
	"on_archive_command":             EffectAppliedLive,
	"worktree_root":                  EffectAppliedLive,
	"vscode_server_binary":           EffectAppliedLive,
	"daemon_poll_interval":           EffectAppliedLive,
	"log_max_size_mb":                EffectAppliedLive,
	"log_max_backups":                EffectAppliedLive,
	"limit_auto_resume":              EffectAppliedLive,
	"limit_retry_interval":           EffectAppliedLive,
	"limit_patterns":                 EffectAppliedLive,
	"global_agent_skills":            EffectAppliedLive,
	"docker_mount_agent_credentials": EffectAppliedLive,
	"ssh_host_key_verification":      EffectAppliedLive,
	"sandbox_ssh":                    EffectAppliedLive,
	// The network listener keys apply live since #2480 PR2: require_token /
	// require_loopback_token / cors_allowed_origins are read per request
	// (livePosture), and listen_addr / preview_listen_addr rebind in place
	// (bind-new-before-close). A listen_addr rebind that FAILS is surfaced as a
	// warning and reported as deferred to the next daemon start — the class here is
	// the success case; the runtime outcome overrides the notice on failure.
	"listen_addr":            EffectAppliedLive,
	"preview_listen_addr":    EffectAppliedLive,
	"require_token":          EffectAppliedLive,
	"require_loopback_token": EffectAppliedLive,
	"cors_allowed_origins":   EffectAppliedLive,
	// Next daemon start — the daemon reads these once, at startup.
	"root_agents":   EffectNextDaemonStart,
	"root_agent":    EffectNextDaemonStart,
	"branch_prefix": EffectNextDaemonStart,
	// Next af launch — af's CLI/TUI owns what these control. The daemon's release
	// check reads auto_update and update_channel (#2212), but it only reports a
	// release; installing is still af's, at launch. See EffectNextAfLaunch.
	"auto_update":    EffectNextAfLaunch,
	"update_channel": EffectNextAfLaunch,
	"theme":          EffectNextAfLaunch,
	"keys":           EffectNextAfLaunch,
	"detach_keys":    EffectNextAfLaunch,
}

// KeyEffectClass returns when a change to key takes effect. A dotted family leaf
// (program_overrides.claude, limit_patterns.foo) is classified by its base key,
// since the daemon applies the whole map.
func KeyEffectClass(key string) EffectClass {
	base := key
	if i := strings.IndexByte(key, '.'); i >= 0 {
		base = key[:i]
	}
	return keyEffectClasses[base]
}

// EffectNotice is the one sentence a save surface shows after writing key, stating
// WHEN the change takes effect. daemonApplied reports whether a running daemon just
// applied the on-disk config (Manager.ApplyConfig succeeded); it only changes the
// applied-live answer, because an applied-live key that no daemon was running to
// apply waits for the next daemon start just like the deferred keys.
//
// It deliberately never tells the user to run a command (#2479) and never claims a
// running daemon acted on a key the daemon does not read (#2480). Sentence case,
// one clause set off with an em dash, per the copy conventions.
func EffectNotice(key string, daemonApplied bool) string {
	switch KeyEffectClass(key) {
	case EffectAppliedLive:
		if daemonApplied {
			return "Applied — the running daemon is using the new value now."
		}
		return "Saved — no daemon is running to apply it, so it takes effect on the next daemon start."
	case EffectNextDaemonStart:
		return "Saved — this setting takes effect on the next daemon start."
	case EffectNextAfLaunch:
		return "Saved — this setting takes effect the next time you launch af."
	default:
		return "Saved."
	}
}

// ListenerRebindDeferredNotice is the honest notice when a listen_addr /
// preview_listen_addr change could NOT be applied to the running daemon: the
// bind-new-before-close rebind failed, so the OLD listener is still serving. The
// value is on disk and takes effect on the next daemon start; the actionable
// reason (address + why) rides alongside in the save surface's warnings. Like every
// #2480 notice it names no command to run.
func ListenerRebindDeferredNotice(key string) string {
	return fmt.Sprintf("Saved — %s could not be applied to the running daemon; it takes effect on the next daemon start (see the warning for the reason).", key)
}
