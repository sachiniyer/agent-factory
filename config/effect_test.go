package config

import (
	"strings"
	"testing"
)

// TestEveryManifestKeyHasAnEffectClass fails the moment a config key is added
// without deciding WHEN its change takes effect (#2480). An unclassified key falls
// to EffectUnknown, whose notice is a bare "Saved." — the vague answer this
// feature exists to replace with a per-key one.
func TestEveryManifestKeyHasAnEffectClass(t *testing.T) {
	for _, e := range Manifest() {
		if KeyEffectClass(e.Key) == EffectUnknown {
			t.Errorf("config key %q has no EffectClass — classify it in keyEffectClasses", e.Key)
		}
	}
}

// TestEffectNoticeIsPerKeyAndHonest pins the three contracts the notice must keep,
// each of which the old single canned sentence broke:
//   - an applied-live key a running daemon accepted says it is live NOW;
//   - a next-daemon-start key says exactly that, and not that it is live now;
//   - a client-side key points at the next af launch and NEVER mentions a daemon,
//     because none read it.
func TestEffectNoticeIsPerKeyAndHonest(t *testing.T) {
	applied := EffectNotice("default_program", ApplyOutcome{DaemonApply: DaemonApplyApplied})
	if !strings.Contains(applied, "using the new value now") {
		t.Errorf("applied-live notice should say it is live now, got %q", applied)
	}

	pending := EffectNotice("branch_prefix", ApplyOutcome{DaemonApply: DaemonApplyApplied})
	if !strings.Contains(pending, "next daemon start") {
		t.Errorf("next-daemon-start notice should defer to the next daemon start, got %q", pending)
	}
	if strings.Contains(pending, "using the new value now") {
		t.Errorf("next-daemon-start notice must not claim it is live now, got %q", pending)
	}

	client := EffectNotice("update_channel", ApplyOutcome{DaemonApply: DaemonApplyApplied})
	if !strings.Contains(client, "launch af") {
		t.Errorf("client-side notice should point at the next af launch, got %q", client)
	}
	if strings.Contains(client, "daemon") {
		t.Errorf("client-side notice must not mention a daemon — none reads it — got %q", client)
	}
}

// TestEffectNoticeDowngradesAppliedLiveWithoutADaemon: an applied-live key still
// waits for the next daemon start when no daemon was running to apply it, so the
// CLI on a box with no daemon does not claim a change is live.
func TestEffectNoticeDowngradesAppliedLiveWithoutADaemon(t *testing.T) {
	n := EffectNotice("default_program", ApplyOutcome{DaemonApply: DaemonApplyNotReached})
	if strings.Contains(n, "using the new value now") {
		t.Errorf("with no daemon, applied-live must not claim it is live now, got %q", n)
	}
	if !strings.Contains(n, "next daemon start") {
		t.Errorf("with no daemon, applied-live should defer to the next daemon start, got %q", n)
	}
}

// TestKeyEffectClassClassifiesDottedLeavesByBase: a dynamic family leaf
// (program_overrides.claude) inherits its base key's class, since the daemon
// applies the whole map.
func TestKeyEffectClassClassifiesDottedLeavesByBase(t *testing.T) {
	if KeyEffectClass("program_overrides.claude") != EffectAppliedLive {
		t.Errorf("a dotted leaf should inherit its base key's class")
	}
}

// TestRootAgentEffectNoticeNamesLiveSessionAdoption is issue #4087's missing
// half: restarting applies the frozen profile, but it still adopts an existing
// root unchanged, so the save notice must name both actions a program edit needs.
func TestRootAgentEffectNoticeNamesLiveSessionAdoption(t *testing.T) {
	for _, key := range []string{"root_agent", "root_agent.enabled", "root_agent.program", "root_agents"} {
		notice := EffectNotice(key, ApplyOutcome{DaemonApply: DaemonApplyApplied})
		if !strings.HasPrefix(notice, "Saved — this setting takes effect on the next daemon start.") {
			t.Errorf("%s lost the existing next-start sentence: %q", key, notice)
		}
		if !strings.Contains(notice, " · ") ||
			!strings.Contains(notice, "already-running root session is adopted as-is") ||
			!strings.Contains(notice, "changing its program") ||
			!strings.Contains(notice, "disabling it") ||
			!strings.Contains(notice, "removing its enabling entry") {
			t.Errorf("%s does not name the live-root adoption requirement: %q", key, notice)
		}
	}
}

