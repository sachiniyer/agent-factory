package config

import (
	"strings"
	"testing"
)

// StatusForKey and EffectNotice answer the same question about one save, as a
// wire status and as prose. Point cases kept missing the disagreements between
// them — superseded masked by the effect class, then the rebind notice
// outranking superseded, then the class outranking unconfirmed in one function
// and not the other — so this asserts the whole cross-product instead: every
// outcome-bit combination against one key per effect class (#4247).
//
// It checks CONSISTENCY, not specific wording: whatever status a save gets, its
// sentence must be one the reader could derive that status from.
func TestEffectNoticeAgreesWithStatusForKeyAcrossOutcomes(t *testing.T) {
	keys := []string{
		"default_program",       // EffectAppliedLive
		"network.listen_addr",   // EffectAppliedLive, and a socket key
		"branch_prefix",         // EffectNextDaemonStart
		"appearance",            // EffectNextAfLaunch
		"not_a_real_config_key", // EffectUnknown
	}
	// noticeAllowed reports whether notice is a sentence consistent with status.
	noticeAllowed := func(key, notice string, status ApplyStatus) bool {
		switch status {
		case ApplyStatusSuperseded:
			return notice == supersededNotice(key)
		case ApplyStatusDeferred:
			// Either the class sentence or the failed-rebind sentence.
			return strings.Contains(notice, "takes effect on the next daemon start") ||
				strings.Contains(notice, "takes effect the next time you launch af") ||
				notice == listenerRebindDeferredNotice(key)
		case ApplyStatusUnconfirmed:
			return strings.Contains(notice, "could not be confirmed")
		case ApplyStatusFailed:
			return strings.Contains(notice, "could not apply the new configuration")
		case ApplyStatusApplied:
			return strings.HasPrefix(notice, "Applied —")
		case ApplyStatusNoDaemon:
			return strings.Contains(notice, "no daemon is running")
		case ApplyStatusUnknown:
			return notice == "Saved."
		}
		return false
	}

	checked := 0
	for _, key := range keys {
		for bits := 0; bits < 32; bits++ {
			outcome := ApplyOutcome{
				DaemonApplied:          bits&1 != 0,
				DaemonApplyFailed:      bits&2 != 0,
				DaemonApplyUnconfirmed: bits&4 != 0,
				SavedValueSuperseded:   bits&8 != 0,
			}
			if bits&16 != 0 {
				outcome.FailedListenerKeys = []string{"network.listen_addr"}
			}
			status := outcome.StatusForKey(key)
			notice := EffectNotice(key, outcome)
			checked++
			if !noticeAllowed(key, notice, status) {
				t.Errorf("key=%q bits=%05b: status=%q disagrees with notice %q",
					key, bits, status, notice)
			}
			// Whatever the status, a save that LOST its race must never be told
			// its value takes effect — that promise would be about the winner's.
			if outcome.SavedValueSuperseded && status != ApplyStatusDeferred &&
				(strings.Contains(notice, "takes effect on the next daemon start") ||
					strings.Contains(notice, "takes effect the next time you launch af")) {
				t.Errorf("key=%q bits=%05b: superseded save promised an effect: %q", key, bits, notice)
			}
		}
	}
	if checked != len(keys)*32 {
		t.Fatalf("covered %d combinations, want %d", checked, len(keys)*32)
	}
	t.Logf("checked %d (key, outcome) combinations", checked)
}
