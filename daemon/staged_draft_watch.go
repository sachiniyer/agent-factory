package daemon

import (
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
)

// stagedDraftSettleWindow is how long a sent-unverified send's bound evidence
// must sit staged in an IDLE composer's live draft — sustained, unbroken Ready
// — before the session is flagged for attention (#7545). It dwarfs the submit
// path's own constants (the #4200 remedy's grace/settle are sub-second) and the
// seconds a turn-boundary queue drain takes, so a transient Ready misread or a
// draft the agent is about to consume can never fire it — and it is bounded
// enough that the seven-day strand this fixes flags at +5 minutes. A var so
// tests can compress it.
var stagedDraftSettleWindow = 5 * time.Minute

// stagedDraftMaxBlindReads bounds one attempt's blind-read budget (#7547
// independent review): StagedDraftUnreadable keeps an armed watch and its
// sustain — deliberately, since a dialog or mid-redraw frame is never a
// negative — but every blind tick still costs a capture-pane. A pane that
// stays unjudgeable for about the settle window's own length (~5 minutes at
// the default 1s poll) cannot be the flag's evidence either way, so the
// attempt retires rather than paying a capture per poll forever. The count
// is consecutive blind reads, not elapsed time: it resets on any provable
// verdict, and a newer send re-arms past it like every other watch state.
const stagedDraftMaxBlindReads = 300

// stagedDraftWatch tracks one send's bound evidence across polls, keyed by
// stableSessionKey like the send re-verification watches beside it. attemptAt
// names the send the evidence belongs to — a newer send supersedes the watch —
// and since is the first tick the Ready+staged condition held. blind counts
// consecutive Unreadable verdicts against the bound above.
//
// closed is the "never captured for again" latch, set by EITHER terminal
// outcome: the flag landed, or the evidence resolved (a transcript echo above
// the composer, gone, or never bound). A closed entry is kept, not deleted,
// precisely so the NEXT Ready+sent-unverified tick for the same attempt skips
// the capture — a delivered send's durable verdict stays sent-unverified, and
// without this memory every idle poll would re-capture it forever (#7545
// review). A newer send's attemptAt mismatch re-arms past it; the sweep drops
// it when the session dies.
//
// due and dueWhy are the deferred flag (#7545 review): the feed runs under the
// instance's observation-settlement fence, where taking the session op lock is
// an ABBA deadlock with sendPromptGuarded — that path holds the op lock while
// it waits on this same fence for automated sends. The feed therefore only
// RECORDS that the flag is due; the poll raises it after the fence releases,
// under the opLock→repoStartLock order every other attention write uses.
type stagedDraftWatch struct {
	attemptAt time.Time
	since     time.Time
	blind     int
	closed    bool
	due       bool
	dueWhy    string
}

