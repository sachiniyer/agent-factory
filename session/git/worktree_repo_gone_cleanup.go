package git

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const cleanupGenerationXattr = "user.agent-factory.cleanup-generation"

var cleanupGenerationInstall = installCleanupGeneration

var repoGoneCleanupRetainedTrees = func(worktree *GitWorktree) error {
	return worktree.cleanupRetainedArchiveTrees()
}

// probeRepoGoneOrigin applies restore's repository-validity rule. Its caller
// supplies a hard outer deadline covering both the context-aware Git probe and
// the affirmative metadata lookup needed to distinguish an absent `.git` entry
// from an unreadable one. A missing or answered non-Git directory is gone; an
// unreadable path or timed-out probe is unknown and must fail closed.
func probeRepoGoneOrigin(ctx context.Context, worktree *GitWorktree) error {
	if worktree.repoPath == "" {
		return fmt.Errorf("%w: repo path is empty", ErrRepoGone)
	}
	topLevel, err := worktree.runGitCommandContext(ctx, worktree.repoPath, "rev-parse", "--show-toplevel")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if definitiveMissingRepository(err) {
			return fmt.Errorf("%w: %s: %v", ErrRepoGone, worktree.repoPath, err)
		}
		if definitiveNonGitRepository(err) {
			repoInfo, repoErr := os.Stat(worktree.repoPath)
			switch {
			case errors.Is(repoErr, os.ErrNotExist):
				return fmt.Errorf("%w: %s is no longer present: %v", ErrRepoGone, worktree.repoPath, err)
			case repoErr != nil:
				return errors.Join(err, fmt.Errorf("inspect repository root: %w", repoErr))
			case !repoInfo.IsDir():
				return fmt.Errorf("%w: %s is no longer a directory: %v", ErrRepoGone, worktree.repoPath, err)
			}
			_, metadataErr := os.Lstat(filepath.Join(worktree.repoPath, ".git"))
			switch {
			case errors.Is(metadataErr, os.ErrNotExist):
				return fmt.Errorf("%w: %s is no longer a git repository: %v", ErrRepoGone, worktree.repoPath, err)
			case metadataErr != nil:
				return errors.Join(err, fmt.Errorf("inspect repository metadata: %w", metadataErr))
			}
		}
		return err
	}
	recorded, err := os.Stat(worktree.repoPath)
	if err != nil {
		return fmt.Errorf("inspect recorded repository root: %w", err)
	}
	resolvedPath := strings.TrimSuffix(topLevel, "\n")
	resolved, err := os.Stat(resolvedPath)
	if err != nil {
		return fmt.Errorf("inspect Git repository root %s: %w", resolvedPath, err)
	}
	if !os.SameFile(recorded, resolved) {
		return fmt.Errorf(
			"%w: recorded origin %s resolves through ancestor repository %s",
			ErrRepoGone, worktree.repoPath, resolvedPath,
		)
	}
	return nil
}

// definitiveMissingRepository recognizes only Git's C-locale failure to enter
// the requested root. Permission, safe-directory, command-start and every other
// operational error remain unknown and cannot authorize deletion.
func definitiveMissingRepository(probeErr error) bool {
	var exitErr *exec.ExitError
	if !errors.As(probeErr, &exitErr) {
		return false
	}
	diagnostic := strings.TrimSpace(string(exitErr.Stderr))
	return strings.Contains(diagnostic, "cannot change to") &&
		(strings.HasSuffix(diagnostic, ": No such file or directory") ||
			strings.HasSuffix(diagnostic, ": Not a directory"))
}

// definitiveNonGitRepository accepts only Git's stable outside-repository
// answer. The caller separately proves that the recorded root has no `.git`
// entry; the same Git diagnostic is also emitted for unreadable metadata and
// therefore is not sufficient deletion authority by itself.
func definitiveNonGitRepository(probeErr error) bool {
	var exitErr *exec.ExitError
	if !errors.As(probeErr, &exitErr) ||
		!strings.Contains(string(exitErr.Stderr), "not a git repository (or any of the parent directories)") ||
		os.Getenv("GIT_DIR") != "" {
		return false
	}
	return true
}

