// Package fsprobe answers one question with real write evidence: can this
// filesystem still complete a write?
//
// It exists because a failing subprocess loses errno identity — ENOSPC
// arrives only as text on an *exec.ExitError's stderr, and the lost-restore
// classifier deliberately never reads message text. The probe produces the
// same verdict from THIS process instead: create a file, write a
// representative payload, Sync it into the filesystem's accounting, close,
// and report whatever errno the filesystem actually gave.
//
// The whole probe runs inside a bounded flight (#5295 review): Write, Sync,
// and Close can each block indefinitely on a degraded NFS/FUSE mount, and the
// probe's callers run synchronously inside the daemon's restore poll — an
// unbounded diagnostic must not wedge the loop it is informing. Per-dir
// flights make concurrent probes of the same path join one worker; once a
// flight misses its deadline the timed-out latch refuses retries instead of
// stacking blocked goroutines and OS threads against the same stalled path
// (the BoundedLstat shape, #3278).
package fsprobe

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// writeSpaceProbeSize bounds the probe write below. A single byte is not
// enough evidence: a filesystem that can still accept stray bytes but not a
// real worktree write — nearly full, or one whose block accounting git hit
// first — would pass the probe while every retry keeps failing, and the
// failure classifies structural until the budget gives up on an environmental
// outage (#5295 review). The worktree metadata writes this probe stands in
// for (gitdir registration, HEAD, config, index) sit in the tens-to-hundreds
// of KB; a real checkout is unbounded, so no fixed size can reproduce git's
// exact demand. One MiB covers the metadata class while staying noise-level
// transient allocation on any disk that is not already exhausted.
const writeSpaceProbeSize = 1 << 20

// probeTimeout bounds one probe flight. A writable filesystem completes a
// 1 MiB write+sync in milliseconds; a flight still running at the bound is
// the wedge the bound exists to catch. The value mirrors the relocation
// identity probe's deadline — the same "the mount did not answer" class.
var probeTimeout = 2 * time.Second

// ErrProbeTimedOut identifies the bounded flight's non-answer: the probe's
// write/sync/close did not complete inside probeTimeout. It is always paired
// with context.DeadlineExceeded in the chain — the deadline names the
// non-answer, the sentinel names WHICH bounded probe could not establish
// state — so a classifier that only accepts deadline+sentinel pairs (the
// lost-restore environmental class) can tell this stall from a session's own
// hung operation, which wraps a bare deadline (#5295 review).
var ErrProbeTimedOut = errors.New("filesystem write probe did not answer inside its bound")

// writeSpaceProbe is the production probe body (and the test seam): create a
// file in dir and WRITE a representative payload to it, then Sync before
// close so the bytes must reach the filesystem's accounting. The write is
// the point — on a tmpfs filled past its size limit a zero-length create
// still succeeds because dentries and inodes live outside the data budget,
// so only a real data write sees ENOSPC. The Sync is the second half — on a
// delayed-allocation filesystem (ext4, xfs) an unsynced write can land in
// the page cache and report success while the filesystem is already out of
// blocks, so a write that only looked cheap could pass while git's real
// write got the ENOSPC the classifier never saw. The file is removed before
// returning either way.
var writeSpaceProbe = func(dir string) error {
	f, err := openProbeFile(dir)
	if err != nil {
		return err
	}
	// From here on the file has no name — either it never had one (O_TMPFILE)
	// or it was unlinked before this point. Nothing stageable can exist no
	// matter how the writes below end.
	var werr error
	buf := make([]byte, 64<<10)
	for written := 0; written < writeSpaceProbeSize; {
		chunk := buf
		if writeSpaceProbeSize-written < len(chunk) {
			chunk = chunk[:writeSpaceProbeSize-written]
		}
		n, wErr := f.Write(chunk)
		written += n
		if wErr != nil {
			werr = wErr
			break
		}
	}
	var serr error
	if werr == nil {
		serr = f.Sync()
	}
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	if serr != nil {
		return serr
	}
	return cerr
}

