package daemon

import (
	"fmt"
	"sort"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/handoff"
)

// HandoffStatus serves `af accounts handoff status` (#6776 slice 5): the
// durable rails record plus the two answers only the daemon knows — the
// resolved global mode and whether the engine is actually observing.
//
// The state file load FAILS CLOSED: a corrupt rails file is itself the
// answer an operator runs status to discover, so an unreadable file errors
// rather than rendering an authoritative-looking empty status. Like
// QuotaReport it is deliberately not gated on requireManagerReady — the file
// answers truthfully while the daemon warms.
func (s *controlServer) HandoffStatus(_ HandoffStatusRequest, resp *HandoffStatusResponse) error {
	store, err := handoff.DefaultStore()
	if err != nil {
		return err
	}
	state, err := store.Load()
	if err != nil {
		return err
	}
	if s.manager != nil {
		resp.Engaged = s.manager.handoffActive.Load()
	}
	if load, err := config.LoadConfigReadOnly(); err != nil {
		// A mode of "off" here would be a guess — and "off" reads as safe.
		resp.ModeUnknown = true
		resp.Warnings = append(resp.Warnings,
			"the global config could not be read, so the resolved account_handoff.mode is unknown")
	} else if load.Config != nil {
		resp.Mode = load.Config.AccountHandoff.Mode
		resp.MaxMoves = load.Config.AccountHandoff.MaxMoves
		resp.MaxMovesWindow = load.Config.AccountHandoff.MaxMovesWindow
		resp.RefusedBackoff = load.Config.AccountHandoff.RefusedBackoff
		resp.OnFailure = load.Config.AccountHandoff.OnFailure
	}
	if state.Halted != nil {
		resp.Halted = true
		resp.HaltReason = state.Halted.Reason
		at := state.Halted.At
		resp.HaltAt = &at
	}
	if state.ReportsHeld != nil {
		since := state.ReportsHeld.Since
		resp.ReportsHeldSince = &since
	}
	// Session keys are stable identities, not titles (R13): the title is
	// attached only as a label for sessions the manager still tracks, and a
	// record whose session is gone still prints under its key.
	titles := s.sessionTitles()
	for key, until := range state.RefusedUntil {
		resp.Backoffs = append(resp.Backoffs, HandoffBackoffEntry{
			SessionKey: key, Title: titles[key], Until: until,
		})
	}
	sort.Slice(resp.Backoffs, func(i, j int) bool {
		if !resp.Backoffs[i].Until.Equal(resp.Backoffs[j].Until) {
			return resp.Backoffs[i].Until.Before(resp.Backoffs[j].Until)
		}
		return resp.Backoffs[i].SessionKey < resp.Backoffs[j].SessionKey
	})
	for _, mv := range state.RecentMoves {
		resp.RecentMoves = append(resp.RecentMoves, HandoffMoveEntry{
			SessionKey: mv.SessionKey, Title: titles[mv.SessionKey],
			Account: mv.Account, At: mv.At,
		})
	}
	sort.Slice(resp.RecentMoves, func(i, j int) bool {
		if !resp.RecentMoves[i].At.Equal(resp.RecentMoves[j].At) {
			return resp.RecentMoves[i].At.Before(resp.RecentMoves[j].At)
		}
		return resp.RecentMoves[i].SessionKey < resp.RecentMoves[j].SessionKey
	})
	return nil
}

// sessionTitles maps every tracked stable session identity to its display
// title, so status output can label records without ever keying on titles.
func (s *controlServer) sessionTitles() map[string]string {
	titles := map[string]string{}
	if s.manager == nil {
		return titles
	}
	s.manager.mu.Lock()
	defer s.manager.mu.Unlock()
	for key, inst := range s.manager.instances {
		repoID, _ := splitDaemonInstanceKey(key)
		if repoID == "" {
			continue
		}
		titles[stableSessionKey(repoID, inst)] = inst.Title
	}
	return titles
}

// HandoffReset serves `af accounts handoff reset` (#6776 slice 5): clear the
// ONE record the operator named — the durable halt, the refusal backoffs, or
// the recent-move ledger — and nothing else. Named-record grammar is the
// safety property: the rails protect each other from an impatient reset, so
// clearing a halt can never silently take the switch-loop history with it.
//
// It mutates durable state, so it passes the upgrade-probation admission
// gate; the file itself still fails closed on corruption — reset never
// rebuilds over a file it cannot read.
func (s *controlServer) HandoffReset(req HandoffResetRequest, resp *HandoffResetResponse) error {
	if err := s.requireMutationAdmission(); err != nil {
		return err
	}
	store, err := handoff.DefaultStore()
	if err != nil {
		return err
	}
	switch req.Record {
	case "halt":
		if req.SessionKey != "" {
			return fmt.Errorf(`record "halt" is mover-level — it takes no session scope`)
		}
		cleared, err := store.ClearHalt()
		if err != nil {
			return err
		}
		if cleared == nil {
			resp.Summary = "no halt was recorded"
			return nil
		}
		resp.Changed = true
		resp.Summary = fmt.Sprintf("cleared halt %q recorded at %s — automatic handoff moves may proceed again",
			cleared.Reason, cleared.At.Format(time.RFC3339))
	case "backoff", "moves":
		var n int
		var err error
		if req.Record == "backoff" {
			n, err = store.ClearRefused(req.SessionKey)
		} else {
			n, err = store.ClearMoves(req.SessionKey)
		}
		if err != nil {
			return err
		}
		resp.Changed = n > 0
		switch {
		case n == 0 && req.SessionKey != "":
			resp.Summary = fmt.Sprintf("no %s entries were recorded for session %q", req.Record, req.SessionKey)
		case n == 0:
			resp.Summary = fmt.Sprintf("no %s entries were recorded", req.Record)
		case req.SessionKey != "":
			resp.Summary = fmt.Sprintf("cleared %d %s %s for session %q",
				n, req.Record, plural(n, "entry", "entries"), req.SessionKey)
		default:
			resp.Summary = fmt.Sprintf("cleared %d %s %s", n, req.Record, plural(n, "entry", "entries"))
		}
	default:
		if req.Record == "" {
			return fmt.Errorf(`reset needs the record to clear — one of: halt, backoff, moves`)
		}
		return fmt.Errorf(`unknown record %q — name one of: halt, backoff, moves`, req.Record)
	}
	return nil
}
