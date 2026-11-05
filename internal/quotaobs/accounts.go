package quotaobs

import (
	"os"

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
// that machinery already wrote. With neither, it does nothing: a mode of
// "off" — absent table, materialized default, or written out by hand — stays
// byte-identical to before the feature existed (#6776, "absent table ≡ off ≡
// today"). The state file alone still triggers it, because a halt recorded
// there must surface even after the mode returns to "off" — a halted mover is
// never silent (R16).
//
// Every read failure degrades honestly rather than hiding: an unreadable
// account list or state file is reported through the Result's warning fields
// instead of being presented as an empty or healthy section.
func collectAccountEvidence(result *Result) {
	mode := config.AccountHandoffModeOff
	enabled := false
	if cfg, err := config.LoadConfig(); err != nil {
		result.ConfigUnreadable = true
	} else if cfg != nil {
		mode = cfg.AccountHandoff.Mode
		enabled = cfg.AccountHandoffEnabled()
	}

	var state handoff.State
	var stateErr error
	stateExists := false
	if store, err := handoff.DefaultStore(); err == nil {
		if _, err := os.Stat(store.Path()); err == nil {
			stateExists = true
		}
		state, stateErr = store.Load()
	}

	if !enabled && !stateExists {
		// Nothing opted this af home in and the machinery never ran here.
		// ConfigUnreadable (if set) is meaningless in that world: a config af
		// could not read cannot have gated anything off.
		result.ConfigUnreadable = false
		return
	}

	mover := &quota.MoverStatus{Mode: mode}
	if stateErr != nil {
		mover.StateUnreadable = true
		result.HandoffUnreadable = true
	} else if state.Halted != nil {
		mover.Halted = true
		mover.HaltReason = state.Halted.Reason
		mover.HaltAt = state.Halted.At
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
			})))
	}
}

// accountFromReading projects an indexer verdict onto the report's account
// row. The indexer's own vocabulary (observed/unknown/none) maps one-for-one;
// the render-side wording lives in quota.AccountObservation.String.
func accountFromReading(name string, reading rateindex.AccountReading) quota.ObservedAccount {
	out := quota.ObservedAccount{Agent: "codex", Name: name}
	switch reading.Status {
	case rateindex.StatusObserved:
		out.Observation = quota.AccountObservationObserved
		out.Reading = reading.Reading
	case rateindex.StatusUnknown:
		out.Observation = quota.AccountObservationUnknown
		out.Reason = reading.Reason
	default:
		out.Observation = quota.AccountObservationNone
	}
	return out
}