// TestEffectNoticeReportsAFailedListenerRebindAsDeferred is the #3397 contract: a
// key whose live rebind FAILED is not live, whatever its effect class says, because
// bind-new-before-close left the old listener serving. The notice must not
// contradict the warning the same save surface prints beside it.
func TestEffectNoticeReportsAFailedListenerRebindAsDeferred(t *testing.T) {
	for _, key := range []string{"network.listen_addr", "network.preview_listen_addr"} {
		n := EffectNotice(key, ApplyOutcome{DaemonApply: DaemonApplyApplied, FailedListenerKeys: []string{key}})
		if strings.Contains(n, "using the new value now") {
			t.Errorf("%s: a failed rebind must never be reported as live, got %q", key, n)
		}
		if !strings.Contains(n, "could not be applied to the running daemon") {
			t.Errorf("%s: a failed rebind should report deferred, got %q", key, n)
		}
		if !strings.Contains(n, key) {
			t.Errorf("%s: the deferred notice should name the key, got %q", key, n)
		}
	}
}

// TestEffectNoticeKeepsTheDeferredSentenceVerbatim pins the user-visible string.
// #3397 moved WHO decides to emit it; the sentence itself must not drift, and this
// is the only place it is written down.
func TestEffectNoticeKeepsTheDeferredSentenceVerbatim(t *testing.T) {
	const want = "Saved — network.listen_addr could not be applied to the running daemon; " +
		"it takes effect on the next daemon start (see the warning for the reason)."
	got := EffectNotice("network.listen_addr", ApplyOutcome{
		DaemonApply: DaemonApplyApplied, FailedListenerKeys: []string{"network.listen_addr"},
	})
	if got != want {
		t.Errorf("deferred notice changed\n got: %q\nwant: %q", got, want)
	}
}

// TestEffectNoticeMatchesFailedListenerKeysAcrossAliasSpellings settles the latent
// question in #3397 rather than assuming it. `af config set listen_addr …` is the
// permanent flat alias for network.listen_addr, and unset removes BOTH spellings, so
// a raw comparison between the written key and the failed-rebind list would print
// "Applied" over a rebind warning the moment the two sides disagreed.
//
// They cannot disagree on master — every FailedListenerKeys entry is a hardcoded
// canonical literal in webListeners.reconcile, and SetResult.Key / UnsetResult.Key
// are both canonicalConfigKey'd before the result is built. But that invariant lives
// in three files and nothing held it in place, so the comparison canonicalizes both
// sides and this pins it from either direction.
func TestEffectNoticeMatchesFailedListenerKeysAcrossAliasSpellings(t *testing.T) {
	for _, tc := range []struct{ name, key, failed string }{
		{"written key is the legacy alias", "listen_addr", "network.listen_addr"},
		{"failed key is the legacy alias", "network.listen_addr", "listen_addr"},
		{"both are the legacy alias", "listen_addr", "listen_addr"},
		{"preview, written as the alias", "preview_listen_addr", "network.preview_listen_addr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := EffectNotice(tc.key, ApplyOutcome{DaemonApply: DaemonApplyApplied, FailedListenerKeys: []string{tc.failed}})
			if strings.Contains(n, "using the new value now") {
				t.Errorf("alias spelling must not defeat the rebind-failure check, got %q", n)
			}
			if !strings.Contains(n, "could not be applied to the running daemon") {
				t.Errorf("expected the deferred notice, got %q", n)
			}
			if !strings.Contains(n, CanonicalConfigKey(tc.key)) {
				t.Errorf("the notice should name the canonical key, got %q", n)
			}
		})
	}
}

// TestEffectNoticeIgnoresAnUnrelatedFailedListenerKey is the other half of the
// guard: a rebind failure on one socket key must not make every other key in the
// same apply report deferred. Only the key that failed did not take effect.
func TestEffectNoticeIgnoresAnUnrelatedFailedListenerKey(t *testing.T) {
	outcome := ApplyOutcome{DaemonApply: DaemonApplyApplied, FailedListenerKeys: []string{"network.listen_addr"}}
	for _, key := range []string{"network.preview_listen_addr", "network.require_token", "default_program"} {
		n := EffectNotice(key, outcome)
		if !strings.Contains(n, "using the new value now") {
			t.Errorf("%s applied fine in this apply and should still report live, got %q", key, n)
		}
	}
}

