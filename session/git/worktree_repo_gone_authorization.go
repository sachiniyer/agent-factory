package git

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
var cleanupGenerationRead = unix.Fgetxattr

// probeRepoGoneOrigin applies restore's repository-validity rule. Its caller
// supplies a hard outer deadline covering both the context-aware Git probe and
// the affirmative metadata lookup needed to distinguish an absent `.git` entry
// from an unreadable one. A missing or answered non-Git directory is gone; an
// unreadable path or timed-out probe is unknown and must fail closed.
func probeRepoGoneOrigin(ctx context.Context, worktree *GitWorktree) error {
	if worktree.repoPath == "" {
		return fmt.Errorf("%w: repo path is empty", ErrRepoGone)
	}
	commandEnv := repoGoneGitCommandEnvironment()
	topLevel, err := worktree.runGitCommandContextWithEnvironment(
		ctx, worktree.repoPath, commandEnv, "rev-parse", "--show-toplevel",
	)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if definitiveMissingRepository(err) {
			return fmt.Errorf("%w: %s: %v", ErrRepoGone, worktree.repoPath, err)
		}
		if definitiveNonGitRepository(err, commandEnv) {
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
func definitiveNonGitRepository(probeErr error, commandEnv []string) bool {
	var exitErr *exec.ExitError
	if !errors.As(probeErr, &exitErr) ||
		!strings.Contains(string(exitErr.Stderr), "not a git repository (or any of the parent directories)") ||
		environmentContainsName(commandEnv, "GIT_DIR") {
		return false
	}
	return true
}

func environmentContainsName(environment []string, wanted string) bool {
	for _, entry := range environment {
		name, _, ok := strings.Cut(entry, "=")
		if ok && name == wanted {
			return true
		}
	}
	return false
}

var repoGoneOriginProbe = probeRepoGoneOrigin
var repoGoneOriginProbeAfterUnpublish = func() {}

type repoGoneOriginProbeFlight struct {
	done           chan struct{}
	err            error
	cancel         context.CancelFunc
	afterUnpublish func()
	// timedOut distinguishes a healthy shared in-flight probe from a worker
	// which already exceeded an outer deadline and remains fenced until exit.
	timedOut bool
	waiters  int
}

var repoGoneOriginProbeFlights = struct {
	sync.Mutex
	byPath map[string]*repoGoneOriginProbeFlight
}{byPath: make(map[string]*repoGoneOriginProbeFlight)}

func boundedRepoGoneOriginProbe(worktree *GitWorktree) error {
	repoGoneOriginProbeFlights.Lock()
	if active := repoGoneOriginProbeFlights.byPath[worktree.repoPath]; active != nil {
		if !active.timedOut {
			active.waiters++
			repoGoneOriginProbeFlights.Unlock()
			return waitForRepoGoneOriginProbe(worktree.repoPath, active)
		}
		repoGoneOriginProbeFlights.Unlock()
		return fmt.Errorf(
			"origin repository check for %s is still running after an earlier deadline: %w",
			worktree.repoPath, context.DeadlineExceeded,
		)
	}
	ctx, cancel := context.WithCancel(context.Background())
	flight := &repoGoneOriginProbeFlight{
		done: make(chan struct{}), cancel: cancel, waiters: 1,
		afterUnpublish: repoGoneOriginProbeAfterUnpublish,
	}
	repoGoneOriginProbeFlights.byPath[worktree.repoPath] = flight
	repoGoneOriginProbeFlights.Unlock()
	go func() {
		flight.err = repoGoneOriginProbe(ctx, worktree)
		cancel()
		repoGoneOriginProbeFlights.Lock()
		if repoGoneOriginProbeFlights.byPath[worktree.repoPath] == flight {
			delete(repoGoneOriginProbeFlights.byPath, worktree.repoPath)
		}
		close(flight.done)
		repoGoneOriginProbeFlights.Unlock()
		flight.afterUnpublish()
	}()
	return waitForRepoGoneOriginProbe(worktree.repoPath, flight)
}

func waitForRepoGoneOriginProbe(repoPath string, flight *repoGoneOriginProbeFlight) error {
	timer := time.NewTimer(relocationIdentityTimeout)
	defer timer.Stop()
	select {
	case <-flight.done:
		return flight.err
	case <-timer.C:
		repoGoneOriginProbeFlights.Lock()
		if repoGoneOriginProbeFlights.byPath[repoPath] == flight {
			flight.timedOut = true
			flight.cancel()
			repoGoneOriginProbeFlights.Unlock()
			return fmt.Errorf(
				"timed out after %s while checking origin repository %s: %w",
				relocationIdentityTimeout, repoPath, context.DeadlineExceeded,
			)
		}
		repoGoneOriginProbeFlights.Unlock()
		<-flight.done
		return flight.err
	}
}

type cleanupPathInspection struct {
	identity   pathIdentity
	generation string
	empty      bool
}

func inspectRelocationCleanupIdentity(path string) (cleanupPathInspection, error) {
	return inspectRelocationCleanupPath(path, false)
}

func inspectFinalizingCleanupIdentity(path string) (cleanupPathInspection, error) {
	return inspectRelocationCleanupPath(path, true)
}

func inspectRelocationCleanupPath(path string, inspectEmpty bool) (cleanupPathInspection, error) {
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
	empty := false
	if inspectEmpty {
		names, err := directoryNames(directory, path)
		if err != nil {
			return cleanupPathInspection{}, err
		}
		empty = len(names) == 0
	}
	return cleanupPathInspection{identity: opened, generation: generation, empty: empty}, nil
}

func boundedRelocationCleanupIdentity(path string) (cleanupPathInspection, error) {
	return boundedRelocationCleanupInspection(path, inspectRelocationCleanupIdentity)
}

func boundedFinalizingCleanupIdentity(path string) (cleanupPathInspection, error) {
	return boundedRelocationCleanupInspection(path, inspectFinalizingCleanupIdentity)
}

func boundedRelocationCleanupInspection(
	path string,
	inspect func(string) (cleanupPathInspection, error),
) (cleanupPathInspection, error) {
	type result struct {
		inspection cleanupPathInspection
		err        error
	}
	resultC := make(chan result, 1)
	go func() {
		inspection, err := inspect(path)
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
	done           chan struct{}
	generation     string
	err            error
	afterUnpublish func()
}

var cleanupGenerationInstallFlights = struct {
	sync.Mutex
	byPath map[string]*cleanupGenerationInstallFlight
}{byPath: make(map[string]*cleanupGenerationInstallFlight)}

var cleanupGenerationInstallAfterUnpublish = func() {}

func boundedCleanupGenerationInstall(path string, expected pathIdentity) (string, error) {
	cleanupGenerationInstallFlights.Lock()
	if cleanupGenerationInstallFlights.byPath[path] != nil {
		cleanupGenerationInstallFlights.Unlock()
		return "", fmt.Errorf(
			"cleanup generation installation for %s is still running after an earlier deadline: %w",
			path, context.DeadlineExceeded,
		)
	}
	flight := &cleanupGenerationInstallFlight{
		done: make(chan struct{}), afterUnpublish: cleanupGenerationInstallAfterUnpublish,
	}
	cleanupGenerationInstallFlights.byPath[path] = flight
	cleanupGenerationInstallFlights.Unlock()
	go func() {
		flight.generation, flight.err = cleanupGenerationInstall(path, expected)
		cleanupGenerationInstallFlights.Lock()
		if cleanupGenerationInstallFlights.byPath[path] == flight {
			delete(cleanupGenerationInstallFlights.byPath, path)
		}
		close(flight.done)
		cleanupGenerationInstallFlights.Unlock()
		flight.afterUnpublish()
	}()
	timer := time.NewTimer(relocationIdentityTimeout)
	defer timer.Stop()
	select {
	case <-flight.done:
		return flight.generation, flight.err
	case <-timer.C:
		cleanupGenerationInstallFlights.Lock()
		if cleanupGenerationInstallFlights.byPath[path] != flight {
			cleanupGenerationInstallFlights.Unlock()
			<-flight.done
			return flight.generation, flight.err
		}
		cleanupGenerationInstallFlights.Unlock()
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
	if err := setCleanupGenerationCreate(int(directory.Fd()), cleanupGenerationXattr, []byte(generation)); err != nil {
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
	size, err := cleanupGenerationRead(int(directory.Fd()), cleanupGenerationXattr, nil)
	if err != nil {
		return "", fmt.Errorf("read durable cleanup generation: %w", err)
	}
	if size <= 0 || size > 128 {
		return "", fmt.Errorf("durable cleanup generation has invalid size %d", size)
	}
	value := make([]byte, size)
	read, err := cleanupGenerationRead(int(directory.Fd()), cleanupGenerationXattr, value)
	if err != nil {
		return "", fmt.Errorf("read durable cleanup generation: %w", err)
	}
	if read != size || read <= 0 {
		return "", fmt.Errorf(
			"durable cleanup generation changed size while it was read: queried %d bytes, read %d",
			size, read,
		)
	}
	if !validCleanupGeneration(value[:read]) {
		return "", fmt.Errorf("durable cleanup generation is not a 32-character lowercase hexadecimal value")
	}
	return string(value[:read]), nil
}

func validCleanupGeneration(value []byte) bool {
	if len(value) != 32 {
		return false
	}
	for _, digit := range value {
		if (digit < '0' || digit > '9') && (digit < 'a' || digit > 'f') {
			return false
		}
	}
	return true
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
// repository-validity rule used when establishing cleanup authorization. Nil
// means a valid origin, ErrRepoGone means missing or conclusively non-Git, and
// every other error is unknown and must retain the archive.
func CheckRepoPresentForRelocation(repoPath string) error {
	return boundedRepoGoneOriginProbe(&GitWorktree{repoPath: repoPath})
}

// archivedWorktreePointerMaxSize bounds the .git pointer read. A linked
// worktree's pointer file is one short line; anything larger is not one.
const archivedWorktreePointerMaxSize = 1 << 16

// VerifyArchivedWorktreePointer checks that the directory occupying
// worktreePath carries a linked-worktree `.git` pointer file — the identity
// evidence the archive itself wrote at creation time and both archive move
// variants preserve. A record-free direct kill authorizes deletion from the
// current pathname occupant, so this is what separates the archived worktree
// from an unrelated directory later created at the same path (#3278 review).
//
// The pointer's repository half is deliberately not compared against the
// recorded origin: the origin is conclusively gone when this runs, so its
// canonical form cannot be resolved, and the recorded path and git's stored
// gitdir may differ only by symlink resolution (macOS TMPDIR). The check binds
// to the pointer's structure — a gitdir line naming a `.git/worktrees/<name>`
// metadata directory — which an unrelated replacement directory does not have.
// Absence, unreadability, or any other shape refuses: deletion authority must
// not fall back to pathname trust.
//
// Bounded like every other filesystem probe on this path (#3278 review): the
// caller holds the session operation lock, and a stalled FUSE/NFS mount would
// otherwise hang the kill on a plain open, fstat, read, or lstat. A tripped
// deadline is an unknown, fail-closed answer — and it latches: a retry while
// the stuck worker is still in flight is refused instead of stacking another
// goroutine and descriptor against the same stalled path, mirroring
// boundedRepoGoneOriginProbe's per-path flight.
func VerifyArchivedWorktreePointer(worktreePath string) error {
	archivedPointerFlights.Lock()
	if active := archivedPointerFlights.byPath[worktreePath]; active != nil {
		if active.timedOut {
			archivedPointerFlights.Unlock()
			return fmt.Errorf(
				"archived worktree pointer check under %s is still running after an earlier deadline: %w",
				worktreePath, context.DeadlineExceeded,
			)
		}
		archivedPointerFlights.Unlock()
		return waitForArchivedPointerFlight(worktreePath, active)
	}
	flight := &archivedPointerFlight{done: make(chan struct{})}
	archivedPointerFlights.byPath[worktreePath] = flight
	archivedPointerFlights.Unlock()
	go func() {
		flight.err = verifyArchivedWorktreePointer(worktreePath)
		archivedPointerFlights.Lock()
		if archivedPointerFlights.byPath[worktreePath] == flight {
			delete(archivedPointerFlights.byPath, worktreePath)
		}
		close(flight.done)
		archivedPointerFlights.Unlock()
	}()
	return waitForArchivedPointerFlight(worktreePath, flight)
}

// archivedPointerFlight dedupes concurrent pointer verifications of one path
// and latches after a deadline so retries cannot accumulate workers against a
// stalled mount. No cancel: the worker's raw syscalls are not context-aware,
// so the latch — refuse new callers until the stuck worker finally returns
// and unpublishes itself — is the containment.
type archivedPointerFlight struct {
	done     chan struct{}
	err      error
	timedOut bool
}

var archivedPointerFlights = struct {
	sync.Mutex
	byPath map[string]*archivedPointerFlight
}{byPath: map[string]*archivedPointerFlight{}}

func waitForArchivedPointerFlight(worktreePath string, flight *archivedPointerFlight) error {
	timer := time.NewTimer(relocationIdentityTimeout)
	defer timer.Stop()
	select {
	case <-flight.done:
		return flight.err
	case <-timer.C:
		archivedPointerFlights.Lock()
		if archivedPointerFlights.byPath[worktreePath] == flight {
			flight.timedOut = true
			archivedPointerFlights.Unlock()
			return fmt.Errorf(
				"timed out after %s while verifying archived worktree pointer under %s: %w",
				relocationIdentityTimeout, worktreePath, context.DeadlineExceeded,
			)
		}
		archivedPointerFlights.Unlock()
		<-flight.done
		return flight.err
	}
}

func verifyArchivedWorktreePointer(worktreePath string) error {
	pointerPath := filepath.Join(worktreePath, ".git")
	// One descriptor, opened without following links, carries every check and
	// the read (#3278 review): a separate stat-then-read pair could be raced
	// by a same-UID process swapping in a symlink or a special file between
	// the two lookups, and an unbounded read of a swapped-in file could stall
	// or exhaust memory on the kill path. O_NONBLOCK keeps the open itself
	// from blocking on a FIFO; the fstat below rejects every non-regular file
	// before any read, and regular-file reads ignore the flag.
	f, err := os.OpenFile(pointerPath, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("archived worktree pointer %s could not be opened without following links: %w", pointerPath, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("archived worktree pointer %s could not be inspected: %w", pointerPath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("archived worktree pointer %s is not a regular file", pointerPath)
	}
	content, err := io.ReadAll(io.LimitReader(f, archivedWorktreePointerMaxSize+1))
	if err != nil {
		return fmt.Errorf("archived worktree pointer %s could not be read: %w", pointerPath, err)
	}
	if len(content) > archivedWorktreePointerMaxSize {
		return fmt.Errorf("archived worktree pointer %s is too large to be a worktree pointer", pointerPath)
	}
	line, _, _ := strings.Cut(string(content), "\n")
	target, ok := strings.CutPrefix(line, "gitdir:")
	if !ok {
		return fmt.Errorf("archived worktree pointer %s does not begin with a gitdir line", pointerPath)
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return fmt.Errorf("archived worktree pointer %s names no gitdir", pointerPath)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(worktreePath, target)
	}
	target = filepath.Clean(target)
	name := filepath.Base(target)
	if name == "" || name == "." || name == string(filepath.Separator) ||
		filepath.Base(filepath.Dir(target)) != "worktrees" ||
		filepath.Base(filepath.Dir(filepath.Dir(target))) != ".git" {
		return fmt.Errorf(
			"archived worktree pointer %s names gitdir %s, which is not a linked-worktree metadata directory",
			pointerPath, target,
		)
	}
	// A gitdir that still exists cannot belong to the gone origin: when the
	// origin probed conclusively absent or non-Git, its .git/worktrees metadata
	// went with it — recorded and resolved forms of the same directory vanish
	// together, symlinked prefixes included. A live target therefore means the
	// pathname occupant is a worktree of some OTHER, still-present repository,
	// and deleting it would corrupt that repository's checkout (#3278 review).
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf(
			"archived worktree pointer %s names gitdir %s, which still exists — the directory belongs to a live repository, not the gone origin",
			pointerPath, target,
		)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf(
			"archived worktree pointer %s: could not establish the state of gitdir %s: %w",
			pointerPath, target, err,
		)
	}
	return nil
}
