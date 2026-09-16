package daemon

import (
	"errors"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// rootEnsureBackoffReset clears one candidate's retry state without touching
// the repo-scoped carry — the reset every ensured-or-moot pass owes its own
// backoff, split from the carry retirement that only a pass leaving a healthy
// root may perform (#4400 review round 5).
func (m *Manager) rootEnsureBackoffReset(st *rootEnsureState) {
	m.mu.Lock()
	st.consecutiveFailures = 0
	st.unansweredFailures = 0
	st.escalated = false
	st.escalatedPersistent = false
	st.nextAttempt = time.Time{}
	st.suppressLogged = false
	m.mu.Unlock()
}

// rootEnsureSucceeded resets a repo's retry state after a pass that left a
// healthy root in place (freshly created or adopted). The repoID keys the
// pending reaped carry the same pass makes moot — by repository, not by this
// candidate's state key, because two spellings of one repo share one carry.
func (m *Manager) rootEnsureSucceeded(repoID string, st *rootEnsureState) {
	m.rootEnsureBackoffReset(st)
	m.mu.Lock()
	// A create still in flight owns the carry: the row this pass adopted can
	// be that create's provisional publication — CreateSession registers it
	// before startup and readiness finish — and retiring the carry now would
	// delete the state the running create still needs if it fails and removes
	// its provisional row. The create retires it after recording its own
	// outcome; the next pass covers every other exit (#4400 review round 4).
	_, inFlight := m.rootCreatesInFlight[repoID]
	if !inFlight {
		delete(m.reapedRootCarries, repoID)
	}
	m.mu.Unlock()
	if !inFlight {
		// Unconditional, not gated on the map: after a restart the file can
		// outlive the map — this pass adopted a healthy root and never
		// hydrated the carry — and a stale durable carry would be consumed by
		// the NEXT no-record create long after this root was healthy. One
		// ENOENT unlink per healthy tick is the cost of never resurrecting an
		// obsolete pin.
		m.removeReapedRootCarry(repoID)
	}
}

// rootEnsureFailed records a failed ensure attempt: exponential backoff up to
// rootEnsureBackoffMax, where the retry cadence stays for as long as the
// failures do. Retrying forever (instead of giving up until restart) is what
// guarantees a root heals after a tmux-server outage of any length — an
// outage is indistinguishable from a broken config while it lasts, and only
// a later retry can tell the difference (#1122). The cost for a genuinely
// broken config is one cheap failed attempt per cadence interval, each
// logged. Crossing rootEnsureEscalationThreshold logs an ERROR so a
// persistent cause is visible without waiting for a user to notice the
// missing root — worded for what the attempts actually established, and
// re-logged if that changes (#3500).
func (m *Manager) rootEnsureFailed(path string, st *rootEnsureState, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st.consecutiveFailures++
	if errors.Is(err, config.ErrRepoProbeUnanswered) {
		st.unansweredFailures++
	}
	backoff := rootEnsureBackoffFor(st.consecutiveFailures)
	st.nextAttempt = time.Now().Add(backoff)
	if rootEnsureShouldEscalate(st) {
		st.escalated = true
		st.escalatedPersistent = rootEnsureCauseIsEstablished(st)
		m.err().Printf("root agent ensure for %q failed %d consecutive times; %s — will keep retrying every %s: %v", path, st.consecutiveFailures, rootEnsureEscalationCause(st), rootEnsureBackoffMax, err)
		return
	}
	m.warn().Printf("root agent ensure for %q failed (attempt %d), retrying in %s: %v", path, st.consecutiveFailures, backoff, err)
}

// rootEnsureAnsweredFailures counts the failures in the current streak that
// produced a real error rather than ending before git could answer. Caller
// holds m.mu.
func rootEnsureAnsweredFailures(st *rootEnsureState) int {
	return st.consecutiveFailures - st.unansweredFailures
}

// rootEnsureCauseIsEstablished reports whether the streak has the evidence a
// persistence claim needs: a full threshold of failures that actually reported
// something. It is the one predicate both the escalation decision and its
// wording read, so the two cannot drift apart. Caller holds m.mu.
func rootEnsureCauseIsEstablished(st *rootEnsureState) bool {
	return rootEnsureAnsweredFailures(st) >= rootEnsureEscalationThreshold
}

// rootEnsureShouldEscalate decides whether a streak gets an escalation ERROR
// now. Once when it crosses the threshold, plus at most once more for the one
// transition that changes what the ERROR may claim: a streak escalated as
// "cause unknown" whose failures LATER start answering has established a real
// persistent cause, and the old strict equality on the threshold could never
// report it — the count is already past the threshold, so the genuine cause
// would be logged as warnings forever while the root stayed down (#3500
// review).
//
// The trigger and the CLAIM are deliberately separate. Visibility is owed after
// a threshold of consecutive failures whatever they were — the root has been
// down that long either way — but "the cause looks persistent" is owed only
// once a threshold of failures has actually reported something. So the first
// ERROR always fires on the count, worded for the evidence, and the upgrade
// fires once that evidence arrives.
//
// The bar is a full threshold rather than a single answered failure because
// this path does not see only repo probes: rootEnsureFailed also records a
// failed session create and a failed dead-root reap, neither of which carries
// the unanswered sentinel. One transient tmux failure must not turn "cause
// unknown" into "looks persistent" (#3500 review round 2) — and a MIXED first
// streak must not either, nor lock itself out of the upgrade by having claimed
// persistence on that one failure (round 3).
//
// Bounded at two ERRORs per streak: escalatedPersistent only ever goes false to
// true, since an answered failure is never un-answered later in the same
// streak. Caller holds m.mu.
func rootEnsureShouldEscalate(st *rootEnsureState) bool {
	if !st.escalated {
		return st.consecutiveFailures >= rootEnsureEscalationThreshold
	}
	return !st.escalatedPersistent && rootEnsureCauseIsEstablished(st)
}

// rootEnsureEscalationCause words what the escalation ERROR is entitled to
// claim about the cause. Attempts whose repo probe went unanswered (#3500)
// still count toward the backoff — they must, since the retry cadence is what
// keeps a loaded box from forking git every tick, and #1122's retry-forever
// contract is unchanged — but they are not evidence of anything: an attempt
// that never got an answer out of git has established nothing about the
// repository or the configuration. Caller holds m.mu.
func rootEnsureEscalationCause(st *rootEnsureState) string {
	answered := rootEnsureAnsweredFailures(st)
	switch {
	case answered == 0:
		return "no attempt got an answer out of git, so the cause is unknown; a repo probe that keeps dying says nothing about the repository or its configuration"
	case !rootEnsureCauseIsEstablished(st):
		return fmt.Sprintf("only %d of those attempts reported a real error and the rest ended before git could answer, so the cause is not established", answered)
	case st.unansweredFailures > 0:
		return fmt.Sprintf("the cause looks persistent, though %d of those attempts ended before git could answer", st.unansweredFailures)
	default:
		return "the cause looks persistent"
	}
}

// rootAgentProgramForProfile resolves the command the root agent runs from a
// resolved root-agent profile. An explicit program wins verbatim (a bare agent
// name resolves through program_overrides downstream, exactly like any session
// program). The default profile — an empty program — is the repo's resolved
// claude command with --dangerously-skip-permissions ensured, the root agent's
// whole purpose being autonomous operation (#1106).
