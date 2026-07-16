package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// The amp panes below are structurally the real thing — a labeled top rule, the
// "│" interior rows, and the bottom rule that does or does not carry a status
// segment — reduced to the parts the detector reads. The BYTE-level fidelity to a
// live amp is pinned separately, against real `capture-pane -p -e -J` fixtures, in
// task.TestIsWorkingContentAmpRealCapture; what these exercise is the daemon
// WIRING: which liveness the poll's still-pane branch settles on.
const (
	ampPaneStreaming = "╭──────────────── medium ─╮\n" +
		"│                         │\n" +
		"╰ ∼ Streaming ─────── repo (master) ─╯\n"
	ampPaneThinking = "╭──────────────── medium ─╮\n" +
		"│                         │\n" +
		"╰ ∼ Thinking ─────── repo (master) ─╯\n"
	ampPaneIdle = "╭──────────────── medium ─╮\n" +
		"│                         │\n" +
		"╰─────────────────── repo (master) ─╯\n"
)

// stillPaneBackend is a FakeBackend whose every tick reports (updated=false,
// hasPrompt=false) with fixed content: a pane that DID NOT CHANGE since the last
// poll. IsAlive inherits FakeBackend's true, so the daemon takes its idle branch
// and has to decide what stillness means from the content alone — which is the
// decision the amp flash got wrong.
type stillPaneBackend struct {
	*session.FakeBackend
	content string
}

func (b stillPaneBackend) HasUpdated(*session.Instance) (bool, bool, string) {
	return false, false, b.content
}

// registerStartedAmp mirrors registerStarted for an amp session — registerStarted
// hardcodes Program "claude", and the program is exactly what ResolvedAgent reads
// to pick the detector, so it cannot be reused here.
func registerStartedAmp(t *testing.T, m *Manager, repoID, repoPath, title, pane string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: "amp"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetBackend(stillPaneBackend{FakeBackend: session.NewFakeBackend(), content: pane})
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	seedDiskInstance(t, repoID, title, repoPath)
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, title)] = inst
	m.mu.Unlock()
	return inst
}

// TestRefreshStatuses_AmpWorkingBehindStillPaneStaysRunning is the status-flash
// regression (Sachin, live). amp holds a STATIC frame while it works and repaints
// only when output actually lands, so a quiet gap mid-turn (between token bursts,
// during a tool call, while the model thinks) leaves the pane byte-identical
// across the 1s daemon poll. The poll infers "working" from pane churn, so it read
// that stillness as idle and settled Ready — painting the green
// "waiting-for-you" dot (#1766) mid-turn, then dropping it on the next burst: the
// flash. In the live repro three consecutive ticks went green while amp displayed
// "Thinking".
//
// A still pane showing amp's in-progress indicator must stay Running.
func TestRefreshStatuses_AmpWorkingBehindStillPaneStaysRunning(t *testing.T) {
	for _, tc := range []struct {
		name string
		pane string
	}{
		{"streaming output", ampPaneStreaming},
		{"thinking, no output at all", ampPaneThinking},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			inst := registerStartedAmp(t, manager, repoID, repoPath, "amping", tc.pane)

			// Poll repeatedly: the flash is an OSCILLATION, so one tick proves
			// nothing. Every tick over a still working pane must hold Running.
			for tick := 1; tick <= 3; tick++ {
				manager.RefreshStatuses()
				if got := inst.GetLiveness(); got != session.LiveRunning {
					t.Fatalf("tick %d: liveness = %v, want LiveRunning — amp is mid-turn, so the status dot must stay dark (a green dot means waiting for input, #1766)", tick, got)
				}
			}
		})
	}
}

// TestRefreshStatuses_AmpIdleBecomesReady is the other half of the contract: the
// fix must not buy a flash-free dot by never going green. Once the turn finishes
// the status segment disappears from the frame, and that genuinely-idle pane —
// still (updated=false) for the same reason a working one is — must settle Ready
// and persist, or an amp session would never report done to the user, to the
// sidebar dot, or to `af sessions watch`.
func TestRefreshStatuses_AmpIdleBecomesReady(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerStartedAmp(t, manager, repoID, repoPath, "amping", ampPaneIdle)

	manager.RefreshStatuses()

	if got := inst.GetLiveness(); got != session.LiveReady {
		t.Fatalf("in-memory liveness = %v, want LiveReady (a finished amp turn must go green)", got)
	}
	if got := persistedLiveness(t, repoID, "amping"); got != session.LiveReady {
		t.Fatalf("persisted liveness = %v, want LiveReady (the transition must persist)", got)
	}
}

// TestRefreshStatuses_AmpWorkingThenIdleTransitions walks the real sequence a turn
// produces — working, then done — through one instance, which is what the user
// actually watches. It pins the whole point of the fix: the dot goes green ONCE,
// at the end, rather than blinking through the turn.
func TestRefreshStatuses_AmpWorkingThenIdleTransitions(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &stillPaneBackend{FakeBackend: session.NewFakeBackend(), content: ampPaneStreaming}
	inst, err := session.NewInstance(session.InstanceOptions{Title: "amping", Path: repoPath, Program: "amp"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetBackend(backend)
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	seedDiskInstance(t, repoID, "amping", repoPath)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, "amping")] = inst
	manager.mu.Unlock()

	manager.RefreshStatuses()
	if got := inst.GetLiveness(); got != session.LiveRunning {
		t.Fatalf("mid-turn liveness = %v, want LiveRunning", got)
	}

	// The turn completes: amp drops the status segment from the frame.
	backend.content = ampPaneIdle
	manager.RefreshStatuses()
	if got := inst.GetLiveness(); got != session.LiveReady {
		t.Fatalf("post-turn liveness = %v, want LiveReady — the dot must go green when amp is genuinely waiting", got)
	}
}

// TestRefreshStatuses_NonAmpStillPaneUnchanged pins the blast radius: the working
// check is additive and agent-scoped, so a claude session whose pane happens to
// contain amp-shaped box drawing still resolves Ready on a still tick exactly as
// it did before. Every non-amp agent keeps the pure pane-churn inference.
func TestRefreshStatuses_NonAmpStillPaneUnchanged(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := stillPaneBackend{FakeBackend: session.NewFakeBackend(), content: ampPaneStreaming}
	inst := registerStarted(t, manager, repoID, repoPath, "clauding", backend, true, session.Running)

	manager.RefreshStatuses()

	if got := inst.GetLiveness(); got != session.LiveReady {
		t.Fatalf("liveness = %v, want LiveReady — a non-amp session must keep the unchanged-pane-means-idle inference", got)
	}
}
