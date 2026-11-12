package session

import (
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// keptCaptureEvidence is the launch identity a reattach probe could not answer
// about (#7123). settleReattachVerification retires runtimePID/runtimeStartID
// on every answer but a positive match, and everything that reads that pair —
// the load path's claim, the interruption verdict, the limit-wall currentness
// checks, the handoff snapshots — still reads the retire exactly as before.
// But "tmux could not answer" (a timeout under load) is no evidence that the
// agent was replaced, and the retire is persisted, so capture used to fail
// closed for the rest of that runtime. Capture alone keeps the pair here, and
// judges from it only once the session-wide pane scan
// (PaneRootProcessMatches, never the active pane) has found the same process
// rooting one of the session's panes again.
//
// verified is per daemon life and never persisted: each restart re-proves the
// identity before capture trusts it. Guarded by i.mu.
type keptCaptureEvidence struct {
	pid      int
	startID  uint64
	verified bool
}

// ErrNoConversationCaptureEvidence marks every answer of the form "af has no
// verified agent process to attribute a rollout to": no launch record, a
// recorded process that is gone or replaced, or a kept identity not yet
// re-verified. Capture fails closed on it — it never guesses from the
// session's active pane — and the daemon tells an unrecorded row's owner what
// that costs at the next restore (#7124 review F1).
var ErrNoConversationCaptureEvidence = errors.New("af cannot tell which open thread is the agent's own")

// CheckConversationCaptureEvidence reports whether conversation capture has a
// verified agent process to judge from, re-establishing a kept identity
// (keptCaptureEvidence) on the way. Every capture entry point asks it once per
// attempt before attributing anything — the daemon loop, the follow, the
// account swap's synchronous capture and the in-sandbox agent-server — so it
// is the one place capture asks tmux anything, and what it asks is the
// session-wide pane scan.
//
// A kept identity is settled like the reattach probe that kept it: a process
// that has exited or whose pid was reused, or a scan that answers "no pane is
// rooted at it", drops it for good; a scan that cannot answer keeps it for the
// next attempt; a positive match marks it verified for this daemon life, after
// which only the process's own identity is re-checked. None of this touches
// runtimePID/runtimeStartID or runtimeEvidenceGeneration: re-established
// evidence feeds capture and nothing else.
func (i *Instance) CheckConversationCaptureEvidence() error {
	i.mu.RLock()
	override := i.codexRuntimeHeldOverride != nil
	launched := i.runtimePID > 0
	kept := i.captureEvidence
	ts := i.tmuxLocked()
	i.mu.RUnlock()
	if override {
		// A fixture's held answer stands in for the process walk.
		return nil
	}
	if launched || kept.pid <= 0 {
		_, err := i.codexVerifiedRuntimeRoot()
		return err
	}
	alive, err := proctree.SameIdentity(proctree.Process{PID: kept.pid, StartID: kept.startID})
	switch {
	case err != nil:
		// "Could not look" is not "gone": keep it, and do not spend a tmux
		// query on a process that cannot be judged anyway.
		return fmt.Errorf("af could not check the agent process it launched for this session (pid %d): %v, so %w",
			kept.pid, err, ErrNoConversationCaptureEvidence)
	case !alive:
		i.dropCaptureEvidence(kept)
		return fmt.Errorf("the agent process af launched for this session (pid %d) is gone or replaced, so %w",
			kept.pid, ErrNoConversationCaptureEvidence)
	case kept.verified:
		return nil
	case ts == nil:
		return fmt.Errorf("af has no tmux session to re-verify the agent process it launched for this session (pid %d) against, so %w",
			kept.pid, ErrNoConversationCaptureEvidence)
	}
	same, err := ts.PaneRootProcessMatches(kept.pid, kept.startID)
	if err != nil && !tmux.PaneQueryFoundNoPane(err) {
		return fmt.Errorf("af could not yet re-verify that the agent process it launched for this session (pid %d) still roots one of its panes (%v), so %w",
			kept.pid, err, ErrNoConversationCaptureEvidence)
	}
	if err != nil || !same {
		i.dropCaptureEvidence(kept)
		return fmt.Errorf("no pane of this session is rooted at the agent process af launched (pid %d) any more, so %w",
			kept.pid, ErrNoConversationCaptureEvidence)
	}
	if i.markCaptureEvidenceVerified(kept) {
		return nil
	}
	// A launch boundary replaced the identity while the scan ran; the new
	// evidence answers for itself.
	_, err = i.codexVerifiedRuntimeRoot()
	return err
}

// markCaptureEvidenceVerified records a positive re-verification of kept,
// unless a launch boundary superseded it while the scan ran.
func (i *Instance) markCaptureEvidenceVerified(kept keptCaptureEvidence) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.runtimePID != 0 || i.captureEvidence.pid != kept.pid || i.captureEvidence.startID != kept.startID {
		return false
	}
	i.captureEvidence.verified = true
	return true
}

// dropCaptureEvidence retires kept after an answer that it is not the agent,
// unless something replaced it meanwhile. The drop reaches disk with the
// row's next write; until then a restart only re-proves the same answer.
func (i *Instance) dropCaptureEvidence(kept keptCaptureEvidence) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.captureEvidence.pid == kept.pid && i.captureEvidence.startID == kept.startID {
		i.captureEvidence = keptCaptureEvidence{}
	}
}

// captureRuntimeEvidence is a bound channel's evidence answer for the diff:
// whether codexVerifiedRuntimeRoot can name a process right now, with no tmux
// query and no re-verification (CheckConversationCaptureEvidence does that,
// once per attempt). A fixture's held override always has an answer.
func (i *Instance) captureRuntimeEvidence() error {
	i.mu.RLock()
	override := i.codexRuntimeHeldOverride
	i.mu.RUnlock()
	if override != nil {
		return nil
	}
	_, err := i.codexVerifiedRuntimeRoot()
	return err
}

// KeepCaptureEvidenceForTest leaves p as the identity an unanswerable
// reattach probe keeps for capture, unverified. Production reaches it only
// through settleReattachVerification.
func (i *Instance) KeepCaptureEvidenceForTest(p proctree.Process) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.captureEvidence = keptCaptureEvidence{pid: p.PID, startID: p.StartID}
}

// keptCaptureEvidenceFromData restores a kept identity from the durable
// record, unverified. A record that also carries launch evidence cannot have
// come from a retire, so the pair is ignored there rather than trusted.
func keptCaptureEvidenceFromData(data InstanceData) keptCaptureEvidence {
	if data.RuntimePID > 0 || data.CaptureRuntimePID <= 0 {
		return keptCaptureEvidence{}
	}
	return keptCaptureEvidence{pid: data.CaptureRuntimePID, startID: data.CaptureRuntimeStartID}
}
