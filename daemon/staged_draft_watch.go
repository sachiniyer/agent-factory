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

// stagedDraftWatch tracks one send's bound evidence across polls, keyed by
// stableSessionKey like the send re-verification watches beside it. attemptAt
// names the send the evidence belongs to — a newer send supersedes the watch —
// and since is the first tick the Ready+staged condition held. flagged is the
// latch: once the attention flag is raised the watch keeps existing only so a
// later send re-arms it; it never captures or re-raises for the same send, so
// a user's own dismissal of the flag is honored.
type stagedDraftWatch struct {
	attemptAt time.Time
	since     time.Time
	flagged   bool
}

// noteStagedDraftEvidence feeds one poll tick through the session's
// staged-draft watch (#7545). It is the observability half #4200 never had:
// the remedy withholds its second Enter when the draft's ownership is
// unprovable and reports sent-unverified — and until now nothing noticed when
// that report left a bound draft sitting in an idle composer indefinitely.
//
// The gate runs BEFORE any pane read, in cost order: only a session the poll
// itself just settled Ready, whose recorded delivery still says
// sent-unverified, can hold an unsubmitted bound draft. Ready is also the
// mid-turn exclusion — resolveIdleLiveness holds Running while the pane shows
// turn chrome, so text queued behind a working agent (#4622's shape) never
// reaches the capture below. The recheck itself is the runtime's: it spends
// one bounded capture only when the send stashed bound evidence, and the
// verdict ends the watch the moment the evidence is seen submitted (a
// transcript echo above the composer) or gone — the common
// delivered-but-unverified case costs a bounded number of captures and then
// none. Nothing is captured after the flag latches.
//
// A blind tick — unreadable pane, failed capture — neither resets the sustain
// nor resolves the watch: the draft was last proven staged, and the flag
// fires only on a tick whose verdict IS staged.
func (m *Manager) noteStagedDraftEvidence(repoID, key string, instance *session.Instance, as session.AgentServer) {
	attemptAt, status := instance.LastPromptDelivery()
	if instance.GetLiveness() != session.LiveReady ||
		status != session.PromptSentUnverified || attemptAt.IsZero() {
		m.dropStagedDraftWatch(key)
		return
	}
	checker, ok := as.(session.StagedDraftChecker)
	if !ok {
		// A runtime that cannot read bound composer evidence — the off-box
		// backends of v1 — can never arm the watch.
		m.dropStagedDraftWatch(key)
		return
	}
	// The latch check precedes the capture: a send already flagged costs no
	// further pane reads — the entry survives only to re-arm if a newer send
	// supersedes it.
	m.pausedMu.Lock()
	entry := m.stagedDraftWatches[key]
	latched := entry != nil && entry.flagged && entry.attemptAt.Equal(attemptAt)
	m.pausedMu.Unlock()
	if latched {
		return
	}
	read, why := checker.BoundDraftStillStaged()
	now := nowFunc()
	flag := false
	m.pausedMu.Lock()
	switch read {
	case session.StagedDraftStaged:
		entry = m.stagedDraftWatches[key]
		if entry == nil || !entry.attemptAt.Equal(attemptAt) {
			entry = &stagedDraftWatch{attemptAt: attemptAt, since: now}
			m.stagedDraftWatches[key] = entry
		}
		if !entry.flagged && now.Sub(entry.since) >= stagedDraftSettleWindow {
			entry.flagged = true
			flag = true
		}
	case session.StagedDraftUnreadable:
		// See the doc: a blind read keeps an armed watch and its sustain.
	default:
		// StagedDraftNoEvidence, StagedDraftGone, StagedDraftSubmitted: this
		// send's evidence resolved — the watch ends here and this attempt is
		// never captured for again.
		delete(m.stagedDraftWatches, key)
	}
	m.pausedMu.Unlock()
	if flag && !m.raiseStagedDraftAttention(repoID, instance, attemptAt, why) {
		// The raise was refused — an agent's own request holds the row, the
		// session left a live state under us, or the persist failed. Unlatch so
		// the next Ready+staged tick retries rather than losing the flag.
		m.pausedMu.Lock()
		if e := m.stagedDraftWatches[key]; e != nil && e.attemptAt.Equal(attemptAt) {
			e.flagged = false
		}
		m.pausedMu.Unlock()
	}
}

// dropStagedDraftWatch ends a send's watch — the gate no longer holds (the
// session left Ready, a newer send moved the verdict, or the send was never
// armable). Cheap; called on the gate's fast exit paths.
func (m *Manager) dropStagedDraftWatch(key string) {
	m.pausedMu.Lock()
	delete(m.stagedDraftWatches, key)
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
// reports whether the flag actually landed.
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
	// Same opLock→repoStartLock ordering as SetAttention/clearAttentionNow:
	// the whole-row persist below must not interleave with a handoff's
	// provisional swap state.
	opLock := m.opLockFor(daemonInstanceKey(repoID, instance.Title))
	opLock.Lock()
	defer opLock.Unlock()
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()
	// An attention request already on the row — the agent's own question —
	// outranks the flag: overwriting its reason would lose what the agent
	// asked, and the row is already needs-you either way. Refusing leaves the
	// watch unlatched so a later tick raises ours once the row is free.
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