// TestEffectNoticeAppliedLiveSurvivesASuccessfulRebind: with no failed keys the
// applied-live answer is unchanged, so the fix cannot have turned every save into a
// deferred report.
func TestEffectNoticeAppliedLiveSurvivesASuccessfulRebind(t *testing.T) {
	n := EffectNotice("network.listen_addr", ApplyOutcome{DaemonApply: DaemonApplyApplied})
	if !strings.Contains(n, "using the new value now") {
		t.Errorf("a successful rebind must still report the change as live, got %q", n)
	}
}

// TestEffectNoticeNotReachedIsTheDaemonlessSentence: a save that recorded "no
// daemon was reached" gets the pre-#3397 sentence verbatim. The outcome must
// SAY not-reached — since #4482 the zero ApplyOutcome is an unrecorded result
// (unknown), not this answer.
func TestEffectNoticeNotReachedIsTheDaemonlessSentence(t *testing.T) {
	const want = "Saved — no daemon is running to apply it, so it takes effect on the next daemon start."
	if got := EffectNotice("network.listen_addr", ApplyOutcome{DaemonApply: DaemonApplyNotReached}); got != want {
		t.Errorf("daemonless notice changed\n got: %q\nwant: %q", got, want)
	}
}

// TestEffectNoticeUnsetOutcomeClaimsNothing is the #4482 zero-value contract:
// an ApplyOutcome no producer filled in reports unknown and claims nothing
// about the daemon — not the no-daemon sentence above, which a forgotten
// assignment would otherwise print as fact.
func TestEffectNoticeUnsetOutcomeClaimsNothing(t *testing.T) {
	got := EffectNotice("network.listen_addr", ApplyOutcome{})
	if strings.Contains(got, "no daemon is running") || strings.Contains(got, "takes effect") {
		t.Errorf("an unrecorded apply result must not report a daemon state or promise an effect, got %q", got)
	}
	if !strings.Contains(got, "was never recorded") {
		t.Errorf("an unrecorded apply result should admit it, got %q", got)
	}
}

// A confirmed reload failure keeps the previous live configuration; a lost
// response cannot establish that fact. Neither outcome means no daemon ran.
func TestEffectNoticeDaemonApplyFailed(t *testing.T) {
	const want = "Saved — the running daemon could not apply the new configuration and is still using its previous value. Resolve the warning, then retry the save or restart the daemon before relying on the saved value."
	for _, key := range []string{"network.require_token", "require_token", "default_program"} {
		if got := EffectNotice(key, ApplyOutcome{DaemonApply: DaemonApplyFailed}); got != want {
			t.Errorf("%s: got %q, want %q", key, got, want)
		}
	}
}

// A listener key can carry BOTH a failed rebind and an unconfirmed apply: the
// rebind failure rides on an apply that SUCCEEDED, so nothing stops the file
// from having moved under the load that apply made. The rebind-deferred sentence
// promises the save takes effect at the next daemon start, which would be a
// promise about whatever bytes the winner left there. EffectNotice must
// therefore rank these the way StatusForKey does, or the sentence and the wire
// status describe different worlds for one save (#4247).
func TestEffectNoticeRanksUnconfirmedAboveAFailedRebind(t *testing.T) {
	const key = "network.listen_addr"
	rebindPromise := listenerRebindDeferredNotice(key)

	unconfirmed := ApplyOutcome{
		DaemonApply:        DaemonApplyUnconfirmed,
		FailedListenerKeys: []string{key},
	}
	got := EffectNotice(key, unconfirmed)
	if got == rebindPromise {
		t.Errorf("EffectNotice(%q) promised a next-start effect for an unconfirmed apply: %q", key, got)
	}
	if status := unconfirmed.StatusForKey(key); status != ApplyStatusUnconfirmed {
		t.Errorf("StatusForKey(%q) = %q, want %q — the notice and the status must agree", key, status, ApplyStatusUnconfirmed)
	}
}

