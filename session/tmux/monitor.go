package tmux

import (
	"crypto/sha256"
	"time"
)

// Status-monitor accessors for TmuxSession.
//
// monitor is not immutable: Restore() swaps in a fresh statusMonitor on every
// (re)attach — on the restore/RPC/event-loop goroutines — while the daemon's
// per-second poll reads the pointer and mutates its dead/prevOutputHash fields
// inside HasUpdated(). Left unsynchronized this is a data race (the pointer
// write in Restore vs. the read+field-mutations in HasUpdated), so all access
// goes through monitorMu. HasUpdated() takes the lock only in short bursts —
// snapshot the pointer, then re-acquire to update fields — and deliberately
// does NOT hold it across its `tmux capture-pane` exec, so a slow tmux server
// can't stall Restore's setMonitor(). setMonitor() is the only other writer of
// the pointer (#1528).

// generationResolution is what a rebind learned about the session standing at
// the name, and WHEN it learned it.
//
// startedAt is the instant the resolution began — the same ordering evidence
// HasUpdatedWithBaseline records as captureStartedAt, and it is load-bearing
// for exactly the same reason: liveness observed BEFORE the asking close()
// returned proves nothing about that close's outcome. A zero startedAt reads
// as "no ordering evidence", which fails safe (a mark is kept, so an expected
// teardown can at worst be logged at ERROR — never the reverse).
type generationResolution struct {
	// generation is what display-message resolved at the name; nil when it did
	// not answer or answered nothing provable.
	generation *tmuxGeneration
	// answered is whether the earlier has-session probe answered at all.
	answered bool
	// startedAt is when the resolution probe began; zero when none ran.
	startedAt time.Time
}

// setMonitor swaps in a new status monitor under monitorMu and binds its
// generation from res.
//
//   - res.generation matches the outgoing monitor's generation: the SAME
//     concrete session answered — the fresh monitor SHARES the generation
//     object, so a close() marking the current monitor still reaches a poll in
//     flight on the swapped-out one (Codex on #4473). A settled mark on a
//     session that just answered live proves the teardown did not take, so the
//     resolution retires it — but only a resolution that BEGAN after the
//     settle, per the ordering rule on generationResolution.startedAt. An
//     in-flight (unsettled) teardown keeps its mark.
//   - res.generation is a different generation: the confirmed-live replacement
//     is not the session af closed, so it starts unmarked — a resolved id is
//     affirmative liveness even when the earlier has-session timed out.
//   - nothing answered (generation nil, probe unanswered): a wedged server is
//     no evidence the request resolved, so the outgoing generation — mark
//     included — carries to the replacement monitor.
//   - answered but unresolved (has-session answered, display-message gave no
//     id): confirmed live but unbindable — fresh unbound monitor, unmarked.
func (t *TmuxSession) setMonitor(m *statusMonitor, res generationResolution) {
	t.monitorMu.Lock()
	defer t.monitorMu.Unlock()
	switch {
	case res.generation != nil && t.monitor != nil && t.monitor.generation.sameAs(res.generation):
		m.generation = t.monitor.generation
		// Same settle-ordering guard the capture path applies in
		// HasUpdatedWithBaseline: a resolution that STRADDLED the close —
		// begun while the kill was still in flight, applied after close()
		// returned — saw the session the kill was about to take, so it is no
		// evidence the teardown failed. Retiring on it would let af's own
		// teardown reach ERROR. The two sites must not disagree about what
		// retires a mark (#4473 review).
		if g := m.generation; !g.teardownSettledAt.IsZero() && res.startedAt.After(g.teardownSettledAt) {
			g.teardownInitiated = false
			g.teardownSettledAt = time.Time{}
		}
	case res.generation != nil:
		m.generation = res.generation
	case !res.answered && t.monitor != nil:
		m.generation = t.monitor.generation
	}
	t.monitor = m
}

// seedDeliveryBaseline records the pane captured in the same tmux command queue
// that submitted Enter. Unlike deferring the baseline to the next daemon poll,
// this cannot absorb a fast response that starts and finishes between delivery
// and that poll.
func (t *TmuxSession) seedDeliveryBaseline(content string) {
	t.monitorMu.Lock()
	defer t.monitorMu.Unlock()
	if t.monitor != nil {
		t.monitor.prevOutputHash = t.monitor.hash(content)
		t.monitor.baselinePending = false
		// The seeded capture IS a frame: pin the resize epoch at it so only a
		// resize AFTER this moment can classify a later change as reflow
		// (#7434).
		t.monitor.capturedResizeEpoch = t.monitor.resizeEpoch
	}
}

