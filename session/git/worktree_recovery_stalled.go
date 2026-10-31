package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
)

// Test seam for path resolution that stalls beneath the bounded worker.
var boundedResolvePath = pathutil.ResolveForCompare

// Test seam for a metadata syscall that stalls beneath the bounded worker.
var boundedLstatPath = os.Lstat

// BoundedLstat runs os.Lstat through the shared identity-probe deadline (#3278
// review). The destruction path's absence checks run while the caller holds
// the session operation lock, so a stalled FUSE/NFS mount must surface as a
// timeout — an unknown, fail-closed answer distinct from ENOENT — never as an
// unbounded syscall wedging the kill. Per-path flight with a timed-out latch,
// mirroring VerifyArchivedWorktreePointer: concurrent callers join one worker,
// and after a deadline retries are refused instead of stacking goroutines and
// blocked OS threads against the same stalled path until the stuck worker
// finally returns and unpublishes itself.
func BoundedLstat(path string) (os.FileInfo, error) {
	boundedLstatFlights.Lock()
	if active := boundedLstatFlights.byPath[path]; active != nil {
		if active.timedOut {
			boundedLstatFlights.Unlock()
			return nil, fmt.Errorf(
				"path check for %s is still running after an earlier deadline: %w",
				path, context.DeadlineExceeded,
			)
		}
		boundedLstatFlights.Unlock()
		return waitForBoundedLstat(path, active)
	}
	flight := &boundedLstatFlight{done: make(chan struct{})}
	boundedLstatFlights.byPath[path] = flight
	boundedLstatFlights.Unlock()
	lstat := boundedLstatPath
	go func() {
		flight.info, flight.err = lstat(path)
		boundedLstatFlights.Lock()
		if boundedLstatFlights.byPath[path] == flight {
			delete(boundedLstatFlights.byPath, path)
		}
		close(flight.done)
		boundedLstatFlights.Unlock()
	}()
	return waitForBoundedLstat(path, flight)
}

type boundedLstatFlight struct {
	done     chan struct{}
	info     os.FileInfo
	err      error
	timedOut bool
}

var boundedLstatFlights = struct {
	sync.Mutex
	byPath map[string]*boundedLstatFlight
}{byPath: map[string]*boundedLstatFlight{}}

func waitForBoundedLstat(path string, flight *boundedLstatFlight) (os.FileInfo, error) {
	timer := time.NewTimer(relocationIdentityTimeout.Load())
	defer timer.Stop()
	select {
	case <-flight.done:
		return flight.info, flight.err
	case <-timer.C:
		boundedLstatFlights.Lock()
		if boundedLstatFlights.byPath[path] == flight {
			flight.timedOut = true
			boundedLstatFlights.Unlock()
			return nil, fmt.Errorf(
				"timed out after %s while checking path %s: %w",
				relocationIdentityTimeout.Load(), path, context.DeadlineExceeded,
			)
		}
		boundedLstatFlights.Unlock()
		<-flight.done
		return flight.info, flight.err
	}
}

// boundedResolveForCompare runs pathutil.ResolveForCompare through the shared
// identity-probe deadline with a per-path flight and timed-out latch (#3278
// review): symlink resolution stats real directories, so a permanently
// stalled mount must neither wedge the caller nor accumulate one blocked
// worker per retry. Joining by path is sound here — resolution depends only
// on the path, not on any per-call snapshot.
func boundedResolveForCompare(path string) (string, error) {
	resolveFlights.Lock()
	if active := resolveFlights.byPath[path]; active != nil {
		if active.timedOut {
			resolveFlights.Unlock()
			return "", fmt.Errorf(
				"path normalization for %s is still running after an earlier deadline: %w",
				path, context.DeadlineExceeded,
			)
		}
		resolveFlights.Unlock()
		return waitForResolveFlight(path, active)
	}
	flight := &resolveFlight{done: make(chan struct{})}
	resolveFlights.byPath[path] = flight
	resolveFlights.Unlock()
	resolve := boundedResolvePath
	go func() {
		flight.resolved = resolve(path)
		resolveFlights.Lock()
		if resolveFlights.byPath[path] == flight {
			delete(resolveFlights.byPath, path)
		}
		close(flight.done)
		resolveFlights.Unlock()
	}()
	return waitForResolveFlight(path, flight)
}

type resolveFlight struct {
	done     chan struct{}
	resolved string
	timedOut bool
}

