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
	// (auto_update and update_channel are read by the updater; keys and detach_keys
	// by the TUI). A notice for these must NOT imply a running daemon
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
	"default_program":   EffectAppliedLive,
	"program_overrides": EffectAppliedLive,
	// default_accounts is read per create, from the daemon's live config snapshot
	// and the per-repo resolution beside it, so a change reaches the next session
	// with no restart (#3386).
	"default_accounts":               EffectAppliedLive,
	"session_env_passthrough":        EffectAppliedLive,
	"on_archive_command":             EffectAppliedLive,
	"worktree_root":                  EffectAppliedLive,
	"vscode_server_binary":           EffectAppliedLive,
	"daemon_poll_interval":           EffectAppliedLive,
	"log_max_size_mb":                EffectAppliedLive,
	"log_max_backups":                EffectAppliedLive,
	"limit_auto_resume":              EffectAppliedLive,
	"limit_account_candidates":       EffectAppliedLive,
	"limit_retry_interval":           EffectAppliedLive,
	"limit_patterns":                 EffectAppliedLive,
	"global_agent_skills":            EffectAppliedLive,
	"docker.mount_agent_credentials": EffectAppliedLive,
	"ssh.host_key_verification":      EffectAppliedLive,
	"sandbox.ssh":                    EffectAppliedLive,
	// The network listener keys apply live since #2480 PR2: require_token /
	// require_loopback_token / cors_allowed_origins are read per request
	// (livePosture), and listen_addr / preview_listen_addr rebind in place
	// (bind-new-before-close). A listen_addr rebind that FAILS is surfaced as a
	// warning and reported as deferred to the next daemon start — the class here is
	// the success case; the runtime outcome overrides the notice on failure.
	"network.listen_addr":            EffectAppliedLive,
	"network.preview_listen_addr":    EffectAppliedLive,
	"network.require_token":          EffectAppliedLive,
	"network.require_loopback_token": EffectAppliedLive,
	"network.cors_allowed_origins":   EffectAppliedLive,
	// Next daemon start — the daemon reads these once, at startup.
	"root_agents":   EffectNextDaemonStart,
	"root_agent":    EffectNextDaemonStart,
	"branch_prefix": EffectNextDaemonStart,
	// The watcher supervisor snapshots this cap when the daemon constructs it;
	// existing supervisors are not rebuilt by ApplyConfig.
	"watcher_events_per_minute": EffectNextDaemonStart,
	// debug_pprof selects the daemon's route table, which is built once when the
	// HTTP listeners bind (daemon/httpserver.go). Nothing re-reads it per request,
	// so a save must say "next daemon start" rather than claim it took effect.
	"debug_pprof": EffectNextDaemonStart,
	// Next af launch — af's CLI/TUI owns what these control. The daemon's release
	// check reads auto_update and update_channel (#2212), but it only reports a
	// release; installing is still af's, at launch. See EffectNextAfLaunch.
	"appearance":     EffectNextAfLaunch,
	"auto_update":    EffectNextAfLaunch,
	"update_channel": EffectNextAfLaunch,
	// Read at the moment an upgrade is attempted — by `af upgrade` from disk, and
	// by the daemon from its live config on each activation — so a save is in
	// force for the next upgrade either path tries, with nothing to restart.
	"upgrade_clear_unverifiable_artifacts": EffectAppliedLive,
	"keys":                                 EffectNextAfLaunch,
	"detach_keys":                          EffectNextAfLaunch,
}

// KeyEffectClass returns when a change to key takes effect. A dotted family leaf
// (program_overrides.claude, limit_patterns.foo) is classified by its base key,
// since the daemon applies the whole map.
func KeyEffectClass(key string) EffectClass {
	key = canonicalConfigKey(key)
	if class, ok := keyEffectClasses[key]; ok {
		return class
	}
	base := key
	if i := strings.IndexByte(key, '.'); i >= 0 {
		base = key[:i]
	}
	return keyEffectClasses[base]
}