// deferDeliveryBaseline makes the next successful status capture establish
// comparison state without claiming pane churn. This is the honest fallback
// when tmux proves Enter ran but the capture later in that command queue fails:
// there is no post-delivery frame against which the next pane can be compared.
func (t *TmuxSession) deferDeliveryBaseline() {
	t.monitorMu.Lock()
	defer t.monitorMu.Unlock()
	if t.monitor != nil {
		t.monitor.baselinePending = true
	}
}

// noteResize stamps the status monitor that a daemon-driven size-changing
// resize is about to apply — ResizeWindow calls it only when the probe says
// the apply will move the pane, so a same-size resize (every web subscribe
// and refit resends one) never advances the epoch or opens the grace (#7434).
// It runs BEFORE resize-window so a capture landing mid-apply already reads
// the stamp: the poll shares no lock with the apply, so a post-apply-only
// stamp lets the rewrapped grid read as unmarked churn (#7437 review). The
// stamp is what lets the status poll tell a reflowed capture apart from the
// agent's output. A nil monitor means nothing is polling: no one to classify
// for.
func (t *TmuxSession) noteResize() {
	t.monitorMu.Lock()
	defer t.monitorMu.Unlock()
	if t.monitor != nil {
		t.monitor.resizeEpoch++
		t.monitor.lastAppliedResizeAt = time.Now()
	}
}

// noteResizeApplied rebases the grace at the apply's end so the repaint tail
// it bounds is the one after the resize, not the round trip that applied it.
// The epoch does NOT move again: the pre-apply stamp already drew the
// boundary, and a mid-apply capture that consumed it would leave a second
// mismatch mis-filing the next real output as repaint.
func (t *TmuxSession) noteResizeApplied() {
	t.monitorMu.Lock()
	defer t.monitorMu.Unlock()
	if t.monitor != nil {
		t.monitor.lastAppliedResizeAt = time.Now()
	}
}

type statusMonitor struct {
	// Store hashes to save memory.
	prevOutputHash []byte
	// baselinePending makes the first successful capture after reattaching to an
	// existing process establish state without claiming the pane changed. Fresh
	// starts and respawns leave it false: their first output is genuinely new.
	baselinePending bool
	// dead is set once a capture-pane failure has been confirmed by
	// ExistsOrUnknown() reporting the tmux session is gone. While true,
	// HasUpdated short-circuits and emits no further logs so a stale
	// instance can't flood agent-factory.log (#489). A successful Start or
	// Restore replaces the monitor with a fresh one, which naturally clears
	// this state on respawn.
	dead bool
	// generation is the concrete tmux session this monitor polls, resolved
	// when a live session answered during restore — see tmuxGeneration. The
	// reused NAME cannot bind a generation: a poll that straddles a
	// same-object restart and lands on the replacement would consult this
	// monitor's teardown attribution against the new session's death, and a
	// $id alone cannot either, because tmux ids are per-server counters a
	// replacement server reissues (#4473 review). nil — or an id-less
	// generation — means unbound: the pre-binding name target, no worse
	// than before.
	// Written before the monitor is installed; read under monitorMu.
	generation *tmuxGeneration
	// lastGenerationProbe is when this monitor last spent a command budget on
	// the id-identity probe, which runs on a slower cadence than the capture
	// (generationProbeInterval). Zero means never, so a freshly bound monitor
	// always probes on its first poll. Guarded by monitorMu.
	lastGenerationProbe time.Time
	// resizeEpoch counts daemon-driven window-size changes applied through
	// ResizeWindow — stamped only when the apply actually changed the pane's
	// dimensions, because a same-size resize reflows nothing and must not
	// open the grace that masks real work (#7434). Every af resize path
	// (web subscribe/refit, TUI owner, af sessions attach, remote in-sandbox)
	// funnels through that one call, so its stamp is the signal that a
	// changed capture is VIEWING's repaint rather than the agent's output.
	// Guarded by monitorMu.
	resizeEpoch uint64
	// lastAppliedResizeAt is when resizeEpoch last advanced. It classifies the
	// repaint tail: a SIGWINCH-driven application redraw can land a capture
	// AFTER the one that consumed the epoch mismatch, so a changed frame
	// inside reflowGrace of the stamp is still attributed to viewing.
	// Guarded by monitorMu.
	lastAppliedResizeAt time.Time
	// capturedResizeEpoch is resizeEpoch as of the last successful capture.
	// Inequality means a size-changing apply interposed between that frame
	// and the one being classified — exact ordering evidence, unlike the
	// grace, which cannot reach across a slow poll anyway. Guarded by
	// monitorMu.
	capturedResizeEpoch uint64
}