// noteStagedDraftEvidence feeds one poll tick through the session's
// staged-draft watch (#7545). It is the observability half #4200 never had:
// the remedy withholds its second Enter when the draft's ownership is
// unprovable and reports sent-unverified — and until now nothing noticed when
// that report left a bound draft sitting in an idle composer indefinitely.
//
// It is called INSIDE the instance's observation-settlement fence and so must
// never acquire the session op lock, the repo start lock, or any lock a send
// takes before that fence — sendPromptGuarded holds the op lock while it waits
// on the fence for automated sends, which made an under-fence flag raise an
// ABBA deadlock (#7545 review). Everything this does under the fence is a leaf
// lock or lock-free: pausedMu (which the send path never takes), the instance's
// own field reads, and the checker's read, which spends an atomic load and one
// bounded capture-pane subprocess — no session mutex. The flag it owes is only
// RECORDED on the watch; raiseDueStagedDraftFlag lands it after the fence
// releases.
//
// The gate runs BEFORE any pane read, in cost order: only a session the poll
// itself just settled Ready, whose recorded delivery still says
// sent-unverified, can hold an unsubmitted bound draft. Ready is also the
// mid-turn exclusion — resolveIdleLiveness holds Running while the pane shows
// turn chrome, so text queued behind a working agent (#4622's shape) never
// reaches the capture below. A blind tick — unreadable pane, failed capture —
// neither resets the sustain nor resolves the watch: the draft was last proven
// staged, and the flag is marked due only on a tick whose verdict IS staged.
func (m *Manager) noteStagedDraftEvidence(repoID, key string, instance *session.Instance, as session.AgentServer) {
	attemptAt, status := instance.LastPromptDelivery()
	if status != session.PromptSentUnverified || attemptAt.IsZero() {
		// The attempt left sent-unverified — delivered, not-delivered, or a
		// newer send superseded it. Its watch retires outright, blind budget
		// included: the budget may survive only for the SAME still-
		// unverified attempt, never past a verdict change (Codex #7547,
		// sixth round).
		m.retireStagedDraftWatch(key)
		return
	}
	if instance.GetLiveness() != session.LiveReady {
		// A still-unverified attempt out of Ready — a flap, not a
		// resolution. The armed state resets so nothing owed carries over,
		// but the blind budget it already spent must not re-arm at zero
		// when the pane comes back (Codex #7547, fifth round).
		m.dropStagedDraftWatch(key)
		return
	}
	checker, ok := as.(session.StagedDraftChecker)
	if !ok {
		// A runtime that cannot read bound composer evidence — a dead or
		// exotic backend — can never arm the watch. The off-box runtimes
		// DO implement it since #7546: remoteAgentServer ferries the
		// sandbox's own checker over the wire.
		m.dropStagedDraftWatch(key)
		return
	}
	// The closed check precedes the capture: an attempt whose flag already
	// landed — or whose evidence already resolved — is never captured for
	// again. The entry survives only to re-arm if a newer send supersedes it.
	m.pausedMu.Lock()
	entry := m.stagedDraftWatches[key]
	settled := entry != nil && entry.closed && entry.attemptAt.Equal(attemptAt)
	m.pausedMu.Unlock()
	if settled {
		return
	}
	// Another attention request owns the row — an agent's own question. The
	// flag could not land while it stands (raiseStagedDraftAttention refuses
	// to clobber it), so neither a capture nor a due mark is spent here: the
	// watch keeps whatever it has armed, and the tick after the row frees
	// takes the read again (#7547 independent review).
	if instance.Attention() != nil {
		return
	}
	read, why := checker.BoundDraftStillStaged()
	now := nowFunc()
	m.pausedMu.Lock()
	defer m.pausedMu.Unlock()
	switch read {
	case session.StagedDraftStaged:
		entry = m.stagedDraftWatches[key]
		if entry == nil || !entry.attemptAt.Equal(attemptAt) {
			entry = &stagedDraftWatch{attemptAt: attemptAt, since: now}
			m.stagedDraftWatches[key] = entry
		}
		// A blind streak may own this entry already: its since is unset,
		// and must stay unset — the window measures sustained STAGED
		// evidence, so it opens on this first provable staged read, never
		// backdated to the blind ticks (Codex #7547, third round: a zero
		// since flags a fresh send in seconds).
		if entry.since.IsZero() {
			entry.since = now
		}
		entry.blind = 0
		if !entry.closed && now.Sub(entry.since) >= stagedDraftSettleWindow {
			entry.due = true
			entry.dueWhy = why
		}
	case session.StagedDraftUnreadable:
		// A blind read keeps an armed watch and its sustain — but a due
		// mark it carries is stale: the raise revalidates the attempt and
		// liveness, not the pane, so a draft cleared between polls followed
		// by a redraw would flush a flag for evidence that is no longer
		// staged. The pending raise waits for a CURRENT staged verdict —
		// the window itself survives the blind tick (Codex #7547, fifth
		// round). And the capture budget stays bounded
		// (stagedDraftMaxBlindReads): a pane unjudgeable for longer than
		// the settle window's own length retires the attempt rather than
		// capturing per poll forever.
		entry = m.stagedDraftWatches[key]
		if entry == nil || !entry.attemptAt.Equal(attemptAt) {
			entry = &stagedDraftWatch{attemptAt: attemptAt}
			m.stagedDraftWatches[key] = entry
		}
		entry.due = false
		entry.dueWhy = ""
		entry.blind++
		if entry.blind > stagedDraftMaxBlindReads {
			entry.closed = true
		}
	default:
		// StagedDraftNoEvidence, StagedDraftGone, StagedDraftSubmitted: this
		// send's evidence resolved. Close the entry rather than deleting it —
		// the recorded verdict stays sent-unverified, so without the memory
		// every idle poll would re-ask the pane (#7545 review).
		entry = m.stagedDraftWatches[key]
		if entry == nil || !entry.attemptAt.Equal(attemptAt) {
			entry = &stagedDraftWatch{attemptAt: attemptAt}
			m.stagedDraftWatches[key] = entry
		}
		entry.closed = true
		entry.due = false
		entry.dueWhy = ""
	}
}

