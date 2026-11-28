package git

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// retainedCleanupInFlight is the process-local fence behind "at most one
// cleanup worker per retained tree" (#7928, #7937 review R1/R2). It records the
// durable filesystem path of every retained archive tree a cleanup worker
// claimed but has not finished removing.
//
// It is keyed on the tree's RECORDED path — the same string a durable
// archive_report row carries — so the decision survives a GitWorktree rebuilt
// from storage: the ghost path and a restored session rebuild `gw`, and the
// report they reload names the same tree the earlier attempt fenced.
//
// It is deliberately NOT durable and NOT on the GitWorktree. A worker stuck
// forever in an uninterruptible syscall holds its entries until the process
// restarts — which is the correct bound, because the only way to clear a
// permanently wedged worker is for the process that owns it to die. An
// in-memory `gw` flag could never reach a rebuilt handle, and a durable record
// outlives the worker's own return: the #7937 head latched a successful
// slow-tree delete behind "restart the daemon" forever (R1).
var retainedCleanupInFlight = struct {
	sync.Mutex
	paths map[string]struct{}
}{paths: make(map[string]struct{})}

// retainedCleanupOutstanding reports whether a claimed tree in this report is
// still owned by an outstanding worker, naming the first fenced path. It is the
// advisory half of the fence: callers use it to refuse BEFORE spawning a
// worker, so a retry against a stalled filesystem starts nothing. The claim
// inside runRetainedCleanupClaimed is the authoritative half — it decides
// atomically under the same mutex, so two attempts that both pass this check
// still serialize onto a single worker.
func retainedCleanupOutstanding(report ArchiveReport) string {
	retainedCleanupInFlight.Lock()
	defer retainedCleanupInFlight.Unlock()
	for _, tree := range report.RetainedTrees {
		if _, ok := retainedCleanupInFlight.paths[tree.filesystemPath()]; ok {
			return tree.filesystemPath()
		}
	}
	return ""
}

// claimRetainedCleanupTrees atomically claims every tree in the report for one
// worker: either all paths register and a release func comes back, or none do
// and the already-claimed path is named. All-or-nothing matters — claiming a
// subset would let two workers each hold half a report while believing the
// other idle. The release func belongs to the worker alone: a deadline that
// abandons it must not drop the fence, or the next retry would start a second
// worker against the same stalled tree (the #7937/Codex P1 leak). A returned
// worker releases every path it claimed — success frees the next retry to
// settle, failure frees it to start fresh.
func claimRetainedCleanupTrees(report ArchiveReport) (release func(), outstanding string) {
	retainedCleanupInFlight.Lock()
	defer retainedCleanupInFlight.Unlock()
	for _, tree := range report.RetainedTrees {
		if _, ok := retainedCleanupInFlight.paths[tree.filesystemPath()]; ok {
			return nil, tree.filesystemPath()
		}
	}
	claimed := make([]string, 0, len(report.RetainedTrees))
	for _, tree := range report.RetainedTrees {
		path := tree.filesystemPath()
		// An empty path is refused by removeRetainedArchiveTree before any
		// syscall, so it can never stall — and two unrelated records must never
		// share a fence key.
		if path == "" {
			continue
		}
		retainedCleanupInFlight.paths[path] = struct{}{}
		claimed = append(claimed, path)
	}
	return func() {
		retainedCleanupInFlight.Lock()
		for _, path := range claimed {
			delete(retainedCleanupInFlight.paths, path)
		}
		retainedCleanupInFlight.Unlock()
	}, ""
}

// retainedCleanupOutstandingError is what a fenced retry gets: the cleanup is
// still running on a stalled or slow filesystem and a second worker was
// refused. It says only what this layer knows — whether the kill retries is
// the caller's policy to state, once: a live kill's retainedKillTeardownError
// says it "will be retried automatically", while a ghost kill is never retried
// and its wrapper says to run the kill again (daemon/manager_sessions.go).
// It is a plain error, never a refusal sentinel — the record stays kept for
// exactly the attempt the caller describes.
func retainedCleanupOutstandingError(path string) error {
	return fmt.Errorf(
		"retained archive tree cleanup at %s is still running from an earlier attempt on a stalled or slow filesystem; "+
			"refusing to start a second cleanup worker", path)
}

// runRetainedCleanupClaimed runs fn — the retained-tree cleanup — while holding
// this report's fence. The claim is taken inside the worker's own run and
// released by its return, so the fence's lifetime IS the worker's lifetime: an
// abandoned worker holds it until it finishes or the process ends, and nothing
// else — no deadline, no retry, no rebuilt GitWorktree — clears it.
//
// An empty report fences nothing, but fn is still invoked: the seam standing in
// for it is "the retained-tree call" to its tests, so skipping the call here
// would silently change what the repo-gone deadline contract runs.
func runRetainedCleanupClaimed(g *GitWorktree, fn func(*GitWorktree) error) error {
	report := g.GetArchiveReport()
	if report.Empty() {
		return fn(g)
	}
	release, outstanding := claimRetainedCleanupTrees(report)
	if outstanding != "" {
		return retainedCleanupOutstandingError(outstanding)
	}
	defer release()
	return fn(g)
}