// generationProbeInterval is the floor between two identity probes for one
// bound monitor. The probe is a `tmux display-message` fork/exec per bound
// session per poll, and at the 1s default poll with ~40 live sessions that
// measured 5.4ms of CPU each — 21.5% of one core, permanently, roughly
// DOUBLING the daemon's status-poll cost (#4473 review; measured on an
// isolated tmux server, 40 sessions x 30 polls x 3 reps, tmux 3.4).
//
// Slowing the cadence is the gate that keeps the mechanism intact. The other
// candidate — probe only after a capture error — would remove the very case
// the probe exists for: a reissued $id answers capture-pane SUCCESSFULLY from
// a foreign session, so there is no error to trigger it and the dead
// generation is never noticed. A capture that DOES fail still resolves the
// identity immediately, through captureTargetAliveOrUnknown, so this interval
// costs the teardown attribution in #4472 nothing at all; it bounds only how
// long a successful capture can read a reissued id before the reuse is seen.
//
// Overridable so tests that poll repeatedly can keep exercising the probe
// rather than silently running the skip path.
var generationProbeInterval = 10 * time.Second

// reflowGrace bounds how long after a size-changing resize-window apply a
// changed capture is still attributed to viewing rather than the agent's
// work (#7434). tmux reflows its own grid in the same apply, so the first
// capture after a resize is caught exactly by the resizeEpoch mismatch; the
// grace exists for the repaint TAIL — a TUI answering SIGWINCH a beat later
// with a full redraw at the new geometry. Three seconds covers a couple of
// 1s polls plus a slow repaint without leaving a stale chip unbounded: real
// output inside the window is masked for at most one idle→run cycle, the
// safe direction (the request survives an extra cycle instead of being
// answered by a repaint). A var so tests can shorten it.
var reflowGrace = 3 * time.Second

// tmuxGeneration is one concrete tmux session as tmux can prove it: the
// session id ($N) plus the SERVER's process id and the session's creation
// stamp. The pair beyond the id matters because tmux ids are per-server
// counters — tearing down the last session exits the server and the next
// server reissues $0 — so a captured $id can name a foreign session after a
// server restart. serverPID and created together distinguish that reuse.
//
// The teardown mark lives HERE, not on the monitor: every monitor bound to
// the same generation shares af's attribution for ITS death. A pure rebind
// installs a fresh monitor for the same live generation, and a close()
// during an in-flight poll on the swapped-out monitor must still classify
// the shared generation's teardown as af-initiated (#4473 review).
// Fields are guarded by monitorMu.
type tmuxGeneration struct {
	sessionID string
	serverPID string
	created   string
	// teardownInitiated records that af itself asked for this session's
	// teardown — set by close() before kill-session runs so the
	// ErrSessionGone branch can tell "af asked" (INFO) from "vanished on its
	// own" (ERROR) (#4472). close() IS af's recorded teardown op, so the mark
	// lands whatever the name resolves to: a monitor whose session was
	// already gone when close() ran is still going silent inside an
	// af-initiated teardown (#5138). It clears only on tmux ANSWERING that
	// the SAME generation is live — a live name is not proof, since the name
	// may already belong to a replacement — see the clear sites in close(),
	// Start, and ClosedConclusivelyAndStillAbsent — except on an unanswered
	// rebind, where the generation object itself is carried to the
	// replacement monitor because a wedged probe is no evidence the request
	// resolved.
	teardownInitiated bool
	// teardownSettledAt is when the asking close() RETURNED; zero while the
	// request is still running or when there is none. A successful capture
	// retires the mark only if the capture began after this stamp — liveness
	// proven before the request finished says nothing about its outcome, so
	// a poll that straddled the close cannot retire the mark just before the
	// kill lands (Codex on #4473).
	teardownSettledAt time.Time
}

