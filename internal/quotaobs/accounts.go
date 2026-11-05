package quotaobs

import (
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/handoff"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/rateindex"
	"github.com/sachiniyer/agent-factory/quota"
)

// collectAccountEvidence fills the per-account section of the result: each
// registered codex account's newest rate_limits claim, plus the mover line
// (configured mode and any recorded halt).
//
// It is key-gated on the auto-handoff machinery being engaged — an enabled
// [account_handoff] mode in the global config, OR an auto-handoff state file
// that machinery already wrote RECORDS to. With neither, it does nothing: a
// mode of "off" — absent table, materialized default, or written out by hand
// — stays byte-identical to before the feature existed (#6776, "absent table
// ≡ off ≡ today"). A state file that carries a record still engages it,
// because a halt recorded there must surface even after the mode returns to
// "off" — a halted mover is never silent (R16).
//
// Every read failure degrades honestly rather than hiding: an unreadable
// account list or state file is reported through the Result's warning fields
// instead of being presented as an empty or healthy section.
func collectAccountEvidence(result *Result) {
	mode := config.AccountHandoffModeOff
	enabled := false
	// LoadConfigReadOnly, not LoadConfig: `af quota` and the QuotaReport RPC
	// are read-only, while LoadConfig materializes a default config.toml —
	// and converts a legacy config.json — on a home that has neither. A
	// missing config resolves to in-memory defaults whose [account_handoff]
	// mode is "off": the same answer, without a read-only command writing.
	if load, err := config.LoadConfigReadOnly(); err != nil {
		result.ConfigUnreadable = true
	} else if load.Config != nil {
		mode = load.Config.AccountHandoff.Mode
		enabled = load.Config.AccountHandoffEnabled()
	}

	var state handoff.State
	var stateErr error
	if store, err := handoff.DefaultStore(); err == nil {
		state, stateErr = store.Load()
	}

	// A file engages the section only when it carries evidence — a halt,
	// held reports, move/backoff records, carry watermarks — or when it
	// exists but cannot be read (an unreadable file IS the evidence). A
	// missing file decodes to an empty state without error, and an empty
	// file records nothing either: both answer "the machinery never ran
	// here", so neither opts the section in. That matters because a no-op
	// durable write (e.g. an older daemon's startup reconcile) must not
	// enrol a home in a feature it never enabled.
	recorded := stateErr != nil || state.Halted != nil ||
		state.ReportsHeld != nil || len(state.RecentMoves) > 0 ||
		len(state.RefusedUntil) > 0 || len(state.CarriedPrefixes) > 0
	if !enabled && !recorded {
		// ConfigUnreadable (if set) still warns: an unreadable config does
		// not establish that handoff is disabled, and hiding that failure
		// would silently omit a section the operator may have enabled.
		return
	}

	mover := &quota.MoverStatus{Mode: mode}
	if result.ConfigUnreadable {
		// The configured mode is unknowable — "mover: off" would be a guess.
		mover.Mode = ""
		mover.ModeUnknown = true
	}
	if stateErr != nil {
		mover.StateUnreadable = true
		result.HandoffUnreadable = true
	} else if state.Halted != nil {
		mover.Halted = true
		mover.HaltReason = state.Halted.Reason
		mover.HaltAt = state.Halted.At
	}
	if state.ReportsHeld != nil {
		mover.ReportsHeldSince = state.ReportsHeld.Since
	}
	result.Mover = mover

	home, err := config.GetConfigDir()
	if err != nil {
		result.AccountsUnreadable = true
		return
	}
	names, err := agentaccount.List(home, "codex")
	if err != nil {
		result.AccountsUnreadable = true
		return
	}

	indexer := rateindex.New()
	for _, name := range names {
		dir, err := agentaccount.Dir(home, "codex", name)
		if err != nil {
			// Unreachable for a name List just produced (it validates each
			// one); kept so a future caller's contract change cannot turn a
			// bad name into a panic or a silently wrong path.
			result.AccountsUnreadable = true
			continue
		}
		result.Accounts = append(result.Accounts,
			accountFromReading(name, indexer.Refresh(dir, rateindex.Options{
				CarriedPrefixes: state.CarriedPrefixes,
			}), stateErr == nil))
	}
}

// accountFromReading projects an indexer verdict onto the report's account
// row. The indexer's own vocabulary (observed/unknown/none) maps one-for-one;
// the render-side wording lives in quota.AccountObservation.String.
//
// watermarksKnown is false when the state file would not decode, leaving the
// carried-prefix map unknown. An "observed" reading may then be a claim inside
// a carried prefix — the source account's numbers wearing the target's name —
// so it degrades to unknown rather than presenting a poisoned percentage.
// StatusNone is still honest: a watermark can only SHRINK the eligible region,
// so "no claim anywhere" cannot become "claim" by knowing the watermark.
// StatusUnknown keeps its own reason.
func accountFromReading(name string, reading rateindex.AccountReading, watermarksKnown bool) quota.ObservedAccount {
	out := quota.ObservedAccount{Agent: "codex", Name: name}
	switch reading.Status {
	case rateindex.StatusObserved:
		out.Observation = quota.AccountObservationObserved
		out.Reading = reading.Reading
		if !watermarksKnown {
			out.Observation = quota.AccountObservationUnknown
			out.Reading = quota.UsageReading{}
			out.Reason = "watermarks unreadable"
		}
	case rateindex.StatusUnknown:
		out.Observation = quota.AccountObservationUnknown
		out.Reason = reading.Reason
	default:
		out.Observation = quota.AccountObservationNone
	}
	return out
}
