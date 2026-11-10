package execgroup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestLeadOwnsItsGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "true")
	Lead(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("Lead must start the command in its own process group")
	}
	if cmd.Cancel == nil {
		t.Fatal("Lead must install the group-kill Cancel")
	}
}

// A group already gone maps to os.ErrProcessDone — the error Wait swallows —
// rather than surfacing as a command failure.
func TestCancelMapsGoneGroupToProcessDone(t *testing.T) {
	cmd := exec.Command("true")
	KillOnCancel(cmd)
	// A pid larger than any platform's maximum names a group that cannot
	// exist, so the signal answers ESRCH deterministically.
	proc, err := os.FindProcess(1 << 23)
	if err != nil {
		t.Fatalf("FindProcess: %v", err)
	}
	cmd.Process = proc
	if err := cmd.Cancel(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Cancel on a gone group = %v, want os.ErrProcessDone", err)
	}
}

// A command that never started has no group to kill: both halves are no-ops,
// not panics on a nil Process.
func TestNilProcessIsNoOp(t *testing.T) {
	cmd := exec.Command("true")
	KillOnCancel(cmd)
	if err := cmd.Cancel(); err != nil {
		t.Fatalf("Cancel with nil Process = %v, want nil", err)
	}
	Reap(cmd)
}

// The Cancel the contract installs actually reaches the whole group: after the
// context deadline fires, the leader's group is empty, not just the direct
// child dead.
func TestCancelKillsTheGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sleep", "60")
	Lead(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pgid := cmd.Process.Pid
	cancel()
	_ = cmd.Wait()
	if err := syscall.Kill(-pgid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("group led by %d still exists after cancel: %v", pgid, err)
	}
}
