package execgroup

import (
	"errors"
	"io/fs"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/log"
)

// holderScanner lists the processes that are members of the group pgid leads
// and hold a descriptor on one of ends.
type holderScanner func(pgid int, ends []proctree.PipeEnd) ([]proctree.Process, error)

// holderSignaller sends sig to p if, by a read answered before deadline, the
// pid still names that instance. It reports whether it sent.
type holderSignaller func(p proctree.Process, sig syscall.Signal, deadline time.Time) bool

// reapPipeHolders SIGKILLs every process that is BOTH a member of the group
// pgid leads and a holder of one of ends (#7121). It is the reap
// Output and CombinedOutput run on a pipe still open WaitDelay after the
// leader exited.
//
// Each process it signals is proven three ways, each by its own kernel read:
//   - it is in the group: listed by GroupMembers(pgid), which reads the
//     process's own pgrp, not its ancestry;
//   - it holds one of this command's pipe ends, still open past WaitDelay:
//     HoldsPipeEnd, against the pipe's identity taken before the child
//     started, while this process still holds its own end;
//   - it is the same instance when the signal goes: its start time is read
//     again first, and that read too must answer before the deadline.
//
// A member that holds none of the ends is never signalled, whatever it is. So
// is one whose descriptors cannot be read, such as a non-dumpable process on
// Linux: unread is not proven. Every process this signals, Reap's group kill
// would also have signalled. It is that kill narrowed to the proven
// stragglers.
//
// Every read that answers it fails closed. A group that cannot be read, a
// member whose descriptors cannot be read, an answer of the wrong shape, an
// identity check that fails or answers late: each means "not proven", so
// nothing is signalled on it, and nothing is guessed in its place.
//
// It scans, signals the holders it found, and scans again, until two scans in
// a row find none. The second scan is there for darwin, whose group read is
// not a snapshot: it can miss a child that a holder was forking (see
// resignal), and a missed child that inherited the pipe holds it too. darwin's
// kern.proc and proc_info reads can also block, uninterruptibly, on a member
// part-way through an exec. So like killGroup's sweep it is bounded by
// resignalBound in wall-clock time, whatever a read does: it returns by then,
// and it sends nothing on an answer that arrived after it.
func reapPipeHolders(pgid int, ends []proctree.PipeEnd) {
	reapPipeHoldersWith(pgid, ends, holderScan, signalIfSame, time.Now().Add(resignalBound))
}

// holderScan is the scanner reapPipeHolders uses, a var so a test can watch
// what each scan searches for.
var holderScan holderScanner = scanPipeHolders

// reapPipeHoldersWith is reapPipeHolders with its scan, signal and deadline
// injected, so every platform's CI can test the loop.
func reapPipeHoldersWith(pgid int, ends []proctree.PipeEnd, scan holderScanner, signal holderSignaller, deadline time.Time) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		killPipeHolders(pgid, ends, scan, signal, deadline)
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func killPipeHolders(pgid int, ends []proctree.PipeEnd, scan holderScanner, signal holderSignaller, deadline time.Time) {
	pause := resignalPollMin
	quiet := 0
	for {
		holders, err := scan(pgid, ends)
		if err != nil {
			noteHolderScanFailure(pgid, err)
			return
		}
		if !time.Now().Before(deadline) {
			return
		}
		if len(holders) == 0 {
			quiet++
			if quiet == 2 {
				return
			}
			continue // the confirming scan, at once
		}
		quiet = 0
		for _, h := range holders {
			signal(h, syscall.SIGKILL, deadline)
		}
		time.Sleep(pause)
		pause = nextPause(pause)
	}
}

// scanPipeHolders is the production holderScanner.
func scanPipeHolders(pgid int, ends []proctree.PipeEnd) ([]proctree.Process, error) {
	members, err := proctree.GroupMembers(pgid)
	if err != nil {
		return nil, err
	}
	var holders []proctree.Process
	for _, m := range members {
		held, err := proctree.HoldsPipeEnd(m.PID, ends...)
		if err != nil {
			if !processGone(err) {
				noteHolderReadFailure(m.PID, err)
			}
			continue
		}
		if held {
			holders = append(holders, m)
		}
	}
	return holders, nil
}

// signalIfSame is the production holderSignaller: proctree.Signal's identity
// check and kill, with the check, a kern.proc read on darwin that can block,
// answered on its own goroutine and honoured only if it arrived before
// deadline. A check that fails, or says the pid now names someone else, sends
// nothing. proctree.Signal is not used directly because its check is
// unbounded, and a late answer would send the kill after the deadline.
func signalIfSame(p proctree.Process, sig syscall.Signal, deadline time.Time) bool {
	if p.PID <= 1 || p.PID == os.Getpid() {
		return false
	}
	same := make(chan bool, 1)
	go func() {
		ok, err := proctree.SameIdentity(p)
		same <- ok && err == nil
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case ok := <-same:
		if !ok || !time.Now().Before(deadline) {
			return false
		}
	case <-timer.C:
		return false
	}
	return proctree.Kill(p.PID, sig) == nil
}

// processGone reports whether a per-process read failed because the process
// is no longer there, as opposed to a read the kernel refused.
func processGone(err error) bool {
	return errors.Is(err, proctree.ErrProcessExited) ||
		errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, syscall.ESRCH)
}

var (
	holderScanFailureNoted sync.Once
	holderReadFailureNoted sync.Once
)

// noteHolderScanFailure logs, once per process, a group read that failed, so
// a straggler left running because nothing could be proven is said somewhere.
func noteHolderScanFailure(pgid int, err error) {
	holderScanFailureNoted.Do(func() {
		log.WarningLog.Printf("execgroup: reading process group %d failed, so no process holding the command's pipes past WaitDelay could be proven and none was signalled (logged once): %v", pgid, err)
	})
}

// noteHolderReadFailure logs, once per process, a member whose descriptors
// could not be read. It is left running, because unread is not proven.
func noteHolderReadFailure(pid int, err error) {
	holderReadFailureNoted.Do(func() {
		log.WarningLog.Printf("execgroup: cannot read the descriptors of process %d, so it was not proven to hold the command's pipes past WaitDelay and was not signalled (logged once): %v", pid, err)
	})
}