// raiseDueStagedDraftFlag lands the attention flag a poll tick recorded while
// it held the observation-settlement fence. It runs AFTER that fence releases
// (the defer pair in refreshInstanceStatus orders it: registered before the
// release, run after it), where taking the session op lock no longer deadlocks
// an automated send waiting on the fence (#7545 review).
//
// Between the under-fence tick and this flush the condition it saw can move —
// a sendPromptGuarded holding the op lock may complete a whole delivery — so
// the flag is re-validated under the lock before it is written: same watch,
// same attemptAt, still due and unclosed; the recorded delivery still
// sent-unverified for that attempt; the session still Ready; the attention
// row still free. Anything that moved simply clears the due mark — a tick
// that still qualifies re-marks it. And the flush itself never blocks the
// poll: the op lock is TryLock'd, so contention keeps the due mark for the
// next tick rather than parking every session's sweep behind one operation.
func (m *Manager) raiseDueStagedDraftFlag(repoID, key string, instance *session.Instance) {
	m.pausedMu.Lock()
	entry := m.stagedDraftWatches[key]
	if entry == nil || !entry.due {
		m.pausedMu.Unlock()
		return
	}
	attemptAt, dueWhy := entry.attemptAt, entry.dueWhy
	m.pausedMu.Unlock()

	// Same opLock→repoStartLock ordering as SetAttention/clearAttentionNow:
	// the whole-row persist below must not interleave with a handoff's
	// provisional swap state. TryLock, never Lock (#7547 independent
	// review): this runs on the poll goroutine, and a kill, archive, or
	// handoff holding the op lock would stall every session's tick behind
	// it — settlement.go's rule for the same class of poll-side write. The
	// due mark STAYS on contention: the flag is still owed, and the next
	// poll's deferred flush retries while the condition holds.
	opLock := m.opLockFor(daemonInstanceKey(repoID, instance.Title))
	if !opLock.TryLock() {
		return
	}
	defer opLock.Unlock()
	// Same rule for the repo start lock: CreateSession holds it across a
	// whole provisioning + send, and the sweep is serial — a blocking wait
	// here suspends every sibling's poll behind one create (Codex #7547).
	repoStartLock := m.startLockForRepo(repoID)
	if !repoStartLock.TryLock() {
		return
	}
	defer repoStartLock.Unlock()

	m.pausedMu.Lock()
	entry = m.stagedDraftWatches[key]
	stillDue := entry != nil && entry.due && !entry.closed && entry.attemptAt.Equal(attemptAt)
	m.pausedMu.Unlock()
	nowAttempt, status := instance.LastPromptDelivery()
	if !stillDue || status != session.PromptSentUnverified || !nowAttempt.Equal(attemptAt) ||
		instance.GetLiveness() != session.LiveReady || instance.Attention() != nil {
		m.clearStagedDraftDue(key, attemptAt)
		return
	}
	if !m.raiseStagedDraftAttention(repoID, instance, attemptAt, dueWhy) {
		// The raise was refused — an agent's own request landed on the row
		// while we waited, or the persist failed. Clear the due mark so the
		// next Ready+staged tick re-arms it rather than losing the flag.
		m.clearStagedDraftDue(key, attemptAt)
		return
	}
	// The flag latched: this send never needs another capture or retry.
	m.pausedMu.Lock()
	if e := m.stagedDraftWatches[key]; e != nil && e.attemptAt.Equal(attemptAt) {
		e.closed = true
		e.due = false
		e.dueWhy = ""
	}
	m.pausedMu.Unlock()
}

// clearStagedDraftDue consumes a due mark whose flush could not land the flag
// — the condition it re-validates moved while the op lock was waited on, or
// the raise itself was refused. The next qualifying tick re-marks it, so a
// refused flag retries on the poll's own cadence.
func (m *Manager) clearStagedDraftDue(key string, attemptAt time.Time) {
	m.pausedMu.Lock()
	if e := m.stagedDraftWatches[key]; e != nil && e.attemptAt.Equal(attemptAt) {
		e.due = false
		e.dueWhy = ""
	}
	m.pausedMu.Unlock()
}

// retireStagedDraftWatch ends a send's watch outright — its verdict moved
// off sent-unverified, so nothing about the attempt may be carried forward,
// blind budget included (#7547 independent review, sixth round). The closed
// latch a kept entry provides is irrelevant here: this path means the
// recorded verdict itself changed, so the gate never feeds this attempt
// again — and a newer send's attemptAt never matches it.
func (m *Manager) retireStagedDraftWatch(key string) {
	m.pausedMu.Lock()
	delete(m.stagedDraftWatches, key)
	m.pausedMu.Unlock()
}

