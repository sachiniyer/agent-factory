package daemon

import (
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// vscodeSupervisor.failures is keyed by daemonInstanceKey — a TITLE slot, not a
// session — so its spawn cooldown is the third instance of the #2868/#2876
// class: state that describes one runtime, filed under a name that outlives it.
//
// The cooldown is only ~5s and self-heals, which is exactly why it survived the
// first two fixes. It still hands a brand-new session an error it never earned:
// the fresh session's very first editor request replays the dead predecessor's
// errVSCodeStartExited instead of getting a spawn attempt.
//
// Both halves are pinned here because the guard has a read side and a write side,
// and fixing only the read side leaves the same bug reachable — which is what
// Codex caught on this PR's first fix attempt.

// TestVSCodeSupervisor_CooldownDoesNotCrossSessions is the read side: a failure
// recorded for session A must not gate session B's first spawn under the same
// title.
func TestVSCodeSupervisor_CooldownDoesNotCrossSessions(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", map[string]string{fakeVSCodeHangEnv: "1"})
	v := newTestVSCodeSupervisor(t, binary)
	v.startGrace = 100 * time.Millisecond
	v.cooldown = time.Hour // a replay inside this window is the bug
	worktree := t.TempDir()

	// Session A's editor starts, never becomes ready, and dies — the exact shape
	// that arms the cooldown.
	if _, err := v.ensureServerForInstance("k", "session-A", worktree); !errors.Is(err, errVSCodeStarting) {
		t.Fatalf("first ensure err = %v, want errVSCodeStarting", err)
	}
	serverA := v.servers["k"]
	if serverA == nil {
		t.Fatal("precondition: the still-starting editor was not registered")
	}
	_ = syscall.Kill(-serverA.cmd.Process.Pid, syscall.SIGKILL)
	<-serverA.exited

	// A asks again and is correctly gated: the cooldown works for the session that
	// earned it. This is the control — without it a fix that simply disabled the
	// cooldown would pass the real assertion below.
	if _, err := v.ensureServerForInstance("k", "session-A", worktree); !errors.Is(err, errVSCodeStartExited) {
		t.Fatalf("session A's own retry err = %v, want the recorded errVSCodeStartExited: the cooldown "+
			"must still gate the session whose editor died", err)
	}

	// Session B — the user killed A and made a new session with the same title —
	// asks for the first time. It must get a SPAWN, not A's error.
	_, err := v.ensureServerForInstance("k", "session-B", worktree)
	if errors.Is(err, errVSCodeStartExited) {
		t.Fatal("a NEW session was handed the predecessor's spawn failure on its first editor " +
			"request: the cooldown is filed under the reused TITLE rather than the stable instance id, " +
			"so it crossed sessions (#2868 class)")
	}
	if !errors.Is(err, errVSCodeStarting) {
		t.Fatalf("session B's first ensure err = %v, want errVSCodeStarting from its own spawn", err)
	}
	if got := v.servers["k"]; got == nil || got.instanceID != "session-B" {
		t.Fatalf("registered editor = %+v, want one owned by session-B", got)
	}
}

// TestVSCodeSupervisor_DeadEditorCooldownIsAttributedToItsOwner is the write
// side, and the subtler half. reconcilePersistedBeforeSpawn records the cooldown
// for a CACHED server it finds dead — and that server can belong to a different
// session than the caller, because the cache is keyed by title. Recording it
// against the caller re-enters the very bug the identity guard prevents, one
// layer up: B asks first, B's own request arms a cooldown in B's name, and B is
// refused on the spot.
func TestVSCodeSupervisor_DeadEditorCooldownIsAttributedToItsOwner(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", map[string]string{fakeVSCodeHangEnv: "1"})
	v := newTestVSCodeSupervisor(t, binary)
	v.startGrace = 100 * time.Millisecond
	v.cooldown = time.Hour
	worktree := t.TempDir()

	if _, err := v.ensureServerForInstance("k", "session-A", worktree); !errors.Is(err, errVSCodeStarting) {
		t.Fatalf("first ensure err = %v, want errVSCodeStarting", err)
	}
	serverA := v.servers["k"]
	if serverA == nil {
		t.Fatal("precondition: the still-starting editor was not registered")
	}
	_ = syscall.Kill(-serverA.cmd.Process.Pid, syscall.SIGKILL)
	<-serverA.exited

	// B is the FIRST to observe A's corpse, so B's own call is what records the
	// cooldown. Attributed to B, that entry then matches B's replay check inside
	// the same call chain.
	if _, err := v.ensureServerForInstance("k", "session-B", worktree); errors.Is(err, errVSCodeStartExited) {
		t.Fatal("the caller that merely DISCOVERED a dead predecessor editor was charged for its " +
			"death: the cooldown must be attributed to the server's own stable id, not to whoever " +
			"asked next")
	}

	v.mu.Lock()
	f, recorded := v.failures["k"]
	v.mu.Unlock()
	if recorded && f.instanceID == "session-B" {
		t.Fatal("the dead editor's cooldown was filed under session-B, which never owned it; a " +
			"later session-B retry would then replay a failure it did not earn")
	}
}

// TestVSCodeSupervisor_KeyIsATitleSlot documents WHY the two tests above are
// reachable at all, so a future reader does not conclude the guards are
// theoretical: the supervisor's map key is the repo/title daemon key, which two
// different sessions genuinely share when a title is reused.
func TestVSCodeSupervisor_KeyIsATitleSlot(t *testing.T) {
	first, err := session.NewInstance(session.InstanceOptions{Title: "recycled", Path: t.TempDir(), Program: "claude"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	second, err := session.NewInstance(session.InstanceOptions{Title: "recycled", Path: t.TempDir(), Program: "claude"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	if first.ID == second.ID {
		t.Fatal("two sessions must not share a stable id")
	}
	if daemonInstanceKey("repo", first.Title) != daemonInstanceKey("repo", second.Title) {
		t.Fatal("two same-titled sessions must share the daemon key — that is what makes the " +
			"identity guards on vscodeSupervisor.failures load-bearing rather than decorative")
	}
}