// ApplyOutcome is what a running daemon actually DID with the write a save surface
// is about to report — the WHOLE outcome, not the single "did the apply call
// return nil" bit EffectNotice used to take.
//
// That bit was the defect behind #3397. A listener key has three outcomes, not two:
// applied live, deferred because no daemon was running, and applied-on-disk but
// NOT live because the socket rebind failed and the old listener is still serving.
// The third one was inexpressible here, so each save surface had to reconstruct it
// from daemon.ApplyConfigResult with its own branch — and a decision that lives at
// the call site is a decision the next surface can be written without. Two of the
// four surfaces were (set, server and client); two were not (unset, server and
// client), and the unset pair printed "Applied — the running daemon is using the
// new value now." over a warning saying the daemon was still serving the old
// address. Carrying the outcome makes EffectNotice the one owner of the decision.
//
// The zero value means no daemon was reached to apply the save. An apply
// failure must set DaemonApplyFailed instead of claiming that no daemon ran.
type ApplyOutcome struct {
	// DaemonApplied reports that a running daemon applied the on-disk config
	// (daemon.Manager.ApplyConfig returned without error). It does NOT report that
	// every changed key took effect — FailedListenerKeys is the rest of the answer.
	DaemonApplied bool
	// DaemonApplyFailed means the daemon returned a failure response instead of
	// applying the saved config.
	DaemonApplyFailed bool
	// DaemonApplyUnconfirmed distinguishes a lost RPC response from a daemon
	// error: the daemon may have applied the config before the connection failed.
	DaemonApplyUnconfirmed bool
	// FailedListenerKeys names the socket keys (network.listen_addr /
	// network.preview_listen_addr) whose live rebind failed, so bind-new-before-close
	// left the OLD listener serving. Both daemon.ApplyConfigResult and
	// daemon.ApplyConfigResponse carry this; config owns no daemon types and must not
	// import daemon (daemon imports config), so it travels as the plain slice.
	FailedListenerKeys []string
	// SavedValueSuperseded is set when the apply completed but the key's live
	// value is not the one this save wrote: a competing write landed between the
	// save's file-lock release and the apply's load, so the apply carried the
	// other value (#4247). Meaningful only alongside DaemonApplied.
	SavedValueSuperseded bool
}

// ApplyStatus is the machine-readable result for the key a save wrote. A
// completed ApplyConfig can still be deferred for one listener key whose rebind
// failed, so this is deliberately key-specific rather than merely the RPC's
// success bit.
type ApplyStatus string

const (
	// ApplyStatusUnknown is what a newer client reports when an older daemon's
	// response predates the additive apply_outcome field.
	ApplyStatusUnknown     ApplyStatus = "unknown"
	ApplyStatusApplied     ApplyStatus = "applied"
	ApplyStatusNoDaemon    ApplyStatus = "no_daemon"
	ApplyStatusFailed      ApplyStatus = "failed"
	ApplyStatusUnconfirmed ApplyStatus = "unconfirmed"
	ApplyStatusDeferred    ApplyStatus = "deferred"
	// ApplyStatusSuperseded reports a save whose write reached disk but lost the
	// race to the apply: a newer write landed in between, so the daemon is not
	// serving the value this save reported.
	ApplyStatusSuperseded ApplyStatus = "superseded"
)

// StatusForKey projects the whole apply onto one saved key's stable wire value.
//
// Precedence, and why it is this order: a lost race outranks everything, because
// it is the one answer about which VALUE IS STORED rather than about what the
// apply did with it. Below that, the key's effect class outranks the apply
// result, since an apply cannot make a startup-only key live and an unconfirmed
// or failed apply does not change when that stored value starts being used.
// Within the apply result, uncertainty wins over failure if a malformed caller
// sets both: once the reply is lost, the client cannot honestly claim the daemon
// kept its previous config.
func (o ApplyOutcome) StatusForKey(key string) ApplyStatus {
	// Ahead of the class switch, because losing the race is a statement about
	// WHICH VALUE IS STORED and the class only describes when a stored value
	// starts being used. A startup-only key is raced on disk exactly like a live
	// one, and "deferred" would promise that THIS save takes effect at the next
	// daemon start while the value waiting there belongs to the writer that won
	// (#4247). Only a readback that resolved the key sets this, so it cannot
	// fire for a key whose value was never actually compared.
	if o.SavedValueSuperseded {
		return ApplyStatusSuperseded
	}
	// Match EffectNotice's key-first rule. A startup-only setting is deferred
	// regardless of whether a daemon happened to receive this save; that apply
	// call cannot make the key live. The same holds for client-side settings,
	// which take effect on the next af launch rather than in the daemon.
	switch KeyEffectClass(key) {
	case EffectNextDaemonStart, EffectNextAfLaunch:
		return ApplyStatusDeferred
	case EffectUnknown:
		return ApplyStatusUnknown
	}
	if o.DaemonApplyUnconfirmed {
		return ApplyStatusUnconfirmed
	}
	if o.DaemonApplyFailed {
		return ApplyStatusFailed
	}
	if o.listenerRebindFailed(key) {
		return ApplyStatusDeferred
	}
	if o.DaemonApplied {
		return ApplyStatusApplied
	}
	return ApplyStatusNoDaemon
}

// listenerRebindFailed reports whether key is one of the socket keys whose live
// rebind failed in this apply.
//
// Both sides are canonicalized, which today is belt and braces: every producer of
// FailedListenerKeys is a hardcoded canonical literal in webListeners.reconcile,
// and both config.SetResult.Key and config.UnsetResult.Key are canonicalConfigKey'd
// before the result is built, so an `af config set listen_addr …` already arrives
// here as "network.listen_addr". But "both sides happen to be canonical" is an
// invariant spread across three files with nothing pinning it, and it is exactly
// the invariant a raw comparison would fail silently — printing "Applied" over a
// rebind warning, which is the bug this function exists to prevent. A map lookup is
// cheaper than the standing risk.
func (o ApplyOutcome) listenerRebindFailed(key string) bool {
	key = canonicalConfigKey(key)
	for _, failed := range o.FailedListenerKeys {
		if canonicalConfigKey(failed) == key {
			return true
		}
	}
	return false
}

