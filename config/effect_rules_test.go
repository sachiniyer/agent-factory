package config

import (
	"strings"
	"testing"
)

// StatusForKey and EffectNotice are two columns of saveRules, so they cannot
// disagree about a save — the test that used to assert their agreement across
// 160 combinations could no longer fail, and was removed rather than kept as a
// test of nothing. What CAN still be wrong is the table itself, so that is what
// these check: that it is total, that no row is half-written, that every wire
// status is reachable, and that no row withholding an effect renders a sentence
// promising one (#4247).

var saveRuleKeys = []string{
	"default_program",     // EffectAppliedLive
	"network.listen_addr", // EffectAppliedLive, and a socket key
	"branch_prefix",       // EffectNextDaemonStart
	"root_agents",         // EffectNextDaemonStart, with the adoption suffix
	"appearance",          // EffectNextAfLaunch
	"not_a_real_config_key",
}

func TestSaveRulesAreCompleteAndTotal(t *testing.T) {
	declared := map[ApplyStatus]bool{
		ApplyStatusUnknown: true, ApplyStatusApplied: true, ApplyStatusNoDaemon: true,
		ApplyStatusFailed: true, ApplyStatusUnconfirmed: true, ApplyStatusDeferred: true,
		ApplyStatusSuperseded: true,
	}
	produced := map[ApplyStatus]bool{}
	for i, rule := range saveRules {
		if rule.applies == nil || rule.notice == nil {
			t.Fatalf("saveRules[%d] is half-written: every row must carry both projections", i)
		}
		if !declared[rule.status] {
			t.Errorf("saveRules[%d] produces undeclared status %q", i, rule.status)
		}
		produced[rule.status] = true
	}
	for status := range declared {
		if !produced[status] {
			t.Errorf("no row produces %q: a wire value nothing can emit", status)
		}
	}
	// The last row must match anything, or some save would get no answer.
	last := saveRules[len(saveRules)-1]
	for _, key := range saveRuleKeys {
		if !last.applies(key, ApplyOutcome{DaemonApplied: true, DaemonApplyFailed: true, SavedValueSuperseded: true}) {
			t.Errorf("the final row does not apply to %q, so the table is not total", key)
		}
	}
}

// A row whose status says the save's value is not reliably in effect must not
// render a sentence promising that it takes effect. Checked per row and per
// key, because two of those sentences vary with the key's class.
func TestNoWithholdingRowPromisesAnEffect(t *testing.T) {
	withholding := map[ApplyStatus]bool{
		ApplyStatusSuperseded: true, ApplyStatusUnconfirmed: true, ApplyStatusFailed: true,
	}
	for i, rule := range saveRules {
		if !withholding[rule.status] {
			continue
		}
		for _, key := range saveRuleKeys {
			if notice := rule.notice(canonicalConfigKey(key)); strings.Contains(notice, "takes effect") {
				t.Errorf("saveRules[%d] (%q) promises an effect for %q: %q", i, rule.status, key, notice)
			}
		}
	}
}

// A failed apply and an unconfirmed one rank DIFFERENTLY against the effect
// class, which is easy to read as an inconsistency, so it is pinned directly.
// DaemonApplyFailed is set only for a "reload config" failure, so the file did
// not load — and a deferred key's next start reads that same file. Every cause
// of DaemonApplyUnconfirmed leaves the file written and loadable (#4247).
func TestDeferredKeyDistinguishesAFailedReloadFromAnUnconfirmedOne(t *testing.T) {
	for _, key := range []string{"branch_prefix", "root_agents", "appearance", "debug_pprof"} {
		failed := ApplyOutcome{DaemonApplyFailed: true}
		if got := failed.StatusForKey(key); got != ApplyStatusFailed {
			t.Errorf("StatusForKey(%q) with a failed reload = %q, want %q", key, got, ApplyStatusFailed)
		}
		if notice := EffectNotice(key, failed); strings.Contains(notice, "takes effect") {
			t.Errorf("EffectNotice(%q) promised an effect from a file that did not load: %q", key, notice)
		}

		unconfirmed := ApplyOutcome{DaemonApplyUnconfirmed: true}
		if got := unconfirmed.StatusForKey(key); got != ApplyStatusDeferred {
			t.Errorf("StatusForKey(%q) with an unconfirmed apply = %q, want %q", key, got, ApplyStatusDeferred)
		}
		if notice := EffectNotice(key, unconfirmed); !strings.Contains(notice, "takes effect") {
			t.Errorf("EffectNotice(%q) withheld the deferred sentence for a save whose write succeeded: %q", key, notice)
		}
	}
}