// cleanupRetainedArchiveTrees consumes every complete source tree owned by this
// report. Kill calls it before deleting the ordinary worktree and session record,
// so retained bytes cannot outlive the destructive action that removed their
// only durable handle.
//
// Every caller must route it through runRetainedCleanupClaimed (or an
// equivalent claim): the filesystem work inside is raw and unbounded, so the
// per-tree fence is what keeps a second worker from stacking onto a tree an
// abandoned one is still inside.
func (g *GitWorktree) cleanupRetainedArchiveTrees() error {
	for {
		report := g.GetArchiveReport()
		if report.Empty() {
			return nil
		}
		tree := report.RetainedTrees[0]
		if err := removeRetainedArchiveTree(tree); err != nil {
			return err
		}
		g.dropRetainedArchiveTree(tree)
	}
}

func removeRetainedArchiveTree(tree ArchiveRetainedTree) error {
	path := tree.filesystemPath()
	if path == "" || !privateSourceName(filepath.Base(path)) {
		return fmt.Errorf("refusing to remove retained archive tree with invalid private path %q", path)
	}
	if !tree.IdentityKnown {
		return fmt.Errorf("refusing to remove retained archive tree %s without its durable identity", path)
	}
	parentPath := filepath.Dir(path)
	parent, _, err := openDirectoryPathFollowingLinks(parentPath, "retained archive parent")
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer parent.Close()

	name := filepath.Base(path)
	current, err := identityAt(parent, name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect retained archive tree %s: %w", path, err)
	}
	if !tree.identity().same(current) {
		return fmt.Errorf("refusing to remove retained archive tree %s because its private name identifies different data", path)
	}
	// A recorded creation pin must match too: a delete-and-recreate at this
	// private name could hand the recycled (dev, ino) to different data
	// (#7900). A probe that cannot answer a recorded pin fails closed with
	// its own "could not read" wording — an unanswered question, not an
	// answered mismatch (R4).
	pin, pinErr := creationPin(parent, name)
	if err := refuseUnreadableTreePin(tree, pin, pinErr, path); err != nil {
		return err
	}
	root, _, err := openDirectoryAt(parent, name, path, "retained archive")
	if err != nil {
		return err
	}
	defer root.Close()
	opened, err := identityFromFile(root)
	if err != nil {
		return err
	}
	if !tree.identity().same(opened) {
		return fmt.Errorf("refusing to remove retained archive tree %s because its opened identity changed", path)
	}
	openedPin, openedPinErr := creationPinOpened(root)
	if err := refuseUnreadableTreePin(tree, openedPin, openedPinErr, path); err != nil {
		return err
	}
	if err := removeOpenedDirectory(parent, name, path, root, nil); err != nil {
		return fmt.Errorf("remove retained archive tree %s: %w", path, err)
	}
	return nil
}

// errCreationStampUnavailable is what a pin probe that answers nothing means
// for a record carrying one: the filesystem offered no birth time, so the
// recorded stamp went unchecked — an unreadable pin, not a mismatched one.
var errCreationStampUnavailable = errors.New("the filesystem answered no creation stamp")

// refuseUnreadableTreePin checks a recorded creation pin against a fresh
// probe for kill cleanup (#7900). A record carrying no pin has nothing to
// check. A pin the probe cannot read — a failed statx/fstat, or a
// filesystem that answered no stamp — refuses with "could not read the
// creation stamp": that is a question that went unanswered, not an answered
// mismatch, so it must not read "identifies different data" (#7900 R4).
// Only a probe that answered and answered differently is different data.
func refuseUnreadableTreePin(tree ArchiveRetainedTree, pin int64, pinErr error, path string) error {
	if tree.CreationTime == 0 {
		return nil
	}
	switch {
	case pinErr != nil:
		return fmt.Errorf("refusing to remove retained archive tree %s: could not read the creation stamp: %w", path, pinErr)
	case pin == 0:
		return fmt.Errorf("refusing to remove retained archive tree %s: could not read the creation stamp: %w", path, errCreationStampUnavailable)
	case pin != tree.CreationTime:
		return fmt.Errorf("refusing to remove retained archive tree %s because its private name identifies different data", path)
	}
	return nil
}

func privateSourceName(name string) bool {
	const prefix = ".af-source-"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	random := strings.TrimPrefix(name, prefix)
	if len(random) != 32 {
		return false
	}
	_, err := hex.DecodeString(random)
	return err == nil
}

func (g *GitWorktree) dropRetainedArchiveTree(removed ArchiveRetainedTree) {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	for index, tree := range g.archiveReport.RetainedTrees {
		if sameRetainedArchiveTree(tree, removed) {
			g.archiveReport.RetainedTrees = append(
				g.archiveReport.RetainedTrees[:index], g.archiveReport.RetainedTrees[index+1:]...,
			)
			g.refreshArchiveWarningLocked()
			return
		}
	}
}

func sameRetainedArchiveTree(left, right ArchiveRetainedTree) bool {
	return left.IdentityKnown == right.IdentityKnown &&
		left.identity().same(right.identity()) &&
		left.CreationTime == right.CreationTime &&
		left.filesystemPath() == right.filesystemPath()
}
