package daemon

import (
	"sort"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// The usage-limit auto-resume scheduler (#1146 PR3): the opt-in daemon loop that
// re-prompts a session parked at a usage-limit wall once its limit window has
// elapsed. It is the automated sibling of the PR2 manual `c` retry and the
// Lost-session restore loop (lostrestore.go) — same poll-goroutine discipline
// (TryLock the per-session op lock, re-verify under it, back off on repeat) —
// and it reuses the exact resumeFromLimit action the manual retry uses, so the
// resume mechanics (re-spawn if the agent exited, re-deliver the stored prompt,
// clear the limit) live in one place.
//
// Gated by config.LimitAutoResume (default OFF): when off, ResumeLimitedSessions
// returns immediately and a limit stays surface-only (badge + manual retry).
// When on, a LiveLimitReached row is resumed at limitResumeAt = its parsed reset
// time + limitResumeGrace; a row whose banner carried NO parseable reset time is
// retried on the fixed config.LimitRetryInterval fallback, or left surface-only
// when that fallback is unset. A resume that itself re-hits the wall (the row
// re-enters LiveLimitReached) is re-scheduled under exponential backoff so the
// daemon never hammers a genuinely-exhausted plan. All comparisons are in UTC —
// limitResetAt is stored UTC by the detector.

// limitResumeGrace is added to a parsed reset time before a session is
// auto-resumed (#1146). Usage-limit windows are rolling and approximate and the
// banner's stated reset time is a lower bound, so a small buffer avoids resuming
// straight back into a still-limited wall and immediately re-parking.
const limitResumeGrace = 2 * time.Minute

// Backoff between repeat auto-resume attempts for one session — a resume that
// re-hits the wall, or a resumeFromLimit that errors. Package vars so tests can
// shorten them (same pattern as lostRestoreBackoff*). Mirrors the lostrestore
// discipline: exponential from base, settling at max, never a permanent
// give-up. The no-parseable-reset-time fallback uses the fixed configured
// interval instead of this backoff (see limitResumeAttempted).
var (
	limitResumeBackoffBase = 10 * time.Second
	limitResumeBackoffMax  = 5 * time.Minute
)

// limitResumeState is the per-session auto-resume retry state, keyed by daemon
// instance key and guarded by Manager.mu (the loop runs on the poll goroutine;
// tests drive ResumeLimitedSessions directly). parkedAt anchors the
// no-parseable-reset-time fallback interval (the first tick the session was seen
// parked); nextAttempt is the backoff gate that ALSO survives the brief non-limit
// window between resumeFromLimit clearing the limit and the next poll re-detecting
// the banner, so an immediate re-limit continues the backoff instead of hammering.
type limitResumeState struct {
	attempts    int
	nextAttempt time.Time
	parkedAt    time.Time
}

// ResumeLimitedSessions runs one auto-resume pass over every LiveLimitReached
// session the manager owns (#1146 PR3). A no-op when limit_auto_resume is off or
// the manager is still warming up — the whole feature is opt-in and does zero
// scheduling otherwise. Called from the daemon poll loop after
// RestoreLostSessions. Mirrors RestoreLostSessions: snapshot under m.mu, prune
// dead/resolved retry state, then attempt each session in a stable order so the
// logs read coherently.
func (m *Manager) ResumeLimitedSessions() {
	if !m.cfg.LimitAutoResume || !m.Ready() {
		return
	}
	retryInterval := m.cfg.LimitRetryIntervalDuration()

	type entry struct {
		key      string
		repoID   string
		instance *session.Instance
	}
	now := nowFunc()
	m.mu.Lock()
	entries := make([]entry, 0, len(m.instances))
	for key, inst := range m.instances {
		repoID, _ := splitDaemonInstanceKey(key)
		entries = append(entries, entry{key: key, repoID: repoID, instance: inst})
	}
	// Drop retry state for sessions that are gone, or that have stayed OUT of
	// LimitReached past their backoff window (a resume that stuck — the episode
	// is over). State is deliberately KEPT for a row that is momentarily
	// non-limit within its backoff window: that window is the gap between
	// resumeFromLimit clearing the limit and the next poll re-detecting the
	// banner, and keeping the state there is what throttles an immediate
	// re-limit instead of restarting it at attempt zero.
	for key, inst := range m.instances {
		st := m.limitResumeStates[key]
		if st == nil {
			continue
		}
		if inst.GetLiveness() != session.LiveLimitReached && !now.Before(st.nextAttempt) {
			delete(m.limitResumeStates, key)
		}
	}
	for key := range m.limitResumeStates {
		if _, live := m.instances[key]; !live {
			delete(m.limitResumeStates, key)
		}
	}
	m.mu.Unlock()

	sort.Slice(entries, func(i, j int) bool { return entries[i].key < entries[j].key })
	for _, e := range entries {
		m.resumeLimitedSession(e.key, e.repoID, e.instance, retryInterval)
	}
}

// resumeLimitedSession auto-resumes one session when it is eligible and its
// limit window has elapsed. Eligibility mirrors restoreLostSession: started,
// LiveLimitReached, not tombstoned, not the reserved root (the manual retry and
// the poll own those), and no kill in flight. It takes the per-target lock and
// then the per-session op lock in that canonical order (#2006) before invoking the
// shared resumeFromLimitLocked body; the op lock is only TryLock'd so the poll
// goroutine never stalls behind a kill teardown, and everything is re-verified
// under the locks because the checks above are point-in-time.
func (m *Manager) resumeLimitedSession(key, repoID string, inst *session.Instance, retryInterval time.Duration) {
	if inst == nil || !inst.Started() || inst.GetLiveness() != session.LiveLimitReached {
		return
	}
	if inst.UserKilled() || session.IsReservedTitle(inst.Title) {
		return
	}

	now := nowFunc()
	m.mu.Lock()
	if _, killing := m.killsInFlight[key]; killing {
		m.mu.Unlock()
		return
	}
	resetAt, hasReset := inst.LimitResetAt()
	if !hasReset && retryInterval <= 0 {
		// No parseable reset time and no fallback interval: the session is
		// surface-only. Return WITHOUT creating retry state so unschedulable
		// parks never accumulate a map entry.
		m.mu.Unlock()
		return
	}
	st := m.limitResumeStates[key]
	if st == nil {
		st = &limitResumeState{parkedAt: now}
		m.limitResumeStates[key] = st
	}
	// due = when this session first becomes eligible. With a parsed reset time it
	// is reset + grace (a reset already in the past yields a due in the past →
	// resume promptly); with no reset time it is the fixed fallback interval
	// measured from when the session was first seen parked. All in UTC.
	due := st.parkedAt.Add(retryInterval)
	if hasReset {
		due = resetAt.Add(limitResumeGrace)
	}
	// The per-attempt backoff/interval gate sits on top of the due time, so the
	// first fire lands at due and repeats are throttled.
	if st.nextAttempt.After(due) {
		due = st.nextAttempt
	}
	skip := now.Before(due)
	m.mu.Unlock()
	if skip {
		return
	}

	// Canonical lock order is target-before-op (#2006); see resumeFromLimit. This
	// runs on the poll goroutine, so it can briefly block here on a DeliverPrompt
	// in flight to the same session — bounded and correct, exactly where taking the
	// op lock first used to deadlock the whole daemon. The op lock is still only
	// TryLock'd, so the poll never stalls behind a kill teardown that holds it.
	unlockTarget := m.lockTarget(repoID, inst.Title)
	defer unlockTarget()

	opLock := m.opLockFor(key)
	if !opLock.TryLock() {
		// A kill (or its finish pass) holds the session; retry next tick.
		return
	}
	defer opLock.Unlock()

	// Re-verify under the lock: a kill may have torn the session down, a
	// self-recovery or the manual retry may have cleared the limit, or the map
	// entry may have been replaced since the point-in-time checks above. Resume
	// only what is still provably a wanted, limit-blocked session.
	m.mu.Lock()
	current := m.instances[key]
	_, killing := m.killsInFlight[key]
	m.mu.Unlock()
	if killing || current != inst || inst.UserKilled() || session.IsReservedTitle(inst.Title) || inst.GetLiveness() != session.LiveLimitReached {
		return
	}

	// Whether this episode carried a parseable reset time, captured BEFORE the
	// resume: resumeFromLimit clears the limit (and the reset time) on success,
	// so it cannot be read back afterwards. It selects the re-schedule cadence.
	_, hadReset := inst.LimitResetAt()

	// Record the attempt and set the backoff/interval gate BEFORE firing, so a
	// resume that re-hits the wall on the very next tick is already throttled —
	// never a hot re-limit loop.
	attempts, wait := m.limitResumeAttempted(st, now, hadReset, retryInterval)

	if err := m.resumeFromLimitLocked(repoID, key, inst, inst.Title); err != nil {
		log.WarningLog.Printf("auto-resume of limit-blocked session %q failed (attempt %d), backing off %s: %v", inst.Title, attempts, wait, err)
		return
	}
	log.InfoLog.Printf("auto-resumed limit-blocked session %q (repo %s) after its usage-limit window elapsed (attempt %d)", inst.Title, repoID, attempts)
}

// limitResumeAttempted records one auto-resume attempt and sets the gate for the
// next one, returning the new attempt count and the wait applied (#1146). With a
// parsed reset time (or when no fixed fallback interval applies) repeats back off
// exponentially — doubling from base, settling at max, never a permanent
// give-up, mirroring lostRestoreFailed. With no reset time and a configured
// interval, repeats use that fixed interval (§4: "retry on that fixed
// interval"). Must be called with attempts reflecting THIS fire, so it increments
// first. Guarded by m.mu.
func (m *Manager) limitResumeAttempted(st *limitResumeState, now time.Time, hadReset bool, retryInterval time.Duration) (int, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st.attempts++
	wait := retryInterval
	if hadReset || retryInterval <= 0 {
		wait = limitResumeBackoffFor(st.attempts)
	}
	st.nextAttempt = now.Add(wait)
	return st.attempts, wait
}

// limitResumeBackoffFor is the exponential backoff for the nth attempt (n>=1):
// base<<(n-1), capped at max. The shift is guarded like lostRestoreFailed's —
// past ~16 doublings the exponential form is meaningless and would overflow.
func limitResumeBackoffFor(attempts int) time.Duration {
	backoff := limitResumeBackoffMax
	if shift := attempts - 1; shift >= 0 && shift < 16 {
		if b := limitResumeBackoffBase << shift; b < backoff {
			backoff = b
		}
	}
	return backoff
}
