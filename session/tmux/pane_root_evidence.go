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
	if pid <= 0 || startID == 0 || t.paneRootEvidence.Load() != nil {
		return
	}
	t.paneRootEvidence.Store(&proctree.Process{PID: pid, StartID: startID})
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
// anchor. Every site that resolves a pane root — spawn stamp, close-path
// capture, reattach settle — funnels its outcome through
// reconcilePaneRootEvidence, so the four anchor rules live in exactly one
// table instead of four separate edits (#7435 R11/R17, Codex 4596311717,
// 4597265491).
type paneRootAnswer struct {
	// resolved is a LIVE root the query captured — a fresh spawn's new pane
	// or a confirmed recorded pair. Zero when no live root answered: an
	// unanswerable query, or a root that exited before the capture.
	resolved proctree.Process
	// disproven is the recorded pair an ANSWERED mismatch disproved — tmux
	// placed some other root in this session's panes.
	disproven proctree.Process
	// supersede marks a fresh spawn boundary: a surviving start() always
	// created a new pane, so previous evidence names a DIFFERENT
	// incarnation and is obsolete whether or not the new root resolves.
	supersede bool
}

// paneRootEvidenceAfter is the rule table itself, a pure function over
// (current anchor, resolution answer) → next anchor:
//
//   - a resolved live root REPLACES whatever stands — a fresh spawn records
//     ITS OWN root, never the previous incarnation's pair (4597265491);
//   - a fresh spawn with no resolution still clears — the previous pair
//     provably names another incarnation (supersede);
//   - an answered mismatch retires ONLY the pair the disproven claim seeded
//     — a newer live resolution is a different claim and survives (R11);
//   - anything else keeps whatever stands: an unanswered query disproves
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
	return cur
}

// reconcilePaneRootEvidence applies the rule table to the stored anchor.
func (t *TmuxSession) reconcilePaneRootEvidence(a paneRootAnswer) {
	t.paneRootEvidence.Store(paneRootEvidenceAfter(t.paneRootEvidence.Load(), a))
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