type repoGoneOriginProbeFlight struct {
	done   chan struct{}
	err    error
	cancel context.CancelFunc
}

var repoGoneOriginProbeFlights = struct {
	sync.Mutex
	byPath map[string]*repoGoneOriginProbeFlight
}{byPath: make(map[string]*repoGoneOriginProbeFlight)}

func boundedRepoGoneOriginProbe(worktree *GitWorktree) error {
	repoGoneOriginProbeFlights.Lock()
	if repoGoneOriginProbeFlights.byPath[worktree.repoPath] != nil {
		repoGoneOriginProbeFlights.Unlock()
		return fmt.Errorf(
			"origin repository check for %s is still running after an earlier deadline: %w",
			worktree.repoPath, context.DeadlineExceeded,
		)
	}
	ctx, cancel := context.WithCancel(context.Background())
	flight := &repoGoneOriginProbeFlight{done: make(chan struct{}), cancel: cancel}
	repoGoneOriginProbeFlights.byPath[worktree.repoPath] = flight
	repoGoneOriginProbeFlights.Unlock()
	go func() {
		flight.err = repoGoneOriginProbe(ctx, worktree)
		close(flight.done)
		cancel()
		repoGoneOriginProbeFlights.Lock()
		if repoGoneOriginProbeFlights.byPath[worktree.repoPath] == flight {
			delete(repoGoneOriginProbeFlights.byPath, worktree.repoPath)
		}
		repoGoneOriginProbeFlights.Unlock()
	}()
	timer := time.NewTimer(relocationIdentityTimeout)
	defer timer.Stop()
	select {
	case <-flight.done:
		return flight.err
	case <-timer.C:
		flight.cancel()
		return fmt.Errorf(
			"timed out after %s while checking origin repository %s: %w",
			relocationIdentityTimeout, worktree.repoPath, context.DeadlineExceeded,
		)
	}
}

type cleanupPathInspection struct {
	identity   pathIdentity
	generation string
}

func inspectRelocationCleanupIdentity(path string) (cleanupPathInspection, error) {
	base, err := relocationPathIdentity(path)
	if err != nil {
		return cleanupPathInspection{}, err
	}
	parentPath := filepath.Dir(path)
	name := filepath.Base(path)
	parent, _, err := openDirectoryPathFollowingLinks(parentPath, "cleanup identity parent")
	if err != nil {
		return cleanupPathInspection{}, err
	}
	defer parent.Close()
	directory, _, err := openDirectoryAt(parent, name, path, "cleanup identity root")
	if err != nil {
		return cleanupPathInspection{}, err
	}
	defer directory.Close()
	opened, err := identityFromFile(directory)
	if err != nil {
		return cleanupPathInspection{}, err
	}
	named, err := identityAt(parent, name)
	if err != nil || !base.same(opened) || !base.same(named) {
		return cleanupPathInspection{}, unverifiedCleanupError(
			"cleanup path %s changed while its durable generation was inspected", path,
		)
	}
	generation, err := cleanupGenerationFromFile(directory)
	if err != nil {
		return cleanupPathInspection{}, err
	}
	return cleanupPathInspection{identity: opened, generation: generation}, nil
}

func boundedRelocationCleanupIdentity(path string) (cleanupPathInspection, error) {
	type result struct {
		inspection cleanupPathInspection
		err        error
	}
	resultC := make(chan result, 1)
	go func() {
		inspection, err := inspectRelocationCleanupIdentity(path)
		resultC <- result{inspection: inspection, err: err}
	}()
	timer := time.NewTimer(relocationIdentityTimeout)
	defer timer.Stop()
	select {
	case observed := <-resultC:
		return observed.inspection, observed.err
	case <-timer.C:
		return cleanupPathInspection{}, fmt.Errorf(
			"timed out after %s while checking cleanup identity at %s: %w",
			relocationIdentityTimeout, path, context.DeadlineExceeded,
		)
	}
}

