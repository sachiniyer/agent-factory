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
	// A live instance: agentServerForStreamTarget now refuses a non-live target
	// (the liveness gate the stream path shares with WorkspaceFacts), so this ID
	// resolution test seeds a Running instance the gate admits. The archived row
	// the stream path must refuse is pinned separately (TestStreamTargetRefuses...
	// below), since the refusal is the stream bug fix's contract, not this one's.
	inst, _ := newCountingInstance(t, "alpha", repoPath)
	inst.ID = "sess-123"
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
// agentServerForStreamTarget is the daemon entry point the /stream and
// /stream-info routes resolve through, and it is the only attach path that
// exists. It must refuse a non-live target with the same actionable off-ramp
// SendPrompt's promptTargetLivenessError returns, rather than handing the instance
// through to a WS upgrade that then fails deep at broker subscribe with a generic
// 500. WorkspaceFacts shares the resolver, so the same gate now also answers a
// kill-confirmation probe for an archived session with the refusal — the TUI's kill
// confirm never asks for an archived session's workspace (app/handle_actions.go
// skips measurement when IsArchived), so no production caller regresses.
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

			as, got, err := m.agentServerForStreamTarget(authoritativeStreamTarget("alpha", repoID))
			if err == nil {
				t.Fatalf("agentServerForStreamTarget admitted a %s session (as=%v instance=%v); it must refuse before the WS upgrade", tc.name, as, got)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("%s refusal message = %q\nwant it to contain %q", tc.name, err.Error(), tc.wantSub)
			}

			// The same gate answers the workspace-facts resolver (the kill-confirm
			// probe): an archived target must not be handed through as measurable.
			if _, _, werr := m.agentServerForStreamTarget(workspaceFactsTarget(WorkspaceFactsRequest{ID: inst.ID, Title: "alpha", RepoID: repoID})); werr == nil {
				t.Fatalf("WorkspaceFacts resolver admitted a %s session; it shares the gate and must refuse", tc.name)
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

	as, got, err := m.agentServerForStreamTarget(authoritativeStreamTarget("live-attach", repoID))
	if err != nil || got != inst || as == nil {
		t.Fatalf("a Running session must resolve to its agent-server: as=%v instance=%v err=%v", as, got, err)
	}
}
