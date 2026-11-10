package execgroup

import (
	"syscall"
	"time"
)

// This file is the sweep killGroup runs after its first kill wherever
// readGroup is defined (darwin). It carries no build tag, so every platform's
// CI tests its logic through an injected read and kill.

// resignalBound caps, in wall-clock time, how long killGroup waits for the
// sweep. A SIGKILLed process is normally a zombie within milliseconds, so
// the bound matters only when something does not end: a member stuck in an
// uninterruptible wait, or a membership read that blocks (see sweep).
// Without it, Cancel would hold os/exec's Wait for as long as that lasts,
// and WaitDelay's timer does not start until Cancel returns.
const resignalBound = 2 * time.Second

// The pause after each re-signal starts at resignalPollMin and doubles up to
// resignalPollMax. A normal sweep ends within the first few pauses, and a
// stuck one reads the group a few dozen times rather than thousands.
const (
	resignalPollMin = time.Millisecond
	resignalPollMax = 50 * time.Millisecond
)

// groupMember is one process that a membership read found in the group.
type groupMember struct {
	pid    int
	zombie bool
}

// groupReader lists the processes in the group that pgid leads, zombies
// included.
type groupReader func(pgid int) ([]groupMember, error)

// signaller sends a signal the way syscall.Kill does.
type signaller func(pid int, sig syscall.Signal) error

// sweep runs resignal on its own goroutine and waits for it for at most
// bound, so killGroup returns within bound whatever a read does.
//
// A read can block. XNU's kern.proc.pgrp waits, with no timeout and no
// interruption, for any member that is part-way through an exec
// (proc_iterate calls proc_transwait). An exec reading its image from a hung
// network or FUSE mount can therefore hold the read indefinitely. When that
// happens, the goroutine stays parked in the read until the exec finishes,
// and then returns without signalling, because by then its deadline has
// passed. That costs one parked goroutine per stuck group, where waiting
// would cost a hung Cancel and Wait.
func sweep(pgid int, read groupReader, kill signaller, bound time.Duration) {
	deadline := time.Now().Add(bound)
	done := make(chan struct{})
	go func() {
		defer close(done)
		resignal(pgid, read, kill, deadline)
	}()
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

// resignal re-sends SIGKILL to the group pgid leads until two consecutive
// reads prove that no member is alive and none can still be forking.
//
// One read is not that proof. kern.proc.pgrp is not a snapshot: XNU lists
// the group's pids under a lock, skipping a process that is still being
// created, and only afterwards reads each listed member's state. One read
// can list a parent P that is forking, miss its child C, and then read P
// after P has finished the fork and died. That read finds no live member
// while C, which no signal ever reached, lives on.
//
// So the loop stops only when a read finds no live member and lists no pid
// that the read before it did not, and that earlier read also found no live
// member. The second read follows the first at once, with no pause and no
// signal in between. Suppose a member X were alive after the second read.
// That read saw every process it listed dead, so it did not list X: X was
// still being created when it took its list, or was created later. Follow
// X's forks back to the first process in that line that was alive and fully
// created at that moment. The second read listed that process and saw it
// dead. Since the second read lists no new pid, the first read listed it as
// well, and saw it dead before the second read began. A process dead then
// was not alive at that later moment, so no such X exists.
//
// Each re-signal can reach a group that is not ours. A read cannot tell our
// group N from a later group that reused N, so a reuse anywhere between one
// read and the next kill goes unseen. That stretch is one pause (up to
// resignalPollMax) plus two reads. One reuse is detectable, and resignal
// stops on it: the leader's pid listed alive after an earlier read showed it
// dead or gone. Our leader cannot come back, so that process is a new one. A
// reused group whose own leader is already gone still goes unseen. Reaching
// it takes XNU's sequential pid allocator wrapping round to this exact
// number, and the new process leading its own group, inside that stretch.
// Reap's single kill after Wait has always carried the same hazard. darwin
// has no handle that pins a group, so closing it would take a different
// mechanism.
//
// A failed read ends the loop, and so does the deadline, which is checked
// after every read: no signal goes out once it has passed, however long the
// read took. In both cases the first kill has already been sent.
func resignal(pgid int, read groupReader, kill signaller, deadline time.Time) {
	pause := resignalPollMin
	var prev map[int]bool // pids the previous read listed, if it found no live member
	leaderGone := false
	for {
		members, err := read(pgid)
		if err != nil || !time.Now().Before(deadline) {
			return
		}
		live, leaderLive := false, false
		pids := make(map[int]bool, len(members))
		for _, m := range members {
			pids[m.pid] = true
			if !m.zombie {
				live = true
				if m.pid == pgid {
					leaderLive = true
				}
			}
		}
		if leaderLive && leaderGone {
			return // pid reused: this group is not ours
		}
		if !leaderLive {
			leaderGone = true
		}
		if !live {
			if prev != nil && subset(pids, prev) {
				return
			}
			prev = pids
			continue // the confirming read, at once
		}
		prev = nil
		_ = kill(-pgid, syscall.SIGKILL)
		time.Sleep(pause)
		pause = min(2*pause, resignalPollMax)
	}
}

// subset reports whether every pid in a is also in b.
func subset(a, b map[int]bool) bool {
	for pid := range a {
		if !b[pid] {
			return false
		}
	}
	return true
}