// sameAs reports whether two resolved generations are the same concrete tmux
// session. An id-less generation (one created only to carry a mark on an
// unbound monitor) never matches: it describes no provable session.
func (g *tmuxGeneration) sameAs(o *tmuxGeneration) bool {
	return g != nil && o != nil && g.sessionID != "" &&
		g.sessionID == o.sessionID && g.serverPID == o.serverPID && g.created == o.created
}

func newStatusMonitor() *statusMonitor {
	return &statusMonitor{}
}

func newReattachStatusMonitor() *statusMonitor {
	return &statusMonitor{baselinePending: true}
}

// markTeardownInitiated records on the CURRENT monitor's generation that af
// asked for this session's teardown and returns the monitor it marked, so the
// asking close() can settle the same one. Marking must reach the generation
// the polls are reading — not a flag on the session object — or a mid-close
// monitor swap would leave the old generation's in-flight polls reading a
// cleared shared flag (Codex on #4473). Because the mark lives on the
// generation, a monitor installed later for the SAME live session shares it
// rather than restarting attribution from zero. A nil monitor means nothing
// is polling: there is no one to attribute a disappearance to. An unbound
// monitor gets an id-less generation to carry the mark — the pre-binding
// name-scoped behavior.
//
// A BOUND monitor is marked only while the name still resolves to its
// generation: kill-session targets the NAME, so when the name already answers
// for a different generation this request is aimed at the replacement, not at
// the bound session that vanished on its own — and marking the bound
// generation would launder that unrequested death into af's request (Codex on
// #4473, #5138). An answered probe reporting nothing at the name is the same
// refusal in the other direction: the bound generation already vanished
// without af asking, kill-session cannot target it, and marking it would
// launder that unrequested death into af's request at the next poll. The
// archive/kill/on_complete window those already-gone teardowns produce is
// covered instead by the instance-level expectation poll supplies through
// the ExpectingTeardown variants — af's own op fence and kill tombstone —
// which says "tearing down" without claiming this close caused the death
// (#5138). A probe that never answers is different again: no mark lands
// and probeAnswered reports false, because a server wedged enough to refuse
// the probe will fail the kill the same way — close() reports the run
// unknown there rather than paying a second and third command budget for
// the same non-answer (Codex on #4473). The resolution cannot run under
// monitorMu — it is a tmux command with a deadline, exactly what the lock
// ordering forbids holding it across.
// probeAnswered is false only when the name-resolution probe ran and never
// got an answer: the server is wedged, so the caller should not spend another
// command budget on a kill that cannot be delivered — and no mark lands,
// because no teardown request was ever sent (Codex on #4473).
func (t *TmuxSession) markTeardownInitiated() (marked *statusMonitor, probeAnswered bool) {
	t.monitorMu.Lock()
	mon := t.monitor
	t.monitorMu.Unlock()
	if mon == nil {
		return nil, true
	}
	if g := mon.generation; g != nil && g.sessionID != "" {
		live, answered := t.confirmedGeneration()
		if !answered {
			return nil, false
		}
		if live == nil || !g.sameAs(live) {
			return nil, true
		}
	}
	t.monitorMu.Lock()
	defer t.monitorMu.Unlock()
	// A monitor swapped in while the resolution ran binds the generation its
	// own restore confirmed at the name — newer evidence than the answer
	// above — so the mark lands on whatever monitor is current now.
	mon = t.monitor
	if mon == nil {
		return nil, true
	}
	if mon.generation == nil {
		mon.generation = &tmuxGeneration{}
	}
	mon.generation.teardownInitiated = true
	mon.generation.teardownSettledAt = time.Time{}
	return mon, true
}

// settleTeardown stamps that the asking close() returned on the generation it
// marked — what lets a later post-settle successful poll retire a mark whose
// teardown demonstrably did not take. It lands on the marked generation
// itself, so a monitor swap mid-close cannot carry the settle onto the
// replacement.
func (t *TmuxSession) settleTeardown(mon *statusMonitor) {
	if mon == nil {
		return
	}
	t.monitorMu.Lock()
	if mon.generation != nil {
		mon.generation.teardownSettledAt = time.Now()
	}
	t.monitorMu.Unlock()
}

// hash hashes the string.
func (m *statusMonitor) hash(s string) []byte {
	h := sha256.New()
	h.Write([]byte(s))
	return h.Sum(nil)
}
