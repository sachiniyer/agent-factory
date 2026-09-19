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
	// The last row must match anything, or some save would get no answer —
	// including an outcome carrying facts but no recorded apply result.
	last := saveRules[len(saveRules)-1]
	for _, key := range saveRuleKeys {
		if !last.applies(key, ApplyOutcome{DaemonApply: DaemonApplyUnset, FailedListenerKeys: []string{"network.listen_addr"}}) {
			t.Errorf("the final row does not apply to %q, so the table is not total", key)
		}
	}
}

// A row whose status says the save's value is not reliably in effect must not
// render a sentence promising that it takes effect. Checked per row and per
// key, because two of those sentences vary with the key's class.
func TestNoWithholdingRowPromisesAnEffect(t *testing.T) {
	withholding := map[ApplyStatus]bool{
		ApplyStatusUnconfirmed: true, ApplyStatusFailed: true,
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
		failed := ApplyOutcome{DaemonApply: DaemonApplyFailed}
		if got := failed.StatusForKey(key); got != ApplyStatusFailed {
			t.Errorf("StatusForKey(%q) with a failed reload = %q, want %q", key, got, ApplyStatusFailed)
		}
		if notice := EffectNotice(key, failed); strings.Contains(notice, "takes effect") {
			t.Errorf("EffectNotice(%q) promised an effect from a file that did not load: %q", key, notice)
		}

		unconfirmed := ApplyOutcome{DaemonApply: DaemonApplyUnconfirmed}
		if got := unconfirmed.StatusForKey(key); got != ApplyStatusDeferred {
			t.Errorf("StatusForKey(%q) with an unconfirmed apply = %q, want %q", key, got, ApplyStatusDeferred)
		}
		if notice := EffectNotice(key, unconfirmed); !strings.Contains(notice, "takes effect") {
			t.Errorf("EffectNotice(%q) withheld the deferred sentence for a save whose write succeeded: %q", key, notice)
		}
	}
}

// A digest mismatch is another cause of DaemonApplyUnconfirmed, so it ranks
// exactly where the other causes do — and that ranking is the decision, not an
// accident. A LIVE key withholds its claim, because the daemon may be serving
// the other writer's value. A DEFERRED key keeps its class sentence, because the
// file was written either way and a race at save time is indistinguishable from
// a hand-edit a minute later, which no save could have reported either (#4247).
func TestAnUnconfirmedApplyWithholdsOnlyTheLiveClaim(t *testing.T) {
	unconfirmed := ApplyOutcome{DaemonApply: DaemonApplyUnconfirmed}
	for _, key := range []string{"default_program", "network.listen_addr", "network.require_token"} {
		if got := unconfirmed.StatusForKey(key); got != ApplyStatusUnconfirmed {
			t.Errorf("live key %q with an unconfirmed apply = %q, want %q", key, got, ApplyStatusUnconfirmed)
		}
		if notice := EffectNotice(key, unconfirmed); strings.Contains(notice, "using the new value now") {
			t.Errorf("EffectNotice(%q) claimed the daemon is serving an unconfirmed save: %q", key, notice)
		}
	}
	for _, key := range []string{"branch_prefix", "appearance"} {
		if got := unconfirmed.StatusForKey(key); got != ApplyStatusDeferred {
			t.Errorf("deferred key %q with an unconfirmed apply = %q, want %q", key, got, ApplyStatusDeferred)
		}
	}
}
