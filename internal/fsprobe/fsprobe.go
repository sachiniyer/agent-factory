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
	crypto_rand "crypto/rand"
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
// syscall — the write probe's write/sync/close, or the stat probe's
// metadata lookup — did not complete inside probeTimeout. It is always
// paired with context.DeadlineExceeded in the chain — the deadline names
// the non-answer, the sentinel names WHICH bounded probe could not
// establish state — so a classifier that only accepts deadline+sentinel
// pairs (the lost-restore environmental class) can tell this stall from a
// session's own hung operation, which wraps a bare deadline (#5295 review).
var ErrProbeTimedOut = errors.New("filesystem probe did not answer inside its bound")

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
	if _, err := crypto_rand.Read(buf); err != nil {
		// Random content matters: on a filesystem with transparent
		// compression or zero-block elision (ZFS/btrfs compression, APFS
		// clones) an all-zero probe "write" can consume almost no data
		// space — the probe would pass while git's real write still gets
		// the ENOSPC the classifier never sees (#5295 review). A seeded
		// counter pattern is far less compressible than zeros if the CSRNG
		// is somehow unavailable.
		for i := range buf {
			buf[i] = byte(i*31 + 7)
		}
	}
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
	if rerr := removePath(name); rerr != nil {
		// Capture the abandoned object's identity BEFORE Close — Stat on a
		// closed handle always errors, which would leave the leftover
		// unregistered and stageable by a later `git add -A` (#5295 review).
		info, serr := f.Stat()
		cerr := f.Close()
		// Close releases the only state unlink could have depended on — try
		// once more before reporting.
		rerr2 := removePath(name)
		err := fmt.Errorf("cannot unlink probe file %s before writing; writing to a named probe file is unsafe: %w", name, rerr)
		if cerr != nil {
			err = errors.Join(err, fmt.Errorf("closing probe file %s: %w", name, cerr))
		}
		if rerr2 != nil {
			// Record the abandoned object's identity, not just its name:
			// os.SameFile is the only proof a future sweep needs, and a
			// path the owner later replaces or recreates at the same name
			// can never inherit our claim (#5295 review).
			if serr == nil {
				abandonedProbes.Store(name, info)
			}
			err = errors.Join(err, fmt.Errorf("leftover probe file %s could not be removed (remove it by hand): %w", name, rerr2))
		}
		return nil, err
	}
	return f, nil
}

// abandonedProbes maps each probe path this process created and could not
// unlink to the os.FileInfo captured at creation — the only ownership
// proof RemoveDebris accepts. A path is stored only after the SECOND
// unlink fails, and only with a captured identity: a user's file that
// happens to share the probe naming pattern is never recorded, a pathname
// reused after replacement can never match, and no file content is ever
// trusted as proof — any bytes this process writes are equally writable by
// the workspace owner (#5295 review).
var abandonedProbes sync.Map // map[string]os.FileInfo — path → identity at creation

// statPath is the test seam under Stat's bounded flight — the metadata
// syscall that decides whether an existing socket-root or destination dir
// gets probed itself rather than the parent that hosts its name.
var statPath = os.Stat

// lstatPath is the test seam under RemoveDebris — the per-candidate
// metadata lookup that verifies the registered identity still sits at the
// path before anything is unlinked.
var lstatPath = os.Lstat

// removePath is the test seam under openProbeFile — the unlink that drops
// the named fallback's only directory entry before its first byte. Stubbing
// it is the only way to drive the double-unlink-failure abandonment path.
var removePath = os.Remove

// RemoveDebris removes stale named-fallback probe leftovers from dir. The
// named fallback unlinks its tempfile before writing payload bytes, so a
// leftover exists only when BOTH unlink attempts failed (#5295 review).
// Its job is cleanup; a failed cleanup is still filesystem evidence worth
// surfacing.
//
// Deletion requires PROVEN ownership — a filename, a size, or file content
// can never prove a file is ours: the workspace owner may legitimately
// have an empty .af-add-probe-<digits> file, and any byte string this
// process writes is equally writable by them, so deleting on either basis
// is data loss the sandbox reap then makes permanent. The ONLY accepted
// proof is the abandonedProbes registry — this process created that exact
// path — re-verified as the SAME filesystem object via os.SameFile (a
// replaced or recreated lookalike at the same name fails the comparison
// and retires the claim) and still a zero-length regular file (an object
// the user has since written into is theirs — the claim is retired, not
// exercised). Only registered paths are ever inspected: a directory with
// nothing registered is a no-op, so a slow or huge workspace costs one
// stat per abandoned probe, never a full enumeration. Unproven leftovers —
// including debris abandoned before a restart — are snapshotted exactly
// like user content: a committed stray file is a cosmetic wart, strictly
// better than dropping work the filter cannot distinguish. The whole sweep
// runs inside a bounded flight for the same reason Write does: every
// syscall here can wedge on a dead mount inside the synchronous snapshot
// path.
func RemoveDebris(dir string) error {
	_, err := runBounded(dir, sweepFlights, "debris sweep",
		"the filesystem did not answer the debris sweep",
		func() (any, error) { return nil, removeDebrisScan(dir) })
	return err
}

