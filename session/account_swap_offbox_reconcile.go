package session

import (
	"errors"
	"fmt"
)

// reconcileSwap learns how a swap whose response was lost actually ended, from
// the agent-server's own terminal record — but only a record that names THIS
// swap proves anything about it (#7773). swapID is the identity the swap
// request carried; the status answer echoes the id of the swap it describes.
//
// A status read is ordered: a running swap holds the recorder until it is
// terminal, so a record that names an EARLIER swap means this one had not
// started when the read was taken — but the request can still be in transit or
// queued on the server's operation lock. The stale-read protocol therefore
// asks the SERVER to discard the staged scope, serialized with the swap under
// the same lock the swap holds: a request already running reaches its terminal
// outcome first and the second read answers under its own id; one still queued
// finds its id tombstoned and its staged scope gone — it can never commit,
// and because a stage carries its own swap's marker it could not have rotated
// a later stage's scope in either. A server too old to offer the discard
// offers no ordering at all, so the reconcile stays indeterminate — never an
// unordered remote rm racing a rotation (#7773 R2). Rollback state
// (account.prev) is never touched on anything less than a commit this swap
// itself produced, and a failed status read or marker probe stays
// indeterminate — never a guess at either answer.
//
// The anonymous rows deserve spelling out (#7788): a FIRST read that returns
// an anonymous committed or rolled_back is treated exactly like a stale named
// record — it proves nothing about this swap — so the discard runs and the
// second read re-decides. On that second read an anonymous rolled_back is NOT
// committed (the discard already tombstoned this swap's id; the record can
// only describe an earlier, unattributed terminal rollback), while an
// anonymous committed stays indeterminate — it might be this swap's own
// commit on a server that cannot echo the id.
//
// The last result carries WHY the answer stayed unknown when there is
// something actionable to say — today only the missing discard route on a
// pre-identity agent-server, which the caller folds into the fence's error.
func (w *sandboxWorkspace) reconcileSwap(sw AccountSwapper, swapID string, scopedStage bool) (committed bool, program string, known bool, cause error) {
	discarded := false
	for attempt := 0; attempt < 2; attempt++ {
		outcome, prog, recID, err := sw.AccountSwapStatus()
		if err != nil {
			return false, "", false, nil
		}
		if recID != "" && recID == swapID {
			// This swap's own terminal record — the only admissible proof of
			// commit. It is read after the serialized discard below, so a
			// commit it reports is ordered after every intervention here.
			switch outcome {
			case swapOutcomeCommitted:
				return true, prog, true, nil
			case swapOutcomeRolledBack:
				return false, "", true, nil
			case swapOutcomePending:
				// The pre-rotation checkpoint: this swap's id was persisted
				// BEFORE swap() ran, so a restart rehydrated a swap that had
				// not yet reached the rotation. The record alone cannot settle
				// it — the host's layout is the other witness. A still-staged,
				// never-rotated scope (scopeOutgoing — account.next.<id>
				// present, no account.prev) PROVES the swap never reached the
				// rotation, so it can only roll back. Settle as rolled_back
				// only AFTER the serialized discard tombstones this swap's id
				// and removes its stage — never on this first read — so a
				// late run(swapID) that names the still-staged scope is
				// refused instead of committing against a daemon that already
				// recorded a rollback (#7862 review). The discard claims this
				// swap's own rolled_back for the slot its stage was just
				// removed from (refuseLocked), so the second read settles. Any
				// other layout stays fenced: a scopeIncoming/unknown over this
				// swap's own pending means the rotation may have reached the
				// host, and a commit must never be guessed, so the row fences
				// exactly where master would.
				if w.probeScopeState(swapID, scopedStage, sshShortStepTimeout) != scopeOutgoing {
					return false, "", false, nil
				}
				// scopeOutgoing: fall through to the shared discard below,
				// then re-read.
			case swapOutcomeUnknown:
				// A terminal unknown: swap() ran and its scope could not be
				// put back, so the rotation MAY have reached the host. The
				// record alone cannot settle it, and unlike the pre-rotation
				// checkpoint this state is NOT safe for the discard to upgrade
				// to rolled_back — a straggler run(swapID) that takes the
				// recorder's lock between this read and the discard can leave
				// the host with neither account in place, and upgrading its
				// terminal unknown to rolled_back would settle a rollback
				// against a host that has no live scope (#7862 review). Fence,
				// as on master.
				return false, "", false, nil
			default:
				return false, "", false, nil
			}
		} else {
			switch outcome {
			case swapOutcomeNone:
				// "Never ran here" is only as good as the host's agreement: a server
				// restart forgets every swap, so the markers must show the staged
				// scope still staged and nothing rotated away.
				if discarded {
					return false, "", true, nil // still none after the staged scope is gone
				}
				if w.probeScopeState(swapID, scopedStage, sshShortStepTimeout) != scopeOutgoing {
					return false, "", false, nil
				}
			case swapOutcomeUnknown, swapOutcomePending:
				// The host's scope is in doubt: fence, and leave every directory —
				// the staged scope included — for whatever recovery inspects them.
				return false, "", false, nil
			case swapOutcomeCommitted, swapOutcomeRolledBack:
				if discarded {
					// The discard serialized with the swap tombstoned this swap's
					// id and removed its staged scope, so it can never commit —
					// the host holds whatever the last attributed swap left. A
					// record that names another swap, or an anonymous rolled_back,
					// is therefore not committed: it describes an earlier swap's
					// terminal state while this one is bounded by the tombstone.
					// An anonymous committed is the one exception: on a server
					// that cannot echo the id it might BE this swap's, which no
					// read here can tell apart.
					return false, "", recID != "" || outcome == swapOutcomeRolledBack, nil
				}
				// A record that is not this swap's — an earlier swap's stale
				// outcome, or an anonymous one — proves nothing about it, and the
				// request may still be in flight or queued on the server: discard
				// the staged scope, then re-read.
			default:
				return false, "", false, nil
			}
		}
		// Discard the staged scope ON THE SERVER, serialized with the swap:
		// either the request ran first and its own record answers the next
		// read, or the discard tombstones the id and a late request can only
		// roll back. A pre-identity agent-server has no such operation — the
		// reconcile fences rather than running an unordered remote rm that can
		// empty a scope under a rotation in flight (#7773 R2) — and the fence
		// names it so the operator knows to upgrade (#7788).
		if err := sw.DiscardAccountNext(swapID); err != nil {
			if errors.Is(err, errAgentRouteMissing) {
				return false, "", false, fmt.Errorf("the agent-server predates the %s route; the row stays fenced until a daemon restart reconciles the swap or `af sessions kill` removes it, and no other verb lifts this fence (#7797) — an agent-server upgrade prevents a recurrence but cannot prove this lost swap's outcome", accountSwapDiscardFeature)
			}
			return false, "", false, nil
		}
		discarded = true
	}
	return false, "", false, nil
}