var resolveFlights = struct {
	sync.Mutex
	byPath map[string]*resolveFlight
}{byPath: map[string]*resolveFlight{}}

func waitForResolveFlight(path string, flight *resolveFlight) (string, error) {
	timer := time.NewTimer(relocationIdentityTimeout.Load())
	defer timer.Stop()
	select {
	case <-flight.done:
		return flight.resolved, nil
	case <-timer.C:
		resolveFlights.Lock()
		if resolveFlights.byPath[path] == flight {
			flight.timedOut = true
			resolveFlights.Unlock()
			return "", fmt.Errorf(
				"timed out after %s while normalizing path %s: %w",
				relocationIdentityTimeout.Load(), path, context.DeadlineExceeded,
			)
		}
		resolveFlights.Unlock()
		<-flight.done
		return flight.resolved, nil
	}
}

// RestoreStalledFenceAfterFailedSettle re-materializes the reclaimable
// stalled state after a failed settle of a reclaimed stalled record (#3278
// review). The failed settle leaves an in-memory claim_stale fence that disk
// never saw — the durable record is still the stalled fence — and that
// mismatch makes the kill producer skip while admission refuses, stranding
// same-process retries before any tombstone exists. Restoring the stalled
// shape re-aligns memory with disk so the next kill reclaims and re-derives.
//
// The reclaim gate passes any stalled record regardless of identity, so the
// reclaimed record — and therefore the durable record — may be identity-
// qualified (the shape recordStall captures when the timed-out claim was on
// the recorded worktree path, persisting IdentityKnown/Device/Inode/FileType
// and AlternatePath). A failed settle's revalidation going unknown, not
// wrong, does not invalidate that evidence: it remains the conservative stale
// fence the failed-settle path installs. Preserve it so the restored record
// matches disk and claim resolution can still arbitrate both candidates on
// the next retry. Drop to the bare identity-unknown shape only when the
// durable record the claim consumed carried no identity and no alternate —
// the only case the bare restore is correct for. Forcing the bare shape on
// an identity-qualified record would misalign memory from disk and unlock
// the ENOENT discharges that this file deliberately excludes for
// identity-qualified records, and a subsequent persist would durably erase
// the alternate pointer needed to arbitrate candidates after a restart.
//
// The claim_stale fence a failed settle leaves behind can carry an identity
// the durable record never had: a claim over an identity-unknown stalled
// record probes the recorded path and captures whatever answers, so the
// claim_stale fence carries that freshly observed identity even though the
// durable record was bare. Restoring it would diverge from disk and close
// the identity-unknown ENOENT discharge the next same-process retry needs
// when the archived path disappeared between claim and settle; only a
// restart reloads the original durable shape. ClaimRelocationSource
// therefore retains the pre-claim fence shape on relocationRecoveryOrigin,
// and the restore uses it instead of inferring the shape from the stale
// fence: when the durable record was identity-unknown, the bare shape is
// restored regardless of the identity the claim observed (#5555).
func (g *GitWorktree) RestoreStalledFenceAfterFailedSettle() error {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if g.activeRelocationClaim != nil || g.relocationRecovery == nil ||
		g.relocationRecovery.State != RelocationRecoveryClaimStale {
		return fmt.Errorf("no stale fence from a failed settle to restore")
	}
	stale := g.relocationRecovery
	origin := g.relocationRecoveryOrigin
	g.relocationRecoveryOrigin = nil
	// When the durable record the claim consumed was a bare identity-unknown
	// stalled fence, restore that bare shape even if the claim resolved a fresh
	// identity — the durable record is the authority, and a bare stall keeps the
	// identity-unknown ENOENT discharge armed for the next same-process retry
	// when the archived path disappeared between claim and settle (#5555).
	if origin != nil && origin.State == RelocationRecoveryStalled &&
		!origin.IdentityKnown && origin.AlternatePath == "" {
		g.relocationRecovery = &RelocationRecovery{State: RelocationRecoveryStalled}
		return nil
	}
	if stale.IdentityKnown || stale.AlternatePath != "" {
		g.relocationRecovery = &RelocationRecovery{
			State:         RelocationRecoveryStalled,
			IdentityKnown: stale.IdentityKnown,
			Device:        stale.Device,
			Inode:         stale.Inode,
			FileType:      stale.FileType,
			AlternatePath: stale.AlternatePath,
		}
		return nil
	}
	g.relocationRecovery = &RelocationRecovery{State: RelocationRecoveryStalled}
	return nil
}

