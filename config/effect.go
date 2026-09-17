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
	// DaemonApplyUnconfirmed means this save cannot establish what the running
	// daemon ended up serving. It has three causes, and they share one
	// implication — the file was written, but the live value is unproven:
	//
	//   - a lost RPC response: the daemon may have applied the config before the
	//     connection failed, so neither success nor failure may be claimed;
	//   - an admission refusal (upgrade probation, quiescing), which says nothing
	//     about whether the apply ran;
	//   - a config DIGEST that does not match: the bytes the apply loaded are not
	//     the bytes this save wrote, so a competing write — another client, or a
	//     hand-edit — landed between the writer's file-lock release and the
	//     apply's load, and the daemon may be serving that value instead.
	//
	// The digest cause DELIBERATELY over-reports, and that is not a defect to be
	// fixed. It compares whole files, so it cannot tell "my key lost a race" from
	// "an unrelated key changed in the same window", and it reports the second as
	// unconfirmed too. That direction is the point: the error is always to
	// WITHHOLD a claim, never to make a false one. Narrowing it to "my key moved"
	// means reading the key's value back out of some store after the fact, which
	// is exactly the mechanism this replaced (#4247) — it needed store selection,
	// value normalisation, generation identity and file-readability handling, and
	// produced nine review findings doing it. An unrelated concurrent write is
	// rare; a save that falsely claims `applied` is not recoverable by the caller.
	//
	// So: do not "fix" an unrelated-key window into `applied`.
	DaemonApplyUnconfirmed bool
	// FailedListenerKeys names the socket keys (network.listen_addr /
	// network.preview_listen_addr) whose live rebind failed, so bind-new-before-close
	// left the OLD listener serving. Both daemon.ApplyConfigResult and
	// daemon.ApplyConfigResponse carry this; config owns no daemon types and must not
	// import daemon (daemon imports config), so it travels as the plain slice.
	FailedListenerKeys []string
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
)

// saveRule is one row of the ordering that turns a save's facts into what the
// save surfaces report. A row carries BOTH projections — the wire status and the
// sentence — so for any save StatusForKey and EffectNotice read the same row and
// cannot disagree.
//
// That is the point of the shape. The two used to be separate hand-written
// orderings, kept in step by a test that asserted they agreed, and review found
// them out of step four times (#4247). A disagreement is now unrepresentable
// rather than tested for: there is one ordering and two columns.
type saveRule struct {
	// applies reports whether this row describes the save; key is canonical.
	applies func(key string, o ApplyOutcome) bool
	status  ApplyStatus
	// notice renders the sentence for the canonical key.
	notice func(key string) string
}

