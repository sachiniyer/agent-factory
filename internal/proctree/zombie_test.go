package proctree

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// The subject of this file is #2103: a zombie (a process that has exited but
// whose parent has not collected its exit status) keeps its process-table
// entry, so every EXISTENCE check still finds it. proctree's liveness check
// was an existence check, so the reapers waited out their full budget for
// processes that had already exited.
//
// observedZombie is deliberately NOT the production check — it reads the
// kernel's state field directly, per platform (see the _linux/_darwin siblings),
// so these tests observe the zombie independently of the code they are testing.

// startZombie spawns a child, kills it, and deliberately does NOT reap it, so
// the kernel keeps its entry as a zombie for the rest of the test.
//
// The not-reaping is the whole point, and it has to be a REAL child: only a
// direct child of this process can be our zombie, and only an uncollected one
// stays in the table. A test that Wait()ed the child would prove nothing — the
// entry would be gone and every check would agree it was gone.
func startZombie(t *testing.T) Process {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting child: %v", err)
	}
	pid := cmd.Process.Pid
	// Collect it only at the very end, so the zombie does not outlive the test.
	t.Cleanup(func() { _, _ = cmd.Process.Wait() })

	snap, err := Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	p, ok := snap[pid]
	if !ok {
		t.Fatalf("child %d missing from snapshot", pid)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("killing child: %v", err)
	}
	// The kill is asynchronous: wait for the kernel to actually park the entry
	// in the zombie state before handing it to the code under test.
	deadline := time.Now().Add(2 * time.Second)
	for !observedZombie(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("child %d never became a zombie", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return p
}

// TestAliveSameZombie: a zombie has exited. It will never run again and it
// cannot be signalled into exiting — the only thing left of it is a table entry
// its parent has not collected. The liveness check must call it dead (#2103).
func TestAliveSameZombie(t *testing.T) {
	p := startZombie(t)
	if AliveSame(p) {
		t.Errorf("AliveSame(zombie pid %d) = true, want false: an exited process is not alive", p.PID)
	}
}

// TestWaitForExitsZombie is the user-visible half of #2103: because AliveSame
// counted zombies as alive, WaitForExits burned its ENTIRE budget waiting for a
// process that had already exited — 3s per worktree teardown, 6s per tmux one —
// and then KillEscalating logged "survived SIGKILL" about a corpse.
//
// The assertion is on elapsed time, not just on the result: the result alone
// cannot tell a prompt correct answer from a timed-out one.
func TestWaitForExitsZombie(t *testing.T) {
	p := startZombie(t)

	const timeout = 2 * time.Second
	start := time.Now()
	remaining := WaitForExits([]Process{p}, timeout)
	elapsed := time.Since(start)

	if len(remaining) != 0 {
		t.Errorf("WaitForExits reported %d survivor(s) for a zombie, want 0", len(remaining))
	}
	// A poll interval of slack; the bug spends the full timeout.
	if elapsed > 500*time.Millisecond {
		t.Errorf("WaitForExits took %v for an already-exited process (timeout %v); it must return promptly", elapsed, timeout)
	}
}

// TestSignalZombie: the kernel accepts a signal aimed at a zombie, so a caller
// that reads "delivered" as "it will now exit" waits for an exit that already
// happened. Signal must report the identity as gone instead — which is what
// KillEscalating already treats as success rather than a warnable failure.
func TestSignalZombie(t *testing.T) {
	p := startZombie(t)
	if err := Signal(p, syscall.SIGKILL); err != ErrIdentityChanged {
		t.Errorf("Signal(zombie) = %v, want ErrIdentityChanged", err)
	}
}

// TestSnapshotOmitsZombie covers how corpses got INTO the reapers' target sets
// in the first place. The tmux reaper selects by kernel session id
// (SessionMembers, session/tmux/reap.go) and a zombie keeps its session id — so
// before #2103 a corpse was picked as a kill target, signalled, and then waited
// on for the full budget.
//
// Dropping it from the table loses no descendant: the kernel reparents a
// process's children when it EXITS, not when it is collected, so a zombie never
// has any.
func TestSnapshotOmitsZombie(t *testing.T) {
	p := startZombie(t)

	snap, err := Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, ok := snap[p.PID]; ok {
		t.Errorf("Snapshot included zombie pid %d; reapers select kill targets from this table", p.PID)
	}
	if got := SessionMembers(snap, p.SID); len(got) != 0 {
		for _, m := range got {
			if m.PID == p.PID {
				t.Errorf("SessionMembers selected zombie pid %d as a reap target", p.PID)
			}
		}
	}
}

// TestKillEscalatingNoWarnOnZombie is the log half of #2103: a corpse must not
// be reported as a process that "survived SIGKILL". That line sends whoever
// reads it hunting for an unkillable process that does not exist.
func TestKillEscalatingNoWarnOnZombie(t *testing.T) {
	p := startZombie(t)

	var lines []string
	logf := func(format string, args ...any) {
		lines = append(lines, format)
	}
	start := time.Now()
	remaining := KillEscalating([]Process{p}, 2*time.Second, 2*time.Second, logf)
	elapsed := time.Since(start)

	if len(remaining) != 0 {
		t.Errorf("KillEscalating reported %d survivor(s) for a zombie, want 0", len(remaining))
	}
	if len(lines) != 0 {
		t.Errorf("KillEscalating logged %d line(s) about an already-exited process: %v", len(lines), lines)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("KillEscalating took %v for an already-exited process; it must return promptly", elapsed)
	}
}
