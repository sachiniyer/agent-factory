package tmux

import (
	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// SeedPaneRootEvidence primes the recorded pane-root identity a restore
// already trusts — the persisted (RuntimePID, RuntimeStartID) pair the launch
// recorded for this exact session name — so a session whose pane vanishes
// before any live resolution still carries the membership evidence the
// vanished-session sweep needs (#7435 R6). It is a SEED, not a proof: the
// sweep's per-candidate SameIdentity and generation checks keep a stale seed
// from admitting a process that is not this generation's, the way
// PaneRootProcessMatches re-proves the same pair against live panes.
// A zero pair is no anchor — a 0 StartID would pass every later-bound check —
// and a live resolution that already stamped this session's root stays: the
// persisted pair is older than anything tmux just answered for.
func (t *TmuxSession) SeedPaneRootEvidence(pid int, startID uint64) {
	if pid <= 0 || startID == 0 {
		return
	}
	t.reconcilePaneRootEvidence(paneRootAnswer{
		seed: &proctree.Process{PID: pid, StartID: startID},
	})
}

// RetirePaneRootEvidence drops the recorded pane-root anchor when the claim
// that produced it is retired — the instance's persisted (runtimePID,
// runtimeStartID) cleared on an answered reattach mismatch (#7435 R11). A
// seed outlives nothing it was seeded from: keeping the disproven pair would
// let tier 2 anchor on a root tmux just said is not this session's.
func (t *TmuxSession) RetirePaneRootEvidence(pid int, startID uint64) {
	t.reconcilePaneRootEvidence(paneRootAnswer{
		disproven: proctree.Process{PID: pid, StartID: startID},
	})
}

// paneRootAnswer is what ONE pane-root resolution learned about the recorded
// anchor — a discriminated input, one field per rule (#7484 item 3). Every
// site that resolves a pane root — spawn stamp, close-path capture, reattach
// settle, restore seed — funnels its outcome through
// reconcilePaneRootEvidence, so the anchor rules live in exactly one table
// instead of a separate edit per site (#7435 R11/R17, Codex 4596311717,
// 4597265491). The two KEEP verdicts are deliberately distinct inputs:
// unanswered is a tmux query that could not answer, exitedBeforeCapture is a
// query that answered with no live root — the same outcome through different
// doors, and the table test drives them as different answers.
type paneRootAnswer struct {
	// resolved is a LIVE root the query captured — a fresh spawn's new pane
	// or a confirmed recorded pair. Zero when no live root answered.
	resolved proctree.Process
	// disproven is the recorded pair an ANSWERED mismatch disproved — tmux
	// placed some other root in this session's panes.
	disproven proctree.Process
	// seed is a persisted pair a restore trusts: installed only while
	// nothing stands, so a live resolution — or even a cleared but
	// newer claim — always outranks it.
	seed *proctree.Process
	// supersede marks a fresh spawn boundary: a surviving start() always
	// created a new pane, so previous evidence names a DIFFERENT
	// incarnation and is obsolete whether or not the new root resolves.
	supersede bool
	// unanswered marks a pane-root query that could not answer — a tmux
	// timeout, an uninspectable pid, a held dead pane. It disproves
	// nothing, so whatever evidence stands keeps standing (R17).
	unanswered bool
	// exitedBeforeCapture marks a capture that answered with no live
	// root — the pane root exited between panePID and the snapshot. The
	// answer carries no new identity, so the PREVIOUS anchor survives
	// (Codex 4596311717): storing the zero identity would erase the
	// recorded SID and tier-2 root, and the surviving cohort would refuse
	// forever on every retry.
	exitedBeforeCapture bool
}

// paneRootEvidenceAfter is the rule table itself, a pure function over
// (current anchor, resolution answer) → next anchor, in strength order:
//
//   - supersede clears first — a fresh spawn's evidence names a different
//     incarnation whether or not the new root resolves (4597265491);
//   - a resolved live root REPLACES whatever remains — the strongest claim
//     there is;
//   - an answered mismatch retires ONLY the pair the disproven claim seeded
//     — a newer live resolution is a different claim and survives (R11);
//   - a seed fills only an EMPTY cell — a standing claim, even one a
//     supersede just cleared conceptually, never loses to a persisted pair;
//   - anything else KEEPS whatever stands: an unanswered query disproves
//     nothing (R17), and a root that exited before capture carries no new
//     evidence (4596311717).
func paneRootEvidenceAfter(cur *proctree.Process, a paneRootAnswer) *proctree.Process {
	if a.supersede {
		cur = nil
	}
	if a.resolved.PID > 0 && a.resolved.StartID != 0 {
		resolved := a.resolved
		return &resolved
	}
	if a.disproven.PID > 0 && a.disproven.StartID != 0 &&
		cur != nil && cur.PID == a.disproven.PID && cur.StartID == a.disproven.StartID {
		return nil
	}
	if cur == nil && a.seed != nil {
		return a.seed
	}
	return cur
}

// reconcilePaneRootEvidence applies the rule table to the stored anchor,
// atomically: the table is the SOLE writer of the cell's next value (#7484
// item 3), and the CAS loop is what makes "read the table, write the cell"
// atomic — a racing answer that lands between the load and the swap restarts
// the computation against the value that actually won, so no verdict is ever
// built on a cell the caller already lost. A keep verdict — or any table
// output pointer-identical to what already stands — needs no CAS at all.
func (t *TmuxSession) reconcilePaneRootEvidence(a paneRootAnswer) {
	for {
		cur := t.paneRootEvidence.Load()
		next := paneRootEvidenceAfter(cur, a)
		if next == cur {
			return
		}
		if t.paneRootEvidence.CompareAndSwap(cur, next) {
			return
		}
	}
}

// PaneRootEvidence reports the recorded pane-root pair this session would
// anchor a vanished-session sweep on, when it carries one. The caller-side
// counterpart of SeedPaneRootEvidence: a restore that retires the claim the
// seed came from must see the anchor retire too (#7435 R11).
func (t *TmuxSession) PaneRootEvidence() (pid int, startID uint64, ok bool) {
	if cur := t.paneRootEvidence.Load(); cur != nil {
		return cur.PID, cur.StartID, true
	}
	return 0, 0, false
}