// saveRules is the one precedence for a save's reported answer: the FIRST row
// that applies decides both the status and the sentence. The order is the
// substance, so each row records why it sits where it does. Several rows share a
// status and differ only in their sentence; that is a difference of projection,
// not of ordering, and it is why the rows are per meaning rather than per status.
var saveRules = []saveRule{
	// Uncertainty about a LIVE key outranks the rest: once the apply's answer is
	// lost, refused, or contradicted by the digest, nothing below may promise
	// what the running daemon is serving.
	//
	// It deliberately does not reach a DEFERRED key. Every cause of this bit
	// leaves the file written — a lost reply and a refusal both do, and a digest
	// mismatch says some file is there, just possibly not this one — so the next
	// daemon start or af launch still reads a config.toml, and the class rows
	// below stay true. A race at save time is also indistinguishable from a
	// hand-edit a minute later, which no save could have reported either.
	{
		applies: func(key string, o ApplyOutcome) bool {
			return o.DaemonApplyUnconfirmed && !deferredEffectClass(key)
		},
		status: ApplyStatusUnconfirmed,
		notice: staticNotice("Saved — the daemon’s live config apply could not be confirmed (see the warnings for the reason)."),
	},
	// A failed apply is evidence about the FILE: DaemonApplyFailed is set only for
	// a "reload config" failure (Manager.ApplyConfig's one error return), so the
	// file did not load — and the next start reads it. It therefore outranks the
	// class rows, which would promise an effect that file cannot deliver.
	{
		applies: func(_ string, o ApplyOutcome) bool { return o.DaemonApplyFailed },
		status:  ApplyStatusFailed,
		notice:  staticNotice("Saved — the running daemon could not apply the new configuration and is still using its previous value. Resolve the warning, then retry the save or restart the daemon before relying on the saved value."),
	},
	// The key's class. No apply can make these keys live, so an apply result that
	// says nothing about the file does not change when the stored value is used.
	{
		applies: classIs(EffectNextDaemonStart),
		status:  ApplyStatusDeferred,
		notice: func(key string) string {
			return WithRootAgentAdoptionNotice(key, "Saved — this setting takes effect on the next daemon start.")
		},
	},
	{
		applies: classIs(EffectNextAfLaunch),
		status:  ApplyStatusDeferred,
		notice:  staticNotice("Saved — this setting takes effect the next time you launch af."),
	},
	{
		applies: classIs(EffectUnknown),
		status:  ApplyStatusUnknown,
		notice:  staticNotice("Saved."),
	},
	// Only EffectAppliedLive reaches here. A failed rebind kept the old listener
	// serving, so the value waits for the next daemon start.
	{
		applies: func(key string, o ApplyOutcome) bool { return o.ListenerRebindFailed(key) },
		status:  ApplyStatusDeferred,
		notice:  listenerRebindDeferredNotice,
	},
	{
		applies: func(_ string, o ApplyOutcome) bool { return o.DaemonApplied },
		status:  ApplyStatusApplied,
		notice:  staticNotice("Applied — the running daemon is using the new value now."),
	},
	// The zero outcome: no daemon was reached. It always applies, so the table is
	// total and every save gets exactly one row.
	{
		applies: func(string, ApplyOutcome) bool { return true },
		status:  ApplyStatusNoDaemon,
		notice:  staticNotice("Saved — no daemon is running to apply it, so it takes effect on the next daemon start."),
	},
}

func staticNotice(sentence string) func(string) string {
	return func(string) string { return sentence }
}

func classIs(class EffectClass) func(string, ApplyOutcome) bool {
	return func(key string, _ ApplyOutcome) bool { return KeyEffectClass(key) == class }
}

// matchSaveRule returns the canonical key and the one row describing this save.
func matchSaveRule(key string, o ApplyOutcome) (string, saveRule) {
	key = canonicalConfigKey(key)
	for _, rule := range saveRules {
		if rule.applies(key, o) {
			return key, rule
		}
	}
	// Unreachable while the last row always applies; kept total rather than
	// panicking on a save surface.
	return key, saveRules[len(saveRules)-1]
}

// StatusForKey projects the whole apply onto one saved key's stable wire value.
// It is a column of saveRules, never a separate decision.
func (o ApplyOutcome) StatusForKey(key string) ApplyStatus {
	_, rule := matchSaveRule(key, o)
	return rule.status
}

// deferredEffectClass reports whether key's value is consumed at the next daemon
// start or af launch rather than by the running daemon. It is what lets an
// unconfirmed apply and a failed one rank differently against the class: an
// unconfirmed apply still wrote the file that start will read, while a failed one
// means that file did not load.
func deferredEffectClass(key string) bool {
	switch KeyEffectClass(key) {
	case EffectNextDaemonStart, EffectNextAfLaunch:
		return true
	}
	return false
}

// ListenerRebindFailed reports whether key is one of the socket keys whose live
// rebind failed in this apply. A failed rebind defers the key dynamically: the
// running daemon keeps the old listener and the value only reaches one at the
// next start, which reads the file.
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
func (o ApplyOutcome) ListenerRebindFailed(key string) bool {
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
	key, rule := matchSaveRule(key, outcome)
	return rule.notice(key)
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
