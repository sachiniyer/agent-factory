package tmux

import (
	"errors"
	"os"
	"syscall"

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

// paneRootKeep names WHICH door a pane-root resolution left through when it
// produced no new evidence (#7491 item 4). The two keeps converge on the same
// cell — whatever stands, stands — but they are different answers and must
// stay distinguishable at the table: a site that answered "no live root"
// must never report the same input as a site that could not answer at all.
type paneRootKeep uint8

const (
	// paneRootKeepNone: the answer carried a real verdict — resolved,
	// disproven, seed, or supersede. Not a keep at all.
	paneRootKeepNone paneRootKeep = iota
	// paneRootKeepUnanswered marks a pane-root query that could not
	// answer — a tmux timeout, an unclassified query failure, a root pid
	// the process table could not READ (as opposed to one it reported
	// absent). It disproves nothing, so whatever evidence stands keeps
	// standing (R17).
	paneRootKeepUnanswered
	// paneRootKeepExited marks a query that ANSWERED with no live root —
	// a held dead pane, a session tmux reports absent, a pid the process
	// table proves gone. The answer carries no new identity, so the
	// PREVIOUS anchor survives (Codex 4596311717): storing the zero
	// identity would erase the recorded SID and tier-2 root, and the
	// surviving cohort would refuse forever on every retry.
	paneRootKeepExited
)

// paneRootVerdict names which rule paneRootEvidenceAfter fired — the
// observable the table test asserts so distinct inputs stay distinct rows:
// two inputs that leave the same cell MUST NOT collapse into one undifferent
// keep, or a mutation that drops one of them passes silently (#7491 item 4).
type paneRootVerdict uint8

const (
	paneRootVerdictKept paneRootVerdict = iota
	paneRootVerdictKeptUnanswered
	paneRootVerdictKeptExited
	paneRootVerdictResolved
	paneRootVerdictRetired
	paneRootVerdictSeeded
	paneRootVerdictSuperseded
)

// paneRootAnswer is what ONE pane-root resolution learned about the recorded
// anchor — a discriminated input, one field per rule (#7484 item 3). Every
// site that resolves a pane root — spawn stamp, close-path capture, reattach
// settle, restore seed — funnels its outcome through
// reconcilePaneRootEvidence, so the anchor rules live in exactly one table
// instead of a separate edit per site (#7435 R11/R17, Codex 4596311717,
// 4597265491).
type paneRootAnswer struct {
	// resolved is a LIVE root the query captured — a fresh spawn's new pane
	// or a confirmed recorded pair. Zero when no live root answered.
	resolved proctree.Process
	// disproven is the recorded pair an ANSWERED mismatch disproved — tmux
	// placed some other root in this session's panes.
	disproven proctree.Process
	// seed is a persisted pair a restore trusts. It fills the cell the
	// higher rules leave: a live resolution or a standing claim outranks
	// it, but a cell supersede just emptied is EMPTY — the seed installs
	// there too, because the cleared claim was already discarded as
	// obsolete, not upheld over the seed (#7491 item 4).
	seed *proctree.Process
	// supersede marks a fresh spawn boundary: a surviving start() always
	// created a new pane, so previous evidence names a DIFFERENT
	// incarnation and is obsolete whether or not the new root resolves.
	supersede bool
	// keep is WHICH non-answer this resolution produced — unanswered
	// (could not ask) versus exited (asked, told none). Distinct kinds
	// with distinct verdicts; both keep the standing cell.
	keep paneRootKeep
}

// paneRootEvidenceAfter is the rule table itself, a pure function over
// (current anchor, resolution answer) → (next anchor, fired rule), in
// strength order:
//
//   - supersede clears first — a fresh spawn's evidence names a different
//     incarnation whether or not the new root resolves (4597265491);
//   - a resolved live root REPLACES whatever remains — the strongest claim
//     there is;
//   - an answered mismatch retires ONLY the pair the disproven claim seeded
//     — a newer live resolution is a different claim and survives (R11);
//   - a seed fills only an EMPTY cell — including one supersede just
//     emptied: a standing claim outranks a persisted pair, a discarded one
//     does not;
//   - anything else KEEPS whatever stands: an unanswered query disproves
//     nothing (R17), and a root that exited before capture carries no new
//     evidence (4596311717).
func paneRootEvidenceAfter(cur *proctree.Process, a paneRootAnswer) (*proctree.Process, paneRootVerdict) {
	if a.supersede {
		cur = nil
	}
	if a.resolved.PID > 0 && a.resolved.StartID != 0 {
		resolved := a.resolved
		return &resolved, paneRootVerdictResolved
	}
	if a.disproven.PID > 0 && a.disproven.StartID != 0 &&
		cur != nil && cur.PID == a.disproven.PID && cur.StartID == a.disproven.StartID {
		return nil, paneRootVerdictRetired
	}
	if cur == nil && a.seed != nil {
		return a.seed, paneRootVerdictSeeded
	}
	switch a.keep {
	case paneRootKeepUnanswered:
		return cur, paneRootVerdictKeptUnanswered
	case paneRootKeepExited:
		return cur, paneRootVerdictKeptExited
	}
	if a.supersede {
		return cur, paneRootVerdictSuperseded
	}
	return cur, paneRootVerdictKept
}

// paneRootReconcileGap is a test-only seam between the cell's Load and the
// write the table computed from it: the storm test parks a racer inside the
// gap to stage — deterministically — the lost update a reconcile built as
// Load-then-Store would suffer, so the test goes red the moment the CAS
// loop is replaced (#7494 item 4). nil in production.
var paneRootReconcileGap func()

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
		if paneRootReconcileGap != nil {
			paneRootReconcileGap()
		}
		next, _ := paneRootEvidenceAfter(cur, a)
		if next == cur {
			return
		}
		if t.paneRootEvidence.CompareAndSwap(cur, next) {
			return
		}
	}
}