type cleanupGenerationInstallFlight struct {
	done       chan struct{}
	generation string
	err        error
}

var cleanupGenerationInstallFlights = struct {
	sync.Mutex
	byPath map[string]*cleanupGenerationInstallFlight
}{byPath: make(map[string]*cleanupGenerationInstallFlight)}

func boundedCleanupGenerationInstall(path string, expected pathIdentity) (string, error) {
	cleanupGenerationInstallFlights.Lock()
	if cleanupGenerationInstallFlights.byPath[path] != nil {
		cleanupGenerationInstallFlights.Unlock()
		return "", fmt.Errorf(
			"cleanup generation installation for %s is still running after an earlier deadline: %w",
			path, context.DeadlineExceeded,
		)
	}
	flight := &cleanupGenerationInstallFlight{done: make(chan struct{})}
	cleanupGenerationInstallFlights.byPath[path] = flight
	cleanupGenerationInstallFlights.Unlock()
	go func() {
		flight.generation, flight.err = cleanupGenerationInstall(path, expected)
		close(flight.done)
		cleanupGenerationInstallFlights.Lock()
		if cleanupGenerationInstallFlights.byPath[path] == flight {
			delete(cleanupGenerationInstallFlights.byPath, path)
		}
		cleanupGenerationInstallFlights.Unlock()
	}()
	timer := time.NewTimer(relocationIdentityTimeout)
	defer timer.Stop()
	select {
	case <-flight.done:
		return flight.generation, flight.err
	case <-timer.C:
		return "", fmt.Errorf(
			"timed out after %s while installing cleanup generation at %s: %w",
			relocationIdentityTimeout, path, context.DeadlineExceeded,
		)
	}
}

func installCleanupGeneration(path string, expected pathIdentity) (string, error) {
	parentPath := filepath.Dir(path)
	name := filepath.Base(path)
	parent, _, err := openDirectoryPathFollowingLinks(parentPath, "cleanup generation parent")
	if err != nil {
		return "", err
	}
	defer parent.Close()
	directory, _, err := openDirectoryAt(parent, name, path, "cleanup generation root")
	if err != nil {
		return "", err
	}
	defer directory.Close()
	opened, err := identityFromFile(directory)
	if err != nil {
		return "", err
	}
	named, err := identityAt(parent, name)
	if err != nil || !expected.same(opened) || !expected.same(named) {
		return "", unverifiedCleanupError("cleanup path %s changed before its generation was installed", path)
	}
	if generation, err := cleanupGenerationFromFile(directory); err == nil {
		if err := directory.Sync(); err != nil {
			return "", fmt.Errorf("make existing cleanup identity durable on %s: %w", path, err)
		}
		return generation, nil
	} else if !isXattrVanished(err) {
		return "", fmt.Errorf("inspect existing cleanup identity on %s: %w", path, err)
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate cleanup identity for %s: %w", path, err)
	}
	generation := hex.EncodeToString(random)
	if err := unix.Fsetxattr(int(directory.Fd()), cleanupGenerationXattr, []byte(generation), unix.XATTR_CREATE); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return "", fmt.Errorf("store cleanup identity on %s: %w", path, err)
		}
	}
	generation, err = cleanupGenerationFromFile(directory)
	if err != nil {
		return "", fmt.Errorf("verify stored cleanup identity on %s: %w", path, err)
	}
	if err := directory.Sync(); err != nil {
		return "", fmt.Errorf("make cleanup identity durable on %s: %w", path, err)
	}
	return generation, nil
}