func removeDebrisScan(dir string) error {
	var candidates []string
	abandonedProbes.Range(func(k, _ any) bool {
		if path, ok := k.(string); ok && filepath.Dir(path) == dir {
			candidates = append(candidates, path)
		}
		return true
	})
	var joined []error
	for _, path := range candidates {
		v, _ := abandonedProbes.Load(path)
		orig, _ := v.(os.FileInfo)
		info, lerr := lstatPath(path)
		if lerr != nil {
			if os.IsNotExist(lerr) {
				abandonedProbes.Delete(path) // already gone — nothing to prove
				continue
			}
			joined = append(joined,
				fmt.Errorf("inspecting registered probe file %s: %w", path, lerr))
			continue
		}
		if orig == nil || !info.Mode().IsRegular() || !os.SameFile(orig, info) {
			// The object at this path is not the one we abandoned — a
			// replaced or recreated lookalike never inherits the claim.
			abandonedProbes.Delete(path)
			continue
		}
		if info.Size() != 0 {
			// Our object, but the workspace owner has written into it —
			// their file now; retire the claim (#5295 review).
			abandonedProbes.Delete(path)
			continue
		}
		if rerr := os.Remove(path); rerr != nil && !os.IsNotExist(rerr) {
			joined = append(joined,
				fmt.Errorf("removing stale probe file %s: %w", path, rerr))
			continue
		}
		abandonedProbes.Delete(path)
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
	_, err := runBounded(dir, writeFlights, "space probe",
		"the filesystem did not complete the write",
		func() (any, error) { return nil, writeSpaceProbe(dir) })
	return err
}

// Full reports whether dir's filesystem has zero free blocks — the statfs
// answer for a filesystem the caller cannot write-probe (a root-owned
// daemon data root the process cannot create files in). Zero free blocks
// is the same verdict an ENOSPC write would report, derived without write
// access. The syscall runs inside the same per-dir bounded flight shape as
// Write: a mount that stops answering metadata calls would otherwise wedge
// the synchronous error path it informs (#5295 review).
func Full(dir string) (bool, error) {
	res, err := runBounded(dir, fullnessFlights, "fullness probe",
		"the filesystem did not answer the statfs",
		func() (any, error) { return fsFull(dir) })
	full, _ := res.(bool)
	return full, err
}

// Stat runs os.Stat for dir inside the same per-dir bounded flight shape as
// Write. A caller deciding WHETHER a path exists — does tmux's tmux-<uid>
// socket root or a worktree's destination dir get probed itself, or the
// parent that hosts its name — cannot afford an unbounded metadata lookup:
// on a degraded mount that never answers, the stat would wedge the
// synchronous restore poll before any probe is reached. The returned
// non-answer carries the same ErrProbeTimedOut + context.DeadlineExceeded
// pair the write probe uses, so the lost-restore classifier reads the
// bounded environmental class rather than charging the structural budget
// (#5295 review).
func Stat(dir string) (os.FileInfo, error) {
	res, err := runBounded(dir, statFlights, "stat probe",
		"the filesystem did not answer the stat",
		func() (any, error) { return statPath(dir) })
	info, _ := res.(os.FileInfo)
	return info, err
}

type probeFlight struct {
	done     chan struct{}
	res      any
	err      error
	timedOut bool
}

type flightRegistry struct {
	sync.Mutex
	byDir map[string]*probeFlight
}

var writeFlights = &flightRegistry{byDir: map[string]*probeFlight{}}
var statFlights = &flightRegistry{byDir: map[string]*probeFlight{}}
var sweepFlights = &flightRegistry{byDir: map[string]*probeFlight{}}
var fullnessFlights = &flightRegistry{byDir: map[string]*probeFlight{}}

func runBounded(dir string, flights *flightRegistry, what, detail string,
	run func() (any, error)) (any, error) {
	flights.Lock()
	if active := flights.byDir[dir]; active != nil {
		if active.timedOut {
			flights.Unlock()
			return nil, fmt.Errorf(
				"%s of %s is still running after an earlier deadline: %w: %w",
				what, dir, ErrProbeTimedOut, context.DeadlineExceeded,
			)
		}
		flights.Unlock()
		return waitForFlight(dir, flights, what, detail, active)
	}
	flight := &probeFlight{done: make(chan struct{})}
	flights.byDir[dir] = flight
	flights.Unlock()
	go func() {
		flight.res, flight.err = run()
		flights.Lock()
		if flights.byDir[dir] == flight {
			delete(flights.byDir, dir)
		}
		close(flight.done)
		flights.Unlock()
	}()
	return waitForFlight(dir, flights, what, detail, flight)
}

func waitForFlight(dir string, flights *flightRegistry, what, detail string,
	flight *probeFlight) (any, error) {
	timer := time.NewTimer(probeTimeout)
	defer timer.Stop()
	select {
	case <-flight.done:
		return flight.res, flight.err
	case <-timer.C:
		flights.Lock()
		if flights.byDir[dir] == flight {
			flight.timedOut = true
			flights.Unlock()
			return nil, fmt.Errorf(
				"%s of %s did not answer within %s — %s: %w: %w",
				what, dir, probeTimeout, detail, ErrProbeTimedOut, context.DeadlineExceeded,
			)
		}
		flights.Unlock()
		<-flight.done
		return flight.res, flight.err
	}
}