// proctreeErrIsGone reports whether a Lookup failure is the process table
// ANSWERING "no such process" — the same three shapes proctree's own
// gone-predicate accepts (#7491 item 4) — as opposed to a read that failed.
// It is how a resolution site classifies its error into the exited keep kind.
func proctreeErrIsGone(err error) bool {
	return errors.Is(err, proctree.ErrProcessExited) ||
		errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, syscall.ESRCH)
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

// resolveServerIdentity fills the kernel fields a pane row cannot carry —
// the server process's StartID and the pid namespace this process writes to —
// so the record survives a daemon restart and a recycled server pid (#7469).
// A lookup failure degrades the record (socket and pid still compare), never
// discards it.
func resolveServerIdentity(id ServerIdentity) ServerIdentity {
	if !id.valid() {
		return ServerIdentity{}
	}
	if p, err := proctree.Lookup(id.PID); err == nil {
		id.StartID = p.StartID
	}
	if ns, err := proctree.PIDNamespaceID(); err == nil {
		id.PIDNS = ns
	}
	return id
}

// noteServerIdentity is the stamp every live pane resolution calls: a row
// that answered its server fields replaces the record — the answer is fresh
// kernel evidence — while a row that could not keeps whatever stands (a
// field-less tmux answers "unknown", never "changed").
func (t *TmuxSession) noteServerIdentity(raw ServerIdentity) {
	id := resolveServerIdentity(raw)
	if id.valid() {
		stored := id
		t.serverEvidence.Store(&stored)
	}
}

// SeedServerEvidence primes the recorded server identity from the persisted
// launch fields on restore, the server-side counterpart of
// SeedPaneRootEvidence (#7469). A seed fills only an EMPTY cell — anything a
// live resolution stamped is newer than the persisted record. A zero record
// is no anchor: sessions launched before this existed stay undecidable, which
// is today's verdict.
func (t *TmuxSession) SeedServerEvidence(socket string, pid int, startID uint64, sessionID string) {
	id := ServerIdentity{Socket: socket, PID: pid, StartID: startID, SessionID: sessionID}
	if !id.valid() {
		return
	}
	if ns, err := proctree.PIDNamespaceID(); err == nil {
		id.PIDNS = ns
	}
	if t.serverEvidence.Load() == nil {
		stored := id
		t.serverEvidence.Store(&stored)
	}
}

// ServerEvidence reports the tmux-server identity the vanished-session sweep
// judges detached carriers against (#7469) — a session launched before the
// record existed, or one whose pane never answered its server fields, has
// none and every carrier verdict degrades to undecidable.
func (t *TmuxSession) ServerEvidence() (ServerIdentity, bool) {
	if id := t.serverEvidence.Load(); id != nil {
		return *id, true
	}
	return ServerIdentity{}, false
}
