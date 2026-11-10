package execgroup

import (
	"errors"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// This file is what killGroup does after its first kill wherever readGroup
// is defined (darwin): sweep the group, and decide what an EPERM from that
// kill means. It carries no build tag, so every platform's CI tests its
// logic through an injected read and kill.

// resignalBound caps, in wall-clock time, how long killGroup spends after
// its first kill. A SIGKILLed process is normally a zombie within
// milliseconds, so the bound matters only when something does not end: a
// member stuck in an uninterruptible wait, or a membership read that blocks
// (see sweep). Without it, Cancel would hold os/exec's Wait for as long as
// that lasts, and WaitDelay's timer does not start until Cancel returns.
const resignalBound = 2 * time.Second

// The pause after each re-signal starts at resignalPollMin and doubles up to
// resignalPollMax. A normal sweep ends within the first few pauses, and a
// stuck one reads the group a few dozen times rather than thousands.
const (
	resignalPollMin = time.Millisecond
	resignalPollMax = 50 * time.Millisecond
)

// groupMember is one process that a membership read found in the group.
// exiting is set from the moment the process starts to exit (XNU's P_LEXIT,
// reported as P_WEXIT), so it is also set on a zombie.
type groupMember struct {
	pid     int
	zombie  bool
	exiting bool
}

// groupReader lists the processes in the group that pgid leads, zombies
// included.
type groupReader func(pgid int) ([]groupMember, error)

// signaller sends a signal the way syscall.Kill does.
type signaller func(pid int, sig syscall.Signal) error

// afterEPERM decides what an EPERM from killGroup's first kill means where
// readGroup is defined (darwin). killpg answers EPERM there both when a
// member refused the signal and when it found no member it could signal at
// all, because every member was a zombie or already exiting (#7072).
//
// One read of the group tells those apart. A member that is neither a
// zombie nor exiting either refused the signal (a setuid child, say) or was
// created after the kill, which killpg skips while a child is still being
// created. A second kill decides between the two. It succeeds on a new
// child, and the sweep then runs as after any kill that landed. EPERM again
// is a real refusal, and afterEPERM returns it. ESRCH means the group is
// gone. A group of only zombies and exiting members had nobody to signal,
// so there is nothing left to kill. afterEPERM sweeps it anyway, which waits
// out the exiting members and catches anything one of them was still
// forking, and returns os.ErrProcessDone.
//
// The exiting flag can be trusted for this. XNU sets P_LEXIT, which
// kern.proc reports as P_WEXIT, before P_REF_DEAD, the mark that makes
// proc_find refuse a process, and never clears it. So every member that
// killpg skipped for exiting reads as exiting, or is left out of the read
// entirely.
//
// A read that fails, or is still blocked at the deadline, proves nothing,
// and the EPERM stands.
func afterEPERM(pgid int, kill signaller, read groupReader, deadline time.Time, eperm error) error {
	members, err := readBy(pgid, read, deadline)
	if err != nil {
		return eperm
	}
	for _, m := range members {
		if m.zombie || m.exiting {
			continue
		}
		switch err := kill(-pgid, syscall.SIGKILL); {
		case err == nil:
			sweep(pgid, read, kill, deadline)
			return nil
		case errors.Is(err, syscall.ESRCH):
			return os.ErrProcessDone
		default:
			return err
		}
	}
	sweep(pgid, read, kill, deadline)
	return os.ErrProcessDone
}

// errReadTimedOut is readBy's answer for a read still blocked at its
// deadline.
var errReadTimedOut = errors.New("execgroup: the membership read was still blocked at its deadline")

// readBy reads the group on its own goroutine and waits for the answer
// until deadline, for the reason sweep gives: a read can block.
func readBy(pgid int, read groupReader, deadline time.Time) ([]groupMember, error) {
	type answer struct {
		members []groupMember
		err     error
	}
	done := make(chan answer, 1)
	go func() {
		members, err := read(pgid)
		if err != nil {
			noteReadFailure(pgid, err)
		}
		done <- answer{members, err}
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case a := <-done:
		return a.members, a.err
	case <-timer.C:
		return nil, errReadTimedOut
	}
}

// sweep runs resignal on its own goroutine and waits for it until deadline,
// so killGroup returns by then whatever a read does.
//
// A read can block. XNU's kern.proc.pgrp waits, with no timeout and no
// interruption, for any member that is part-way through an exec
// (proc_iterate calls proc_transwait). An exec reading its image from a hung
// network or FUSE mount can therefore hold the read indefinitely. When that
// happens, the goroutine stays parked in the read until the exec finishes,
// and then returns without signalling, because by then its deadline has
// passed. That costs one parked goroutine per stuck group, where waiting
// would cost a hung Cancel and Wait.
func sweep(pgid int, read groupReader, kill signaller, deadline time.Time) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		resignal(pgid, read, kill, deadline)
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

// resignal re-sends SIGKILL to the group pgid leads until two consecutive
// reads find no live member. That rule narrows the fork race. It does not
// rule it out.
//
// One quiet read is plainly not enough. kern.proc.pgrp is not a snapshot:
// XNU lists the group's pids under a lock, skipping a process still being
// created, and only afterwards reads each listed member's state. One read
// can list a parent P that is forking, miss its child C, and then read P
// after P has finished the fork and died. That read finds no live member
// while C, which no signal ever reached, lives on.
//
// So the loop stops only when a read finds no live member and lists no pid
// that the read before it did not, and that earlier read also found no live
// member. The second read follows the first at once, with no pause and no
// signal in between, so an escaped C is normally listed, alive, by the
// second. Normally, not always. A pid reaped between a read's locked listing
// and its state read is silently left out of that read's report. An orphan
// is reaped inside its own exit, so it can go from alive to absent without
// ever showing as a zombie. So an escaped orphan that forks and exits at
// once (a double fork, "( holder & )"), with the reading thread stalled at
// the wrong moment in both reads, leaves both reports quiet while its
// unsignalled child holds the pipe. More quiet reads only lengthen the fork
// chain that takes, and never close it. Closing it takes a locked pid
// listing (proc_listpids) reported apart from the state reads, so that a
// pid reaped in between shows as gone rather than vanishing (#7072).
//
// Measured on macos-15 (#7069): before this sweep, one group kill missed a
// mid-fork child in about half the trials that had a fork in flight. With
// this rule, 0 of 300 forkingGroup trials did. A rule that stopped on a
// single quiet read held 0 of 525 across two probe runs, so even the stale
// read above, plain in XNU's source, is too rare to have shown up there.
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
		if err != nil {
			noteReadFailure(pgid, err)
			return
		}
		if !time.Now().Before(deadline) {
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
		pause = nextPause(pause)
	}
}

// nextPause doubles a pause, up to resignalPollMax.
func nextPause(pause time.Duration) time.Duration {
	return min(2*pause, resignalPollMax)
}

// readFailureNoted makes noteReadFailure log once per process.
var readFailureNoted sync.Once

// noteReadFailure logs the first membership read that fails. Without the
// read, killGroup has only its first kill to go on: the sweep stops, and an
// EPERM stands. Neither shows anywhere else.
func noteReadFailure(pgid int, err error) {
	readFailureNoted.Do(func() {
		log.WarningLog.Printf("execgroup: reading process group %d failed, so its kill went ahead without the follow-up that read drives (logged once): %v", pgid, err)
	})
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