func TestEffectNoticeDaemonApplyUnconfirmed(t *testing.T) {
	const want = "Saved — the daemon’s live config apply could not be confirmed (see the warnings for the reason)."
	outcome := ApplyOutcome{DaemonApply: DaemonApplyUnconfirmed}
	if got := EffectNotice("network.require_token", outcome); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestApplyOutcomeStatusForKey(t *testing.T) {
	tests := []struct {
		name    string
		outcome ApplyOutcome
		key     string
		want    ApplyStatus
	}{
		{name: "no daemon", outcome: ApplyOutcome{DaemonApply: DaemonApplyNotReached}, key: "default_program", want: ApplyStatusNoDaemon},
		{name: "applied", outcome: ApplyOutcome{DaemonApply: DaemonApplyApplied}, key: "default_program", want: ApplyStatusApplied},
		{name: "failed", outcome: ApplyOutcome{DaemonApply: DaemonApplyFailed}, key: "default_program", want: ApplyStatusFailed},
		{name: "unconfirmed", outcome: ApplyOutcome{DaemonApply: DaemonApplyUnconfirmed}, key: "default_program", want: ApplyStatusUnconfirmed},
		{
			name: "failed listener key is deferred",
			outcome: ApplyOutcome{
				DaemonApply:        DaemonApplyApplied,
				FailedListenerKeys: []string{"network.listen_addr"},
			},
			key:  "network.listen_addr",
			want: ApplyStatusDeferred,
		},
		{
			name: "unrelated key still applied",
			outcome: ApplyOutcome{
				DaemonApply:        DaemonApplyApplied,
				FailedListenerKeys: []string{"network.listen_addr"},
			},
			key:  "network.require_token",
			want: ApplyStatusApplied,
		},
		{
			name:    "next daemon start",
			outcome: ApplyOutcome{DaemonApply: DaemonApplyApplied},
			key:     "branch_prefix",
			want:    ApplyStatusDeferred,
		},
		{
			name:    "next client start",
			outcome: ApplyOutcome{DaemonApply: DaemonApplyApplied},
			key:     "update_channel",
			want:    ApplyStatusDeferred,
		},
		{
			name:    "startup-only key remains deferred without daemon",
			outcome: ApplyOutcome{DaemonApply: DaemonApplyNotReached},
			key:     "debug_pprof",
			want:    ApplyStatusDeferred,
		},
		{
			// This case used to expect "deferred", on the premise that an apply
			// failure was UNRELATED to a key the apply cannot make live. There is
			// no such failure: Manager.ApplyConfig has exactly one error return,
			// wrapping config.LoadConfig, so a failure means the whole file did not
			// load — and the next daemon start reads that same file. "Deferred"
			// promised an effect the invalid file cannot deliver (#4247).
			name:    "startup-only key reports the failed reload that will also break its next start",
			outcome: ApplyOutcome{DaemonApply: DaemonApplyFailed},
			key:     "root_agents",
			want:    ApplyStatusFailed,
		},
		{
			// The complement, and the reason failure and uncertainty rank
			// differently against the class: an unconfirmed apply still WROTE the
			// file, so the next start reads this save's value.
			name:    "startup-only key stays deferred when the apply is merely unconfirmed",
			outcome: ApplyOutcome{DaemonApply: DaemonApplyUnconfirmed},
			key:     "root_agents",
			want:    ApplyStatusDeferred,
		},
		{
			name:    "unclassified key is unknown",
			outcome: ApplyOutcome{DaemonApply: DaemonApplyApplied},
			key:     "future_unclassified_key",
			want:    ApplyStatusUnknown,
		},
		{
			// A successful apply whose digest did not match: the daemon loaded
			// some other writer's file, so its live value is unproven and the
			// applied claim is withheld rather than replaced by a stronger one
			// (#4247).
			name: "applied but not confirmed by the digest withholds the live claim",
			outcome: ApplyOutcome{
				DaemonApply: DaemonApplyUnconfirmed,
			},
			key:  "default_program",
			want: ApplyStatusUnconfirmed,
		},
		{
			// The complement: an apply result that says nothing about WHICH value
			// is stored still loses to the class, because no apply can make a
			// startup-only key live.
			name: "an unconfirmed apply still defers a startup-only key",
			outcome: ApplyOutcome{
				DaemonApply: DaemonApplyUnconfirmed,
			},
			key:  "branch_prefix",
			want: ApplyStatusDeferred,
		},
		{
			// The zero ApplyOutcome is not an outcome at all: no producer
			// recorded an apply result. It reports unknown rather than the
			// no-daemon status a forgotten assignment would otherwise
			// masquerade as (#4482).
			name:    "an unset apply result is unknown, not no-daemon",
			outcome: ApplyOutcome{},
			key:     "default_program",
			want:    ApplyStatusUnknown,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.outcome.StatusForKey(tc.key); got != tc.want {
				t.Errorf("StatusForKey(%q) = %q, want %q", tc.key, got, tc.want)
			}
		})
	}
}
