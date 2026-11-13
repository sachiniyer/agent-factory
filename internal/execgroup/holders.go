package execgroup

import (
	"errors"
	"io/fs"
	"sync"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/log"
)

// holderScanner lists the processes that are members of the group pgid leads
// and hold a descriptor on one of the pipe ends ids names.
type holderScanner func(pgid int, ids []uint64) ([]proctree.Process, error)

// holderSignaller signals one process instance, re-checking that the pid
// still names that instance first, the way proctree.Signal does.
type holderSignaller func(p proctree.Process, sig syscall.Signal) error

// reapPipeHolders SIGKILLs every process that is BOTH a member of the group
// pgid leads and a holder of one of the pipe ends ids (#7121). It is the reap
// Output and CombinedOutput run on a pipe still open WaitDelay after the
// leader exited.
//
// Each process it signals is proven three ways, each by its own kernel read:
//   - it is in the group: listed by GroupMembers(pgid), which reads the
//     process's own pgrp, not its ancestry;
//   - it holds one of this command's pipe ends, still open past WaitDelay:
//     HoldsPipeEnd, against the end's identity taken before the child
//     started;
//   - it is the same instance when the signal goes: proctree.Signal re-reads
//     its start time.
//
// A member that holds none of the ends is never signalled, whatever it is. So
// is one whose descriptors cannot be read, such as a non-dumpable process on
// Linux: unread is not proven. Every process this signals, Reap's group kill
// would also have signalled. It is that kill narrowed to the proven
// stragglers.
//
// It scans, signals the holders it found, and scans again, until two scans in
// a row find none. The second scan is there for darwin, whose group read is
// not a snapshot: it can miss a child that a holder was forking (see
// resignal), and a missed child that inherited the pipe holds it too. Like
// killGroup's sweep it is bounded by resignalBound in wall-clock time, whatever
// a read does, and sends nothing once that has passed.
func reapPipeHolders(pgid int, ids []uint64) {
	reapPipeHoldersWith(pgid, ids, scanPipeHolders, proctree.Signal, time.Now().Add(resignalBound))
}

// reapPipeHoldersWith is reapPipeHolders with its scan, signal and deadline
// injected, so every platform's CI can test the loop.
func reapPipeHoldersWith(pgid int, ids []uint64, scan holderScanner, signal holderSignaller, deadline time.Time) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		killPipeHolders(pgid, ids, scan, signal, deadline)
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func killPipeHolders(pgid int, ids []uint64, scan holderScanner, signal holderSignaller, deadline time.Time) {
	pause := resignalPollMin
	quiet := 0
	for {
		holders, err := scan(pgid, ids)
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
			_ = signal(h, syscall.SIGKILL)
		}
		time.Sleep(pause)
		pause = nextPause(pause)
	}
}

// scanPipeHolders is the production holderScanner.
func scanPipeHolders(pgid int, ids []uint64) ([]proctree.Process, error) {
	members, err := proctree.GroupMembers(pgid)
	if err != nil {
		return nil, err
	}
	var holders []proctree.Process
	for _, m := range members {
		held, err := proctree.HoldsPipeEnd(m.PID, ids...)
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
