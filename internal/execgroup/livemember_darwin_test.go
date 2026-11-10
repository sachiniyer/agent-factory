//go:build darwin

package execgroup

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// groupHasLiveMember must count a running member as live, and must not count
// the zombie leader that os/exec has not reaped yet. kern.proc.pgrp does list
// that zombie, and it stays listed until the caller's Wait reaps it, which
// may be long after Cancel ran. Were it counted, killGroup would re-signal
// until its bound on every such Cancel.
func TestGroupHasLiveMemberIgnoresTheZombieLeader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sleep", holderLife)
	Lead(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pgid := cmd.Process.Pid
	defer func() { _ = cmd.Wait() }()

	live, err := groupHasLiveMember(pgid)
	if err != nil || !live {
		killOwnGroup(t, pgid)
		t.Fatalf("groupHasLiveMember on a running group = %v, %v; want true, nil", live, err)
	}

	// Kill the group without Wait, so the leader stays an unreaped zombie
	// in its group.
	killOwnGroup(t, pgid)
	deadline := time.Now().Add(killBound)
	for {
		kps, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
		if err != nil {
			t.Fatalf("kern.proc.pgrp: %v", err)
		}
		if len(kps) == 1 && kps[0].Proc.P_stat == szomb {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the killed leader %d did not show as the group's one zombie within %v (%d entries)", pgid, killBound, len(kps))
		}
		time.Sleep(time.Millisecond)
	}
	live, err = groupHasLiveMember(pgid)
	if err != nil || live {
		t.Fatalf("groupHasLiveMember on a group holding only its zombie leader = %v, %v; want false, nil", live, err)
	}

	// Once the leader is reaped the group is gone: no live member, no error.
	_ = cmd.Wait()
	live, err = groupHasLiveMember(pgid)
	if err != nil || live {
		t.Fatalf("groupHasLiveMember on a reaped group = %v, %v; want false, nil", live, err)
	}
}
