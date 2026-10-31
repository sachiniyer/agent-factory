package daemon

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// The TUI sends both the stable ID and the repo id. The ID must be resolved in the
// ID namespace, so a session whose title differs from its ID is still found, and a
// stale ID refuses instead of matching a title (#5796 review).
func TestWorkspaceFactsResolvesExplicitStableID(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	inst, err := session.FromInstanceData(session.InstanceData{
		ID: "sess-123", Title: "alpha", Path: repoPath, Status: session.Archived, BackendType: "local",
		Worktree: session.GitWorktreeData{RepoPath: repoPath, WorktreePath: repoPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, "alpha")] = inst
	m.mu.Unlock()

	_, got, err := m.agentServerForStreamTarget(workspaceFactsTarget(WorkspaceFactsRequest{ID: "sess-123", Title: "alpha", RepoID: repoID}))
	if err != nil || got != inst {
		t.Fatalf("explicit ID with a repo scope must resolve the session: got %v, err %v", got, err)
	}
	if _, _, err := m.agentServerForStreamTarget(workspaceFactsTarget(WorkspaceFactsRequest{ID: "stale", Title: "alpha", RepoID: repoID})); err == nil {
		t.Fatal("a stale ID must refuse, not fall back to the title")
	}
}

// TestStreamTargetRefusesNonLiveForAttach pins the bug fix for "CLI: Attaching to
// archived sessions fails with opaque WebSocket 500 instead of restore guidance".
// agentServerForAttachTarget is the daemon entry point the /stream and
// /stream-info routes resolve through. It must refuse a non-live target with the same actionable off-ramp
// SendPrompt's promptTargetLivenessError returns, rather than handing the instance
// through to a WS upgrade that then fails deep at broker subscribe with a generic
// 500. The gate is attach-only: the workspace-facts resolver (the kill-confirm
// measurement) must keep resolving the same rows, because a remote row can be
// LiveLost/LiveDead while its endpoint has recovered, and the kill confirmation
// still needs to measure its unpushed work.
func TestStreamTargetRefusesNonLiveForAttach(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	cases := []struct {
		name     string
		liveness session.Liveness
		wantSub  string
	}{
		// Archived (#1529): no live tmux to stream — the restore off-ramp, mirroring
		// the TUI's interactiveGuard. The exact message is the one promptTargetLivenessError returns.
		{"archived", session.LiveArchived, `is Archived; prompt not delivered; restore it first (af sessions restore -- alpha)`},
		{"lost", session.LiveLost, `is Lost; prompt not delivered; recover it first`},
		{"dead", session.LiveDead, `is Dead; prompt not delivered; recover it first`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inst, _ := newCountingInstance(t, "alpha", repoPath)
			inst.ID = "id-" + tc.name
			if err := inst.Transition(session.ObserveLiveness(tc.liveness)); err != nil {
				t.Fatalf("transition to %v: %v", tc.liveness, err)
			}
			m.mu.Lock()
			m.instances[daemonInstanceKey(repoID, "alpha")] = inst
			m.mu.Unlock()

			as, got, err := m.agentServerForAttachTarget(authoritativeStreamTarget("alpha", repoID))
			if err == nil {
				t.Fatalf("agentServerForAttachTarget admitted a %s session (as=%v instance=%v); it must refuse before the WS upgrade", tc.name, as, got)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("%s refusal message = %q\nwant it to contain %q", tc.name, err.Error(), tc.wantSub)
			}

			// The workspace-facts resolver is NOT an attach path: it must still
			// resolve the row so the kill confirmation can measure its workspace.
			if _, got, werr := m.agentServerForStreamTarget(workspaceFactsTarget(WorkspaceFactsRequest{ID: inst.ID, Title: "alpha", RepoID: repoID})); werr != nil || got != inst {
				t.Fatalf("WorkspaceFacts resolver must resolve a %s session (liveness gating is attach-only): instance=%v err=%v", tc.name, got, werr)
			}
		})
	}
}

// TestStreamTargetAdmitsLiveForAttach is the non-regression half of the gate: a
// Running session — the case attach exists for — still resolves and returns its
// agent-server, so the liveness refusal did not widen into a live-session refusal.
func TestStreamTargetAdmitsLiveForAttach(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	inst, _ := newCountingInstance(t, "live-attach", repoPath)
	inst.ID = "id-live-attach"
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, "live-attach")] = inst
	m.mu.Unlock()

	as, got, err := m.agentServerForAttachTarget(authoritativeStreamTarget("live-attach", repoID))
	if err != nil || got != inst || as == nil {
		t.Fatalf("a Running session must resolve to its agent-server: as=%v instance=%v err=%v", as, got, err)
	}
}
