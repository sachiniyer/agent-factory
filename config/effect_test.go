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
	applied := EffectNotice("default_program", ApplyOutcome{DaemonApplied: true})
	if !strings.Contains(applied, "using the new value now") {
		t.Errorf("applied-live notice should say it is live now, got %q", applied)
	}

	pending := EffectNotice("branch_prefix", ApplyOutcome{DaemonApplied: true})
	if !strings.Contains(pending, "next daemon start") {
		t.Errorf("next-daemon-start notice should defer to the next daemon start, got %q", pending)
	}
	if strings.Contains(pending, "using the new value now") {
		t.Errorf("next-daemon-start notice must not claim it is live now, got %q", pending)
	}

	client := EffectNotice("update_channel", ApplyOutcome{DaemonApplied: true})
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
	n := EffectNotice("default_program", ApplyOutcome{})
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

func TestThemeIsAppliedLiveToTheDaemonPaletteProjection(t *testing.T) {
	if KeyEffectClass("theme") != EffectAppliedLive {
		t.Fatalf("theme effect class = %v, want EffectAppliedLive", KeyEffectClass("theme"))
	}
}

// TestEffectNoticeReportsAFailedListenerRebindAsDeferred is the #3397 contract: a
// key whose live rebind FAILED is not live, whatever its effect class says, because
// bind-new-before-close left the old listener serving. The notice must not
// contradict the warning the same save surface prints beside it.
func TestEffectNoticeReportsAFailedListenerRebindAsDeferred(t *testing.T) {
	for _, key := range []string{"network.listen_addr", "network.preview_listen_addr"} {
		n := EffectNotice(key, ApplyOutcome{DaemonApplied: true, FailedListenerKeys: []string{key}})
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
		DaemonApplied: true, FailedListenerKeys: []string{"network.listen_addr"},
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
			n := EffectNotice(tc.key, ApplyOutcome{DaemonApplied: true, FailedListenerKeys: []string{tc.failed}})
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
	outcome := ApplyOutcome{DaemonApplied: true, FailedListenerKeys: []string{"network.listen_addr"}}
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
	n := EffectNotice("network.listen_addr", ApplyOutcome{DaemonApplied: true})
	if !strings.Contains(n, "using the new value now") {
		t.Errorf("a successful rebind must still report the change as live, got %q", n)
	}
}

// TestEffectNoticeZeroOutcomeIsTheDaemonlessSentence: a caller with no apply result
// at all — no daemon ran, or its apply errored — stays expressible, and gets the
// pre-#3397 sentence verbatim.
func TestEffectNoticeZeroOutcomeIsTheDaemonlessSentence(t *testing.T) {
	const want = "Saved — no daemon is running to apply it, so it takes effect on the next daemon start."
	if got := EffectNotice("network.listen_addr", ApplyOutcome{}); got != want {
		t.Errorf("daemonless notice changed\n got: %q\nwant: %q", got, want)
	}
}
