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
	// Join, never select: delayed writeback can put the typed resource errno
	// (ENOSPC/EDQUOT) on Close while Write or Sync reported something
	// structural — dropping cerr would hand the classifier only the
	// structural error and let it spend give-up budget during an
	// environmental outage (#5295 review).
	return errors.Join(werr, serr, cerr)
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
			// Leave the file alone and say where it is. It is treated as
			// user content — never deleted on a name match, and snapshotted
			// like any other file: a committed stray is a cosmetic wart,
			// strictly better than deleting a file whose ownership cannot
			// be proven (#5295 review).
			err = errors.Join(err, fmt.Errorf("leftover probe file %s could not be removed (remove it by hand): %w", name, rerr2))
		}
		return nil, err
	}
	return f, nil
}

// statPath is the test seam under Stat's bounded flight — the metadata
// syscall that decides whether an existing socket-root or destination dir
// gets probed itself rather than the parent that hosts its name.
var statPath = os.Stat

// lstatPath is the test seam under Lstat's bounded flight — the metadata
// syscall that reports the final path component itself without following a
// symlink, so a worktree destination whose final name is a symlink to an
// existing directory is not probed through to its target's filesystem.
var lstatPath = os.Lstat

// evalSymlinksPath is the test seam under EvalSymlinks's bounded flight — the
// metadata + Readlink walk that resolves a path to its real destination. The
// raw filepath.EvalSymlinks performs an unbounded sequence of metadata
// lookups that would wedge the synchronous restore poll on a stalled mount
// (the same hazard Stat is bounded for), so EvalSymlinks runs it inside a
// per-dir flight.
var evalSymlinksPath = filepath.EvalSymlinks

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

// Lstat runs os.Lstat for path inside the same per-dir bounded flight shape as
// Stat. A caller deciding whether the final component of a worktree
// destination is a symlink — so a `git worktree add` whose destination already
// exists as a symlink to a nonempty directory is not probed through the symlink
// — cannot follow the symlink the way Stat does: `git worktree add` rejects a
// destination that already exists without writing through it, so probing the
// symlink target's filesystem would target an unrelated filesystem and a
// coincidental ENOSPC there would stall the lost-restore loop indefinitely on
// a filesystem freeing it cannot fix. Lstat reports the final component
// itself so the probe can skip a symlinked destination. The syscall is bounded
// for the same reason Stat is: an unbounded os.Lstat on a mount that never
// answers would wedge the synchronous restore poll the probe informs. The
// returned non-answer carries the same ErrProbeTimedOut +
// context.DeadlineExceeded pair the other probes use, so the lost-restore
// classifier reads the bounded environmental class rather than charging the
// structural budget (#5295 review).
func Lstat(path string) (os.FileInfo, error) {
	res, err := runBounded(path, lstatFlights, "lstat probe",
		"the filesystem did not answer the lstat",
		func() (any, error) { return lstatPath(path) })
	info, _ := res.(os.FileInfo)
	return info, err
}

// EvalSymlinks runs filepath.EvalSymlinks for path inside the same per-dir
// bounded flight shape as Stat. A caller resolving a path through symlinks —
// to mirror the kernel's chdir for a worktree probe, or to find the real
// filesystem a relative argument writes to — cannot afford an unbounded
// Readlink/Stat walk: on a degraded mount that never answers, the resolution
// would wedge the synchronous restore poll before the probe is reached. The
// returned non-answer carries the same ErrProbeTimedOut +
// context.DeadlineExceeded pair the other probes use, so the lost-restore
// classifier reads the bounded environmental class rather than charging the
// structural budget for a mount stall (#5295 review).
func EvalSymlinks(path string) (string, error) {
	res, err := runBounded(path, evalSymlinksFlights, "eval-symlinks probe",
		"the filesystem did not answer the symlink resolution",
		func() (any, error) { return evalSymlinksPath(path) })
	s, _ := res.(string)
	return s, err
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
var lstatFlights = &flightRegistry{byDir: map[string]*probeFlight{}}
var evalSymlinksFlights = &flightRegistry{byDir: map[string]*probeFlight{}}
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