// SetRelocationIdentityErrorAfterNForTest makes identity inspection of one
// exact path return err only on the n-th probe (1-indexed) of that path after
// the seam is installed; earlier probes fall through to the real inspector. It
// exists for callers outside this package that must let one probe (e.g. a
// reclaim's resolution) succeed and then fail a later probe (e.g. a settle's
// revalidation) within a single driver call, to exercise the failed-settle
// restore path deterministically without racing a physical directory change.
func SetRelocationIdentityErrorAfterNForTest(path string, err error, n int) func() {
	previous := relocationPathIdentity
	var calls int
	relocationPathIdentity = func(observed string) (pathIdentity, error) {
		if observed == path {
			calls++
			if calls == n {
				return pathIdentity{}, err
			}
		}
		return previous(observed)
	}
	return func() { relocationPathIdentity = previous }
}

// SettleStalledRelocationForAbsentPath clears an identity-unknown stalled
// record once its guarded pathname is conclusively absent (#3278 review). Such
// a record holds no directory identity — the probe that created it never
// answered — so when the pathname now answers ENOENT there is nothing left
// for the fence to protect, and retaining it would leave the row permanently
// undeletable: every claim wraps the same ENOENT in ErrRelocateStateUnknown
// and every kill and restore refuses. Identity-qualified records are
// deliberately excluded: they may carry an alternate candidate that the
// ordinary claim resolution must arbitrate.
func (g *GitWorktree) SettleStalledRelocationForAbsentPath() error {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if !g.identityUnknownStallLocked() {
		return fmt.Errorf("no identity-unknown stalled relocation record to settle")
	}
	if _, err := BoundedLstat(g.worktreePath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stalled relocation path %s is not conclusively absent: %v", g.worktreePath, err)
	}
	g.relocationRecovery = nil
	return nil
}

// identityUnknownStallLocked reports whether the only lifecycle state is an
// identity-unknown stalled record with no alternate candidate — the shape a
// timed-out relocation probe and FenceUnverifiedWorktree install. It names no
// directory and no second location, so nothing but the recorded path itself can
// be at stake.
func (g *GitWorktree) identityUnknownStallLocked() bool {
	r := g.relocationRecovery
	return g.activeRelocationClaim == nil && r != nil && r.State == RelocationRecoveryStalled &&
		!r.IdentityKnown && r.AlternatePath == ""
}

// SettleAbsentIdentityUnknownStall is settleAbsentIdentityUnknownStallLocked
// for callers outside the package: every destructive gate that refuses on an
// unresolved record (kill admission, kill teardown) runs it first, so the one
// record that provably guards nothing cannot strand the row.
func (g *GitWorktree) SettleAbsentIdentityUnknownStall() bool {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	return g.settleAbsentIdentityUnknownStallLocked()
}

// settleAbsentIdentityUnknownStallLocked clears that record once the recorded
// path conclusively answers ENOENT (#5102). With no identity and no alternate,
// the record can only have been guarding whatever sits at the recorded path;
// when nothing does, keeping it is what makes the row permanently unarchivable
// and unkillable, because every claim and cleanup would refuse on the same
// unknown. It never clears while the path exists or the lstat cannot answer —
// that is exactly the state the fence exists for. Reports whether it cleared.
func (g *GitWorktree) settleAbsentIdentityUnknownStallLocked() bool {
	if !g.identityUnknownStallLocked() {
		return false
	}
	if _, err := BoundedLstat(g.worktreePath); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	g.relocationRecovery = nil
	g.absentStallSettled = true
	return true
}

// AbsentStallSettled reports whether this process discharged an
// identity-unknown stall over the recorded path on proven absence, and af has
// not placed a worktree there since (#5102). Kill admission and kill teardown
// share this GitWorktree, so a discharge at admission is visible at teardown
// even though teardown's own settle then finds no record to clear.
func (g *GitWorktree) AbsentStallSettled() bool {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	return g.absentStallSettled
}

// markWorktreePlaced drops the absent-stall proof once af itself has put a
// worktree at the recorded path (#5102): that directory is af's, and a kill
// must clean it up normally rather than refuse it as an unverified occupant.
func (g *GitWorktree) markWorktreePlaced() {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	g.absentStallSettled = false
}
