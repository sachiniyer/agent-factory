//go:build darwin

package execgroup

import (
	"context"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

// readGroup must report a running leader as live and not exiting, the same
// leader as a zombie once it is killed but not yet waited (still flagged
// exiting: XNU never clears P_LEXIT), and nothing once Wait has reaped it.
// The zombie row matters. kern.proc.pgrp keeps a zombie in its group until
// its parent reaps it, which may be long after Cancel ran. If that row read
// as live, every such sweep would run to its bound.
func TestReadGroupSeesTheLeaderLiveThenZombieThenGone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sleep", holderLife)
	Lead(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pgid := cmd.Process.Pid
	waited := false
	defer func() {
		// Unwaited, the leader's pid still names this test's group.
		if !waited {
			killOwnGroup(t, pgid)
			_ = cmd.Wait()
		}
	}()

	members, err := readGroup(pgid)
	if want := []groupMember{live(pgid)}; err != nil || !reflect.DeepEqual(members, want) {
		t.Fatalf("readGroup on a running group = %v, %v; want %v, nil", members, err, want)
	}

	killOwnGroup(t, pgid)
	want := []groupMember{zombie(pgid)}
	deadline := time.Now().Add(killBound)
	for {
		members, err = readGroup(pgid)
		if err != nil {
			t.Fatalf("readGroup: %v", err)
		}
		if reflect.DeepEqual(members, want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("readGroup after killing the leader = %v; want %v within %v", members, want, killBound)
		}
		time.Sleep(time.Millisecond)
	}

	_ = cmd.Wait()
	waited = true
	members, err = readGroup(pgid)
	if err != nil || len(members) != 0 {
		t.Fatalf("readGroup on a reaped group = %v, %v; want none, nil", members, err)
	}
}