func cleanupGenerationFromFile(directory *os.File) (string, error) {
	size, err := unix.Fgetxattr(int(directory.Fd()), cleanupGenerationXattr, nil)
	if err != nil {
		return "", fmt.Errorf("read durable cleanup generation: %w", err)
	}
	if size <= 0 || size > 128 {
		return "", fmt.Errorf("durable cleanup generation has invalid size %d", size)
	}
	value := make([]byte, size)
	read, err := unix.Fgetxattr(int(directory.Fd()), cleanupGenerationXattr, value)
	if err != nil {
		return "", fmt.Errorf("read durable cleanup generation: %w", err)
	}
	return string(value[:read]), nil
}

func validatedCleanupPathIdentity(path, expectedGeneration string) (pathIdentity, error) {
	if expectedGeneration == "" {
		return pathIdentity{}, fmt.Errorf("cleanup record has no durable directory generation")
	}
	inspection, err := boundedRelocationCleanupIdentity(path)
	if err != nil {
		return pathIdentity{}, err
	}
	if inspection.generation != expectedGeneration {
		return pathIdentity{}, unverifiedCleanupError(
			"cleanup path %s has a different durable directory generation", path,
		)
	}
	return inspection.identity, nil
}

func requireCleanupPathIdentity(path string, expected pathIdentity, expectedGeneration string) error {
	identity, err := validatedCleanupPathIdentity(path, expectedGeneration)
	if err != nil {
		return err
	}
	if !expected.same(identity) {
		return unverifiedCleanupError("cleanup path %s has a different filesystem identity", path)
	}
	return nil
}

// CheckRepoPresentForRelocation applies the same bounded, fail-closed
// repository-validity rule used by cleanup admission. Nil means a valid origin,
// ErrRepoGone means missing or conclusively non-Git, and every other error is an
// unknown operational result which must retain the archive.
func CheckRepoPresentForRelocation(repoPath string) error {
	return boundedRepoGoneOriginProbe(&GitWorktree{repoPath: repoPath})
}

// removeClaimedRepoGoneDirectory removes only the directory identified by the
// recovery claim. The parent and root are opened before the recursive walk, so a
// same-UID rename cannot redirect the walk to a replacement pathname. Every
// descendant and the final root entry use the copy mover's claim-rename-verify-
// unlink primitive; a changed entry is retained rather than reinterpreted.
func removeClaimedRepoGoneDirectory(
	path string,
	expected pathIdentity,
	expectedGeneration string,
	beforeRootRemoval func(string) error,
) error {
	parentPath := filepath.Dir(path)
	name := filepath.Base(path)
	parent, _, err := openDirectoryPathFollowingLinks(parentPath, "repo-gone cleanup parent")
	if err != nil {
		return err
	}
	defer parent.Close()

	directory, _, err := openDirectoryAt(parent, name, path, "repo-gone cleanup root")
	if err != nil {
		return err
	}
	defer directory.Close()
	openedIdentity, err := identityFromFile(directory)
	if err != nil {
		return fmt.Errorf("inspect opened repo-gone cleanup root %s: %w", path, err)
	}
	namedIdentity, err := identityAt(parent, name)
	if err != nil || !expected.same(openedIdentity) || !expected.same(namedIdentity) {
		return unverifiedCleanupError("refusing to remove repo-gone worktree %s because its claimed identity changed", path)
	}
	generation, err := cleanupGenerationFromFile(directory)
	if err != nil || generation != expectedGeneration || expectedGeneration == "" {
		return unverifiedCleanupError("refusing to remove repo-gone worktree %s because its durable generation changed", path)
	}

	repoGoneBeforeWriterReap(path)
	reapClaimedWorktreeWriters(path, expected)
	repoGoneBeforeRecursiveDelete(path)
	manifest, err := snapshotCopiedTree(directory, path)
	if err != nil {
		return recheckCleanupRoot(parent, name, path, expected, err)
	}
	if err := removeCopiedTree(directory, path, manifest, true); err != nil {
		return recheckCleanupRoot(parent, name, path, expected, err)
	}
	return removeCopiedEntryRestoringNameWithCheckpoint(parent, parentPath, copiedEntry{
		name: name, source: expected, directory: &copiedDirectory{},
	}, true, beforeRootRemoval)
}