// openProbeFile returns a write-capable probe file that can never be staged:
// an O_TMPFILE anonymous inode where the platform offers one (Linux — tmpfs,
// ext4, xfs, btrfs, overlayfs all support it), falling back to a named
// tempfile that is unlinked BEFORE its first byte is written. On the named
// path an unlink failure is fatal to the probe — never write into a file
// whose name could not be dropped, because a surviving .af-add-probe-* inside
// a tracked worktree is exactly what a later `git add -A` would stage as
// workspace content; the leftover's path is surfaced in the error so an
// operator can remove it by hand (#5295 review).
func openProbeFile(dir string) (*os.File, error) {
	if f, err := openAnonymousProbeFile(dir); err == nil {
		return f, nil
	}
	f, err := os.CreateTemp(dir, ".af-add-probe-*")
	if err != nil {
		return nil, err
	}
	name := f.Name()
	if rerr := os.Remove(name); rerr != nil {
		cerr := f.Close()
		// Close releases the only state unlink could have depended on — try
		// once more before reporting.
		rerr2 := os.Remove(name)
		err := fmt.Errorf("cannot unlink probe file %s before writing; writing to a named probe file is unsafe: %w", name, rerr)
		if cerr != nil {
			err = errors.Join(err, fmt.Errorf("closing probe file %s: %w", name, cerr))
		}
		if rerr2 != nil {
			err = errors.Join(err, fmt.Errorf("leftover probe file %s could not be removed (remove it by hand): %w", name, rerr2))
		}
		return nil, err
	}
	return f, nil
}

// RemoveDebris removes stale named-fallback probe leftovers from dir. The
// named fallback unlinks its tempfile before writing a single byte, so a
// leftover .af-add-probe-* only exists when BOTH unlink attempts failed — an
// empty file that is nevertheless a name inside the tracked tree, which the
// next `git add -A` snapshot would stage as workspace content after the
// filesystem recovers (#5295 review). Callers that snapshot a tree sweep
// before staging so a wedged unlink from an earlier attempt cannot become
// committed debris. Removal failures are joined and returned — a filesystem
// that cannot unlink is evidence in its own right.
func RemoveDebris(dir string) error {
	matches, err := filepath.Glob(filepath.Join(dir, ".af-add-probe-*"))
	if err != nil {
		return err
	}
	var joined []error
	for _, name := range matches {
		if rerr := os.Remove(name); rerr != nil && !os.IsNotExist(rerr) {
			joined = append(joined,
				fmt.Errorf("removing stale probe file %s: %w", name, rerr))
		}
	}
	return errors.Join(joined...)
}

// Write runs the space probe for dir inside a per-dir bounded flight: a
// filesystem that stops completing writes (a degraded mount) must surface as
// a typed non-answer — context.DeadlineExceeded carried in the chain —
// rather than holding the caller's synchronous restore poll hostage. Once a
// flight times out, later callers get the still-running refusal until the
// stuck worker returns and unpublishes itself; a probe that answers inside
// the bound reports the filesystem's own verdict, which on a full or wedged
// mount is the real errno.
func Write(dir string) error {
	probeFlights.Lock()
	if active := probeFlights.byDir[dir]; active != nil {
		if active.timedOut {
			probeFlights.Unlock()
			return fmt.Errorf(
				"space probe of %s is still running after an earlier deadline: %w: %w",
				dir, ErrProbeTimedOut, context.DeadlineExceeded,
			)
		}
		probeFlights.Unlock()
		return waitForProbe(dir, active)
	}
	flight := &probeFlight{done: make(chan struct{})}
	probeFlights.byDir[dir] = flight
	probeFlights.Unlock()
	go func() {
		flight.err = writeSpaceProbe(dir)
		probeFlights.Lock()
		if probeFlights.byDir[dir] == flight {
			delete(probeFlights.byDir, dir)
		}
		close(flight.done)
		probeFlights.Unlock()
	}()
	return waitForProbe(dir, flight)
}

type probeFlight struct {
	done     chan struct{}
	err      error
	timedOut bool
}

var probeFlights = struct {
	sync.Mutex
	byDir map[string]*probeFlight
}{byDir: map[string]*probeFlight{}}

func waitForProbe(dir string, flight *probeFlight) error {
	timer := time.NewTimer(probeTimeout)
	defer timer.Stop()
	select {
	case <-flight.done:
		return flight.err
	case <-timer.C:
		probeFlights.Lock()
		if probeFlights.byDir[dir] == flight {
			flight.timedOut = true
			probeFlights.Unlock()
			return fmt.Errorf(
				"space probe of %s did not answer within %s — the filesystem did not complete the write: %w: %w",
				dir, probeTimeout, ErrProbeTimedOut, context.DeadlineExceeded,
			)
		}
		probeFlights.Unlock()
		<-flight.done
		return flight.err
	}
}