// A config file that no longer loads is the opposite case to an unconfirmed
// apply: the next start reads the same unloadable file, so a key served from
// that file must not keep its deferred promise. That is why it is its own fact
// rather than another cause of DaemonApplyUnconfirmed, whose rows let the class
// stand (#4247).
func TestUnreadableFileWithholdsTheDeferredPromise(t *testing.T) {
	rebindFailed := []string{"network.listen_addr"}
	cases := []struct {
		name    string
		key     string
		outcome ApplyOutcome
		when    string
	}{
		{"next daemon start", "branch_prefix", ApplyOutcome{DaemonApplied: true, SavedFileUnreadable: true}, "the next daemon start"},
		{"next af launch", "appearance", ApplyOutcome{DaemonApplied: true, SavedFileUnreadable: true}, "the next af launch"},
		{"failed listener rebind", "network.listen_addr",
			ApplyOutcome{DaemonApplied: true, FailedListenerKeys: rebindFailed, SavedFileUnreadable: true},
			"the next daemon start"},
		{"alongside an unconfirmed apply", "branch_prefix",
			ApplyOutcome{DaemonApplyUnconfirmed: true, SavedFileUnreadable: true}, "the next daemon start"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.outcome.StatusForKey(tc.key); got != ApplyStatusUnconfirmed {
				t.Errorf("StatusForKey(%q) = %q, want %q", tc.key, got, ApplyStatusUnconfirmed)
			}
			notice := EffectNotice(tc.key, tc.outcome)
			if strings.Contains(notice, "takes effect") {
				t.Errorf("EffectNotice(%q) promised an effect from an unloadable file: %q", tc.key, notice)
			}
			if !strings.Contains(notice, tc.when) {
				t.Errorf("EffectNotice(%q) = %q, want it to name %q", tc.key, notice, tc.when)
			}
		})
	}

	// A lost race still outranks it: that is a statement about which value is
	// stored, and the unreadable-file row would describe a save that never won.
	both := ApplyOutcome{DaemonApplied: true, SavedValueSuperseded: true, SavedFileUnreadable: true}
	if got := both.StatusForKey("branch_prefix"); got != ApplyStatusSuperseded {
		t.Errorf("superseded + unreadable file = %q, want %q", got, ApplyStatusSuperseded)
	}

	// The row is gated on FileAuthoritative, so the fact cannot rewrite a LIVE
	// key's answer even if a caller set it there.
	live := ApplyOutcome{DaemonApplied: true, SavedFileUnreadable: true}
	if got := live.StatusForKey("default_program"); got != ApplyStatusApplied {
		t.Errorf("a live key with the unreadable-file fact = %q, want %q", got, ApplyStatusApplied)
	}
}

// FileAuthoritative is the single definition the daemon's readback and fallback
// now share; before it, each spelled the condition out over a second copy of the
// class test living in the daemon package.
func TestFileAuthoritative(t *testing.T) {
	rebind := ApplyOutcome{FailedListenerKeys: []string{"network.listen_addr"}}
	cases := []struct {
		key     string
		outcome ApplyOutcome
		want    bool
	}{
		{"branch_prefix", ApplyOutcome{}, true},
		{"appearance", ApplyOutcome{}, true},
		{"default_program", ApplyOutcome{}, false},
		{"network.listen_addr", ApplyOutcome{}, false},
		{"network.listen_addr", rebind, true},
		{"listen_addr", rebind, true}, // the legacy alias spelling
		{"network.preview_listen_addr", rebind, false},
	}
	for _, tc := range cases {
		if got := tc.outcome.FileAuthoritative(tc.key); got != tc.want {
			t.Errorf("FileAuthoritative(%q, rebind=%v) = %v, want %v",
				tc.key, tc.outcome.FailedListenerKeys, got, tc.want)
		}
	}
}
