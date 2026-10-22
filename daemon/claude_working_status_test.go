package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// Byte-level fidelity to a live Claude Code pane is pinned against real
// captures in task.TestIsWorkingContentClaudeRealCapture; these panes exercise
// which liveness the poll's still-pane branch settles on.
const (
	claudePaneWorking = "✻ Orbiting… (2m 14s · ↓ 10.1k tokens)\n" +
		"────────────────────────────────────────\n" +
		"❯ \n" +
		"────────────────────────────────────────\n" +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle) · esc to interrupt · ← 2 agents\n"
	claudePaneIdle = "✻ Baked for 10m 46s · done 10:54 PM\n" +
		"────────────────────────────────────────\n" +
		"❯ \n" +
		"────────────────────────────────────────\n" +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← 2 agents\n"
)

// The limit check runs before the working check, so a usage-limit banner wins
// even if the working chrome is still on screen.
func TestRefreshStatuses_ClaudeUsageLimitBeatsWorkingChrome(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	pane := "  ⎿  " + claudeLimitBanner + ".\n" + claudePaneWorking
	backend := &stillPaneBackend{FakeBackend: session.NewFakeBackend(), content: pane}
	inst := registerStarted(t, manager, repoID, repoPath, "clauding", backend, true, session.Running)

	manager.RefreshStatuses()
	if got := inst.GetLiveness(); got != session.LiveLimitReached {
		t.Fatalf("liveness = %v, want LiveLimitReached", got)
	}
}

func TestRefreshStatuses_ClaudeWorkingBehindStillPaneStaysRunning(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &stillPaneBackend{FakeBackend: session.NewFakeBackend(), content: claudePaneWorking}
	inst := registerStarted(t, manager, repoID, repoPath, "clauding", backend, true, session.Running)

	for tick := 1; tick <= 3; tick++ {
		manager.RefreshStatuses()
		if got := inst.GetLiveness(); got != session.LiveRunning {
			t.Fatalf("tick %d: liveness = %v, want LiveRunning — claude is mid-turn, so `af sessions watch` must not return yet", tick, got)
		}
	}

	backend.content = claudePaneIdle
	manager.RefreshStatuses()
	if got := inst.GetLiveness(); got != session.LiveReady {
		t.Fatalf("post-turn liveness = %v, want LiveReady", got)
	}
	if got := persistedLiveness(t, repoID, "clauding"); got != session.LiveReady {
		t.Fatalf("persisted liveness = %v, want LiveReady", got)
	}
}