// removeFinalizingRepoGoneRoot consumes only the empty root marker described by
// cleanup_finalizing. The descriptor walk had already removed and verified all
// children before that state was persisted. If the name is now absent, the
// original empty marker is gone. A replacement, changed generation, or surviving
// exact marker which has been repopulated remains an unresolved cleanup obligation.
func removeFinalizingRepoGoneRoot(
	path string,
	expected pathIdentity,
	expectedGeneration string,
	beforeRootRemoval func(string) error,
) error {
	parentPath := filepath.Dir(path)
	name := filepath.Base(path)
	parent, _, err := openDirectoryPathFollowingLinks(parentPath, "repo-gone finalization parent")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer parent.Close()

	identity, err := identityAt(parent, name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !expected.same(identity) {
		return unverifiedCleanupError("finalizing root %s no longer has its claimed identity", path)
	}
	directory, _, err := openDirectoryAt(parent, name, path, "repo-gone finalization root")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	generation, generationErr := cleanupGenerationFromFile(directory)
	if generationErr != nil {
		_ = directory.Close()
		return generationErr
	}
	if generation != expectedGeneration || expectedGeneration == "" {
		_ = directory.Close()
		return unverifiedCleanupError("finalizing root %s no longer has its claimed generation", path)
	}
	names, err := directoryNames(directory, path)
	_ = directory.Close()
	if err != nil {
		return err
	}
	if len(names) != 0 {
		return unverifiedCleanupError(
			"refusing to finish repo-gone cleanup because finalizing root %s was repopulated", path,
		)
	}
	err = removeCopiedEntryRestoringNameWithCheckpoint(parent, parentPath, copiedEntry{
		name: name, source: expected, directory: &copiedDirectory{},
	}, true, beforeRootRemoval)
	return err
}

// CleanupClaimedRepoGone removes an archived worktree using the cleanup-only
// claim authorized before the kill's durable commit. The claim is checked after
// pane teardown, writers are reaped before deletion, and its identity stays
// claimed through an identity-anchored recursive delete. The origin is
// deliberately not probed again here: its absence was the pre-commit admission
// condition, while a returned origin is a separate directory that must not be
// able to strand an already committed kill.
func (g *GitWorktree) CleanupClaimedRepoGone(claim RelocationClaim) (CleanupState, error) {
	state, err, _ := g.cleanupClaimedRepoGone(claim)
	return state, err
}

// CleanupClaimedRepoGoneWithLateResult is the ghost-cleanup form. On a caller
// deadline, lateResult reports the descriptor worker's eventual result so the
// daemon can reconcile the persisted row instead of leaving a completed delete
// fenced forever. A nil channel means no worker outlived the call.
func (g *GitWorktree) CleanupClaimedRepoGoneWithLateResult(claim RelocationClaim) (CleanupState, error, <-chan error) {
	return g.cleanupClaimedRepoGone(claim)
}

func (g *GitWorktree) cleanupClaimedRepoGone(claim RelocationClaim) (CleanupState, error, <-chan error) {
	completed := false
	defer func() {
		if !completed {
			g.PreserveRelocationClaim(claim)
		}
	}()
	checkpoint := g.repoGoneFinalizationCheckpointSnapshot()
	if claim.cleanupFinalizing {
		if !claim.cleanupRootGone {
			if err := removeFinalizingRepoGoneRoot(
				claim.Path, claim.identity, claim.cleanupGeneration,
				func(securedPath string) error {
					return g.checkpointRepoGoneFinalization(claim, securedPath, checkpoint)
				},
			); err != nil {
				return CleanupStateUnknown, fmt.Errorf(
					"finish repo-gone cleanup root %s: %w", claim.Path, err,
				), nil
			}
			if err := g.completeRemovedRelocationClaim(claim); err != nil {
				return CleanupStateUnknown, err, nil
			}
		} else if err := g.completeRepoGoneFinalization(claim); err != nil {
			return CleanupStateUnknown, err, nil
		}
		completed = true
		return CleanupSettled, nil, nil
	}

	if err := g.RevalidateRelocationClaim(claim); err != nil {
		return CleanupStateUnknown, fmt.Errorf("cannot authorize repo-gone cleanup: %w", err), nil
	}
	if g.hooksCancel != nil {
		g.hooksCancel()
	}
	deleteDone := make(chan error, 1)
	go func() {
		// Current archives may carry complete retained source trees for files the
		// published copy could not read. Consume those handles and the primary
		// archive inside one deadline-bound transaction.
		err := repoGoneCleanupRetainedTrees(g)
		if err != nil {
			deleteDone <- fmt.Errorf("remove retained archive trees before repo-gone cleanup: %w", err)
			return
		}
		if err := g.RevalidateRelocationClaim(claim); err != nil {
			deleteDone <- fmt.Errorf("repo-gone cleanup identity changed before reaping writers: %w", err)
			return
		}
		err = repoGoneRemoveDirectory(claim.Path, claim.identity, claim.cleanupGeneration, func(securedPath string) error {
			return g.checkpointRepoGoneFinalization(claim, securedPath, checkpoint)
		})
		if err == nil {
			err = g.completeRemovedRelocationClaim(claim)
		}
		deleteDone <- err
	}()
	timer := time.NewTimer(repoGoneCleanupTimeout)
	defer timer.Stop()
	select {
	case err := <-deleteDone:
		if err != nil {
			revalidationErr := g.RevalidateRelocationClaim(claim)
			return CleanupStateUnknown, errors.Join(
				fmt.Errorf("failed to remove repo-gone worktree %s: %w", claim.Path, err),
				revalidationErr,
			), nil
		}
	case <-timer.C:
		// The worker may be stuck in an uninterruptible filesystem syscall and
		// cannot safely be killed. Its opened handles identify only the claimed
		// tree, so it may finish in the background; the caller still returns and
		// the durable record remains the retry/manual-recovery handle.
		g.markCleanupStalledWithClaim(&claim)
		completed = true
		return CleanupStateUnknown, fmt.Errorf(
			"repo-gone recursive deletion of %s did not finish within %s: %w",
			claim.Path, repoGoneCleanupTimeout, context.DeadlineExceeded,
		), deleteDone
	}
	completed = true
	return CleanupSettled, nil, nil
}

func reapClaimedWorktreeWriters(path string, expected pathIdentity) {
	repoGoneReapMatching(path, func(pid int) bool {
		cwd, _, ok := repoGoneOpenWorkingDir(pid)
		if !ok {
			return false
		}
		defer cwd.Close()
		return openedDirectoryAtOrUnder(cwd, expected)
	})
}

func openedDirectoryAtOrUnder(directory *os.File, expected pathIdentity) bool {
	current, _, err := openDirectoryAt(directory, ".", "", "process working")
	if err != nil {
		return false
	}
	for {
		identity, err := identityFromFile(current)
		if err != nil {
			_ = current.Close()
			return false
		}
		if expected.same(identity) {
			_ = current.Close()
			return true
		}
		parent, _, err := openDirectoryAt(current, "..", "", "process working parent")
		if err != nil {
			_ = current.Close()
			return false
		}
		parentIdentity, err := identityFromFile(parent)
		_ = current.Close()
		if err != nil {
			_ = parent.Close()
			return false
		}
		if identity.same(parentIdentity) {
			_ = parent.Close()
			return false
		}
		current = parent
	}
}
