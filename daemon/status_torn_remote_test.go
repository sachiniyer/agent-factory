package daemon

import (
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// delegatingRemoteBackend reproduces the load-bearing shape of the REAL sandbox
// backends (docker/ssh/hook) that the existing remoteWorkspaceBackend does not:
// its HasUpdated delegates STRAIGHT BACK to inst.AgentServer().Snapshot(), exactly
// as remoteAgentBackend does. That delegation is the #2005 hazard — paired with a
// torn-down runtime (remoteClient nil) whose AgentServer() used to fall back to a
// localAgentServer, it made the poll recurse localAgentServer→backend→AgentServer→…
// until the daemon overflowed its stack. A depth cap stands in for the kernel stack
// limit so the pre-fix recursion is a bounded, assertable spike instead of a crash.
type delegatingRemoteBackend struct {
	*session.FakeBackend
	mu       sync.Mutex
	depth    int
	maxDepth int
}

func (b *delegatingRemoteBackend) Type() string { return "docker" }

func (b *delegatingRemoteBackend) Capabilities() session.Capabilities {
	return session.Capabilities{
		Workspace:        session.WorkspaceRemote,
		Archive:          true,
		Recover:          true,
		InteractiveInput: true,
	}
}

func (b *delegatingRemoteBackend) HasUpdated(inst *session.Instance) (bool, bool, string) {
	b.mu.Lock()
	b.depth++
	over := b.depth > b.maxDepth
	b.mu.Unlock()
	if over {
		return false, false, ""
	}
	obs, _ := inst.AgentServer().Snapshot()
	return obs.Updated, obs.HasPrompt, obs.Content
}

func (b *delegatingRemoteBackend) reentries() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.depth
}

// TestRefreshInstanceStatus_TornDownRemoteRuntime_DoesNotRecurse is the daemon-side
// #2005 regression. A remote sandbox session that went Lost is left, after a FAILED
// lost-recovery, with started=true + Lost + a WorkspaceRemote backend still bound
// but its remoteClient torn down (nil). A poll tick over that instance must run to
// completion — probe, settle, return — and leave the session in a definite Lost
// state, NOT recurse into a localAgentServer that delegates back through the remote
// backend forever and overflows the daemon's stack.
func TestRefreshInstanceStatus_TornDownRemoteRuntime_DoesNotRecurse(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	// No RemoteAgentServer endpoint => remoteClient stays nil, the torn-down shape.
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "torn-remote",
		Path:    repoPath,
		Program: "claude",
	})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	backend := &delegatingRemoteBackend{FakeBackend: session.NewFakeBackend(), maxDepth: 64}
	inst.SetBackend(backend)
	inst.SetStartedForTest(true)        // a running session that went Lost keeps started=true
	inst.SetStatusForTest(session.Lost) // Lost + started is what the poll and restore loop touch
	seedDiskInstance(t, repoID, "torn-remote", repoPath)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, "torn-remote")] = inst
	manager.mu.Unlock()

	// One poll tick. Pre-fix this recursed until the stack overflowed (the depth cap
	// turns that into a bounded spike); post-fix AgentServer() returns the dead
	// server, whose Snapshot errors without ever touching the backend.
	manager.refreshInstanceStatus(repoID, inst)

	if got := backend.reentries(); got > 1 {
		t.Fatalf("poll recursed through the remote backend %d times (#2005); it must not re-enter it at all", got)
	}
	if got := inst.GetLiveness(); got != session.LiveLost {
		t.Fatalf("poll left the session at %v; a torn-down remote session must resolve to a definite Lost", got)
	}
	if !inst.Started() {
		t.Fatalf("poll cleared started; the session must stay started so the #1128 restore loop keeps retrying")
	}
}