// dropStagedDraftWatch disarms a send's watch while its attempt is still
// sent-unverified — the session left Ready mid-watch, or the backend cannot
// read bound evidence. The entry is KEPT, not deleted, for the blind budget
// it already spent (#7547 independent review): a Ready→Running→Ready flap
// must not re-spend 300 captures by resetting the count. The armed state
// alone resets — since reopens on the next provable staged read, due clears
// — and a newer send's attemptAt mismatch still re-arms fresh like every
// other watch state. A verdict change is a retirement, not a drop: see
// retireStagedDraftWatch.
func (m *Manager) dropStagedDraftWatch(key string) {
	m.pausedMu.Lock()
	if e := m.stagedDraftWatches[key]; e != nil && !e.closed {
		e.since = time.Time{}
		e.due = false
		e.dueWhy = ""
	}
	m.pausedMu.Unlock()
}

// sweepStagedDraftWatches drops watches whose session no longer exists — the
// poll is the only feed, so a watch it can no longer reach is a permanent
// leak. Called once per sweep from RefreshStatuses beside
// sweepUnverifiedSendWatches; m.mu and pausedMu are taken separately, never
// nested.
func (m *Manager) sweepStagedDraftWatches() {
	m.pausedMu.Lock()
	size := len(m.stagedDraftWatches)
	m.pausedMu.Unlock()
	if size == 0 {
		return // nothing armed: skip the instance walk in the common case
	}
	m.mu.Lock()
	live := make(map[string]struct{}, len(m.instances))
	for key, inst := range m.instances {
		repoID, _ := splitDaemonInstanceKey(key)
		live[stableSessionKey(repoID, inst)] = struct{}{}
	}
	m.mu.Unlock()
	m.pausedMu.Lock()
	defer m.pausedMu.Unlock()
	for key := range m.stagedDraftWatches {
		if _, ok := live[key]; !ok {
			delete(m.stagedDraftWatches, key)
		}
	}
}

// stagedDraftAttentionReason is the flag's one-line copy — sentence case, the
// clause set off by —, short enough for the TUI attention line. It names the
// send's instant and the two safe resolutions, and by contract it may never
// carry any part of the prompt (#7545: no prompt text leaves the pane).
func stagedDraftAttentionReason(attemptAt time.Time) string {
	return fmt.Sprintf(
		"A prompt sent at %s is still unsubmitted in the composer — attach and press Enter, or send a fresh prompt",
		attemptAt.Local().Format("Jan 2 15:04"))
}

// raiseStagedDraftAttention sets the session's attention request for a send
// whose bound draft has outlived the settle window in an idle composer, and
// persists and publishes it the way every other attention write does. It
// reports whether the flag actually landed. Callers hold the session's op
// lock and repo start lock — the ordering daemon/attention.go's own writes
// use — and run it after the observation-settlement fence releases: with
// TryLock at the caller that order is hygiene, not deadlock prevention — a
// contended acquisition fails fast and keeps the due mark rather than
// parking the sweep (#7547 independent review).
//
// The flag is the whole resolution path by design (#7545, flag-only): the
// recheck never presses Enter. #4200's remedy Enter is safe only inside its
// own check-then-act window — inputMu held, a boundary frame proving "only
// our chip", a grace frame proving stillness. At recheck time there is no
// boundary; "bound evidence still present" proves the draft CONTAINS af's
// text, not that the composer holds only it unmodified, and auto-submitting
// an instruction that may be days old could run something the sender no
// longer wants. The person who sent it decides, with the flag telling them
// the two safe resolutions: attach and press Enter to submit the staged
// draft, or send-prompt, whose pre-clear replaces it.
func (m *Manager) raiseStagedDraftAttention(repoID string, instance *session.Instance, attemptAt time.Time, why string) bool {
	// An attention request already on the row — the agent's own question —
	// outranks the flag: overwriting its reason would lose what the agent
	// asked, and the row is already needs-you either way. Refusing leaves the
	// watch unclosed so a later tick raises ours once the row is free.
	if instance.Attention() != nil {
		return false
	}
	// The reason names the send time and the remedy, never the prompt: no
	// part of the payload may leave the pane (#7545).
	reason := stagedDraftAttentionReason(attemptAt)
	previous, err := instance.SetAttention(reason, nowFunc())
	if err != nil {
		return false // the session moved out of a live, attention-eligible state
	}
	data := instance.ToInstanceData()
	if err := persistInstanceData(repoID, data); err != nil {
		instance.RestoreAttention(previous)
		m.warn().Printf("session %q: could not persist the staged-draft attention flag: %v", instance.Title, err)
		return false
	}
	m.publishEvent(agentproto.EventSessionUpdated, data)
	m.warn().Printf("session %q: a prompt sent at %s was reported sent-unverified and its bound draft is still staged in the idle composer (%s); flagged for attention (#7545)",
		instance.Title, attemptAt.UTC().Format(time.RFC3339), why)
	return true
}