// EffectNotice is the one sentence a save surface shows after writing key, stating
// WHEN the change takes effect. outcome is what the running daemon did with it (see
// ApplyOutcome); it only changes the applied-live answer, because an applied-live
// key that no daemon was running to apply waits for the next daemon start just like
// the deferred keys.
//
// It deliberately never tells the user to run a command (#2479), never claims a
// running daemon acted on a key the daemon does not read (#2480), and since #3397
// never claims a key is live when the rebind that would have made it live failed.
// Sentence case, one clause set off with an em dash, per the copy conventions.
func EffectNotice(key string, outcome ApplyOutcome) string {
	key = canonicalConfigKey(key)
	// This prefix mirrors StatusForKey's precedence deliberately. The two answer the
	// same question for the same save — one as prose, one as a wire status — so an
	// ordering that differs between them lets a save be reported as `superseded`
	// while its sentence promises the value takes effect.
	//
	// A lost race comes first, because it is the one answer about WHICH value is
	// stored, and it contradicts every "takes effect" promise below — including the
	// rebind-deferred one.
	if outcome.SavedValueSuperseded {
		return supersededNotice(key)
	}
	// Then the class, exactly as StatusForKey does. An apply result says nothing
	// about a key the apply cannot make live, so a startup-only or client-side key
	// is deferred whether or not the apply was confirmed — the value is already on
	// disk, which is what the next start reads.
	switch KeyEffectClass(key) {
	case EffectNextDaemonStart:
		notice := "Saved — this setting takes effect on the next daemon start."
		return WithRootAgentAdoptionNotice(key, notice)
	case EffectNextAfLaunch:
		return "Saved — this setting takes effect the next time you launch af."
	case EffectUnknown:
		return "Saved."
	}
	// EffectAppliedLive from here, in StatusForKey's order: uncertainty, then
	// failure, then a rebind that kept the old listener. The rebind sits below the
	// first two because both can accompany it on the version-skewed fallback, where
	// FailedListenerKeys comes from an apply that SUCCEEDED while the post-apply file
	// read could not confirm which value the daemon loaded.
	if outcome.DaemonApplyUnconfirmed {
		return "Saved — the daemon’s live config apply could not be confirmed. See warnings for details."
	}
	if outcome.DaemonApplyFailed {
		return "Saved — the running daemon could not apply the new configuration and is still using its previous value. Resolve the warning, then retry the save or restart the daemon before relying on the saved value."
	}
	if outcome.listenerRebindFailed(key) {
		return listenerRebindDeferredNotice(key)
	}
	if outcome.DaemonApplied {
		return "Applied — the running daemon is using the new value now."
	}
	return "Saved — no daemon is running to apply it, so it takes effect on the next daemon start."
}

// supersededNotice is the one sentence for a save that lost a race, worded for
// what the key's class makes the stored value mean. A live key's loser is about
// what the daemon is serving now; a deferred key's loser is about what the next
// daemon start or af launch will read. Both must avoid the "takes effect"
// promise, which would be made about the winner's value rather than this save's.
func supersededNotice(key string) string {
	switch KeyEffectClass(key) {
	case EffectNextDaemonStart:
		return "Saved — a newer write raced this save, so the value waiting for the next daemon start is not the one this save wrote."
	case EffectNextAfLaunch:
		return "Saved — a newer write raced this save, so the value waiting for the next af launch is not the one this save wrote."
	default:
		return "Saved — a newer write raced this save, so the running daemon may be using a different value."
	}
}

// WithRootAgentAdoptionNotice appends the half of restart guidance unique to
// always-ensured roots. Global and per-project save surfaces use this one copy.
func WithRootAgentAdoptionNotice(key, notice string) string {
	key = canonicalConfigKey(key)
	if key != "root_agent" && !strings.HasPrefix(key, "root_agent.") && key != "root_agents" {
		return notice
	}
	return notice + " · An already-running root session is adopted as-is, so after changing its program, disabling it, or removing its enabling entry, restart the daemon first and then kill that session."
}

// listenerRebindDeferredNotice is the honest notice when a network.listen_addr /
// network.preview_listen_addr change could NOT be applied to the running daemon:
// the bind-new-before-close rebind failed, so the OLD listener is still serving. The
// value is on disk and takes effect on the next daemon start; the actionable
// reason (address + why) rides alongside in the save surface's warnings. Like every
// #2480 notice it names no command to run.
//
// Unexported since #3397. It was exported for the save surfaces to call directly,
// which is precisely how two of them came to decide this correctly and two not at
// all. EffectNotice above is now the only way to reach it, so no surface can pick
// the wrong sentence — or forget that this sentence exists. The wording itself is
// unchanged; it is user-visible.
func listenerRebindDeferredNotice(key string) string {
	return fmt.Sprintf("Saved — %s could not be applied to the running daemon; it takes effect on the next daemon start (see the warning for the reason).", key)
}
