package tmux

import (
	"errors"
	"os"
	"syscall"
	"time"

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
// the server process's StartID and the pid namespace the pid was PROVEN in —
// so the record survives a daemon restart and a recycled server pid (#7469).
// The proof (#7581 item 1): the per-socket reader must show THIS pid holding
// a listener bound to the recorded socket (#7537/#7543) — read-only, never
// tmuxSocketClaimed or SIGUSR1. #{pid} names the server in ITS pid
// namespace; a server living in another one makes the number fiction here —
// it can name a different process entirely — so a stamp that cannot prove
// the pid leaves StartID and PIDNS empty and every pid-derived compare is
// undecidable. Socket and PID still ride the record: the socket compare no
// namespace can break, and the pid still names the verdict's reason.
func resolveServerIdentity(id ServerIdentity, budget time.Duration) ServerIdentity {
	if !id.valid() {
		return ServerIdentity{}
	}
	id.StartID, id.PIDNS = 0, ""
	// The proof's kernel reads run INSIDE what the caller's deadline has
	// left (Codex on #7581): a proof that does not finish in time is
	// unproven — undecidable — never a stamp that blocked the bounded read
	// it rode in on. The seam is captured before the worker starts, so a
	// worker abandoned past the deadline touches no package state a test
	// may already have restored (the #7550 flight race).
	bind := tmuxServerSocketBinding
	type proof struct {
		startID uint64
		ns      string
		bound   bool
	}
	answered := make(chan proof, 1)
	go func() {
		switch binding, _ := bind(id.PID, id.Socket); binding {
		case socketBindingBound, socketBindingBoundByTable:
			p := proof{bound: true}
			if proc, err := proctree.Lookup(id.PID); err == nil {
				p.startID = proc.StartID
			}
			if ns, err := proctree.PIDNamespaceID(); err == nil {
				p.ns = ns
			}
			answered <- p
		default:
			answered <- proof{}
		}
	}()
	ctx, cancel := tmuxTimeoutContextWithin(budget)
	defer cancel()
	select {
	case p := <-answered:
		if p.bound {
			id.StartID, id.PIDNS = p.startID, p.ns
		}
	case <-ctx.Done():
	}
	return id
}

// noteServerIdentity is the stamp every live pane resolution calls: a row
// that answered its server fields replaces the record — the answer is fresh
// kernel evidence — while a row that could not keeps whatever stands (a
// field-less tmux answers "unknown", never "changed"). A landed stamp also
// clears the spawn-boundary retirement: the record once more names the
// session's current incarnation (#7469 R1a). A stamp naming the SAME
// (socket, pid, session) carries the standing record's proof fields forward
// — the namespace proof is paid once per record, not once per row (#7581).
// budget bounds the namespace proof's kernel reads to what the calling
// read's deadline has left — a proof that outruns it is simply unproven
// (#7581, Codex).
func (t *TmuxSession) noteServerIdentity(raw ServerIdentity, budget time.Duration) {
	id := raw
	if cur := t.serverEvidence.Load(); cur != nil &&
		cur.Socket == raw.Socket && cur.PID == raw.PID && cur.SessionID == raw.SessionID &&
		(cur.StartID != 0 || cur.PIDNS != "") {
		// An unchanged tuple carries the standing record's proof forward —
		// but only a PROOF is paid once. An unproven standing record (the
		// deadline won, or the pid genuinely could not be shown bound)
		// re-proves on this stamp's remaining budget: a transiently slow
		// first proof must not pin undecidable forever, and an honestly
		// unprovable one re-pays a bounded read per stamp — never a signal,
		// never a fresh timeout (Codex on #7581).
		id.StartID, id.PIDNS = cur.StartID, cur.PIDNS
	} else {
		id = resolveServerIdentity(raw, budget)
	}
	if id.valid() {
		stored := id
		t.serverEvidence.Store(&stored)
		t.serverEvidenceRetired.Store(false)
	}
}

// RetireServerIdentity drops the recorded tmux-server identity when the
// incarnation it describes is retired — every spawn boundary (#7469 R1). A
// record must never predate the session's CURRENT incarnation: if the
// post-spawn pane resolution cannot restamp (a timeout, an instantly-dead
// pane, an empty reply), keeping the previous launch's record would let a
// stale (socket, old server pid) judge the replacement server's own
// survivors foreign — named, left alive, while teardown deletes the
// worktree under them. Absent is undecidable, and undecidable keeps today's
// verdict — the safe direction either way.
//
// The retirement is keyed to "a new incarnation can exist" — the
// new-session issue inside start(), not to Start's success (R1a): a spawn
// that then fails still retires it. The retired marker stays raised until
// a stamp or seed lands so the persisted row can write absent too.
func (t *TmuxSession) RetireServerIdentity() {
	t.serverEvidence.Store(nil)
	t.serverEvidenceRetired.Store(true)
}

// ServerEvidenceRetired reports whether the spawn-boundary retirement
// still stands — a new-session was issued and no live stamp or persisted
// seed has landed since (#7469 R1a). The instance row consults it at
// marshal time so the previous incarnation's identity is never persisted
// under the replacement's name.
func (t *TmuxSession) ServerEvidenceRetired() bool {
	return t.serverEvidenceRetired.Load()
}

// SeedServerEvidence primes the recorded server identity from the persisted
// launch fields on restore, the server-side counterpart of
// SeedPaneRootEvidence (#7469). A seed fills only an EMPTY cell — anything a
// live resolution stamped is newer than the persisted record. A zero record
// is no anchor: sessions launched before this existed stay undecidable, which
// is today's verdict.
func (t *TmuxSession) SeedServerEvidence(socket string, pid int, startID uint64, sessionID, pidns string) {
	// pidns is the namespace the server pid was RECORDED in, persisted with
	// the row — never the restoring daemon's namespace, which can differ
	// when state crosses a container boundary (Codex on #7469). An empty
	// record leaves the namespace guards inert, exactly like an unrecorded
	// one — the pid compare then stands on StartID alone.
	id := ServerIdentity{Socket: socket, PID: pid, StartID: startID, SessionID: sessionID, PIDNS: pidns}
	if !id.valid() {
		return
	}
	if t.serverEvidence.Load() == nil {
		stored := id
		t.serverEvidence.Store(&stored)
		t.serverEvidenceRetired.Store(false)
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
