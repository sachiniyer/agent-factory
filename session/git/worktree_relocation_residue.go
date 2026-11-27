// Residue detection for the record-free crash window of a cross-device worktree
// relocate. The secure move layer (worktree_dir_move.go) commits a cross-device
// move as two renames — quarantine the source to an unguessable
// `.af-source-<32hex>` sibling, then publish a staged copy at the destination
// from a `.af-copy-<32hex>` sibling — and the relocation recovery record is
// only materialized after the publish rename succeeds. A daemon death between
// the two renames leaves the complete worktree (including uncommitted changes)
// stranded at the `.af-source-<32hex>` sibling and a complete copy at the
// `.af-copy-<32hex>` sibling, with the recorded path absent, the destination
// absent, and no record naming either. This file names that footprint so the
// restart-facing archive arbitration can refuse to classify it as a deletion
// outside af instead of committing the row Archived with the worktree-missing
// stamp and silently losing the stranded work.
package git

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
)

// privateMoveResidueName reports whether name is one of the unguessable
// private-move directories af's cross-device relocate layer leaves beside its
// endpoints: the source quarantine (`.af-source-<32hex>`, written before the
// publish rename) and the staging copy (`.af-copy-<32hex>`, written beside the
// destination). The 32-hex random suffix is exactly what privateMoveName
// produces, so a non-af directory cannot match by accident.
func privateMoveResidueName(name string) bool {
	const (
		sourcePrefix = ".af-source-"
		copyPrefix   = ".af-copy-"
	)
	var random string
	switch {
	case strings.HasPrefix(name, sourcePrefix):
		random = strings.TrimPrefix(name, sourcePrefix)
	case strings.HasPrefix(name, copyPrefix):
		random = strings.TrimPrefix(name, copyPrefix)
	default:
		return false
	}
	if len(random) != 32 {
		return false
	}
	_, err := hex.DecodeString(random)
	return err == nil
}

// ListAFRelocationResidue lists af private-move residue directories present in
// the given directory parents: `.af-source-<32hex>` and `.af-copy-<32hex>`
// siblings af's cross-device relocate layer leaves beside its source and
// destination when it is cut off between the quarantine rename and the publish
// rename. A record-free crash in that window strands the complete worktree
// (with any uncommitted changes) under one of these with no relocation record
// naming it, so a restart-facing caller uses a non-empty result to refuse to
// classify the absent recorded path as a deletion outside af instead of
// archiving over the stranded work.
//
// An absent directory contributes no residue; an unreadable directory returns
// an error so the caller can refuse rather than guess a deletion from a scan
// that could not be completed.
func ListAFRelocationResidue(dirs ...string) ([]string, error) {
	var residue []string
	for _, dir := range dirs {
		entries, err := BoundedReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				// ENOENT from a path containing a dangling symlink is
				// indistinguishable from an absent directory at the readdir
				// call, but they are not the same: the link could resolve
				// somewhere carrying residue once fixed, so "no residue" is
				// unproven. Refuse rather than classify the scan empty (R7).
				dangling, dErr := danglingSymlinkComponent(dir)
				if dErr != nil {
					return nil, fmt.Errorf("scan af relocation residue beside %s: %w", dir, dErr)
				}
				if dangling != "" {
					return nil, fmt.Errorf(
						"scan af relocation residue beside %s: %s is a dangling symlink, so the scan cannot prove the directory carries no residue",
						dir, dangling,
					)
				}
				continue
			}
			return nil, fmt.Errorf("scan af relocation residue beside %s: %w", dir, err)
		}
		for _, e := range entries {
			// Filter by the residue-shaped name before any metadata
			// syscall: the unguessable `.af-source-<32hex>`/`.af-copy-<32hex>`
			// suffix is what privateMoveName produces, so a directory af did
			// not write cannot match, and the stat below runs only on
			// candidates worth inspecting.
			if !privateMoveResidueName(e.Name()) {
				continue
			}
			// readdir reports the entry type when the filesystem fills in
			// d_type, but some NFS/FUSE filesystems return DT_UNKNOWN, on
			// which DirEntry.IsDir() returns false without statting the
			// entry. A real `.af-source-*`/`.af-copy-*` directory would then
			// be skipped and the caller would archive the session as a
			// deletion despite the stranded work. When the type is not known
			// to be a directory, stat the joined path through BoundedLstat
			// (the same deadline + per-path flight the rest of the identity
			// probe uses) rather than the entry's own Info(), whose syscall
			// is unbounded and can wedge the held op-lock if an NFS/FUSE
			// mount stalls after the bounded listing. A failed inspection is
			// propagated so the arbitration stays fail-closed rather than
			// guessing a deletion from a scan it could not complete.
			if e.IsDir() {
				residue = append(residue, filepath.Join(dir, e.Name()))
				continue
			}
			if e.Type() != 0 {
				// A known non-directory type (symlink, device, ...).
				continue
			}
			info, ierr := BoundedLstat(filepath.Join(dir, e.Name()))
			if ierr != nil {
				return nil, fmt.Errorf("scan af relocation residue beside %s: cannot inspect %s: %w", dir, e.Name(), ierr)
			}
			if !info.IsDir() {
				continue
			}
			residue = append(residue, filepath.Join(dir, e.Name()))
		}
	}
	return residue, nil
}

// danglingSymlinkComponent returns the first component of dir — from the root
// down to dir itself — that is a symlink resolving to nothing, or "" when the
// path is genuinely absent or fully resolvable. A path component that lstat
// cannot find ends the walk as absent: nothing below a missing directory can
// carry residue. Each symlink component is resolved with stat so a link that
// points at a live tree is fine — only one whose target does not exist makes
// the scan's "no residue" answer unproven. Every component syscall is bounded
// (BoundedLstat/BoundedStat): a raw os.Lstat on a component under a stalled
// NFS/FUSE mount would wedge the same op-lock the bounded scan exists to
// protect, which is exactly the path this helper walks (#7819 R10). A
// bounded-syscall timeout therefore errors the walk rather than being read
// as absence — unknown, never "no residue".
func danglingSymlinkComponent(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("residue scan cannot resolve %s: %w", dir, err)
	}
	prefix := filepath.VolumeName(abs) + string(filepath.Separator)
	for _, part := range strings.Split(abs, string(filepath.Separator)) {
		if part == "" {
			continue
		}
		prefix = filepath.Join(prefix, part)
		info, lerr := BoundedLstat(prefix)
		if lerr != nil {
			if errors.Is(lerr, os.ErrNotExist) {
				return "", nil
			}
			return "", fmt.Errorf("residue scan cannot inspect %s: %w", prefix, lerr)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if _, serr := BoundedStat(prefix); serr != nil {
			if errors.Is(serr, os.ErrNotExist) {
				return prefix, nil
			}
			return "", fmt.Errorf("residue scan cannot inspect %s: %w", prefix, serr)
		}
	}
	return "", nil
}

// ResidueBinding reports how an af relocation residue directory relates to a
// session's recorded source path, so the daemon's archive arbitration refuses
// only on this session's own stranded worktree and never on another session's
// residue that happens to share the (shared-by-every-session) source or archive
// parent.
type ResidueBinding int

const (
	// ResidueForeign is proven to belong to a different worktree — its git
	// registration backpointer names another path, or it points into a
	// different repository's metadata. It must not block this session's
	// archive.
	ResidueForeign ResidueBinding = iota
	// ResidueOurs is proven to be this session's stranded worktree: its .git
	// pointer resolves into this repository's metadata and the registration
	// backpointer there names this session's recorded source path. The daemon
	// refuses and points the operator at it.
	ResidueOurs
	// ResidueUnknown could not be read. The daemon refuses fail-closed but
	// must not claim the residue is this session's work.
	ResidueUnknown
)

// BindAFRelocationResidue reports whether the residue directory at candidate is
// this session's own stranded worktree, by reading its linked-worktree .git
// pointer and the registration backpointer git wrote for it. af's cross-device
// move is a raw rename, not `git worktree move`, so git's registration keeps
// pointing at the recorded (now-absent) source; the candidate's .git pointer
// resolves into the repo's `.git/worktrees/<name>/` leaf whose `gitdir`
// backpointer names `<source>/.git` exactly when the candidate is this
// session's stranded tree. A candidate belonging to another session (another
// session's interrupted move, or a tree another session's archive retained on
// purpose) backpoints at that other session's path and reads as foreign, so it
// does not block this archive. A candidate whose binding cannot be read is
// unknown: the caller refuses fail-closed but must not claim it is this
// session's work.
func BindAFRelocationResidue(candidate, source, repoPath string) (ResidueBinding, error) {
	encoded, err := boundedPointerCheckValue("residue-bind\x00"+source, candidate, func(path string) (string, error) {
		target, pErr := verifyResidueCandidatePointerShape(path)
		if pErr != nil {
			return "unknown", pErr
		}
		expectedRoot, rErr := resolveRepoMetadataRoot(repoPath)
		if rErr != nil {
			return "unknown", rErr
		}
		targetRoot := filepath.Dir(filepath.Dir(target))
		if pathutil.ResolveForCompare(targetRoot) != pathutil.ResolveForCompare(expectedRoot) {
			return "foreign", nil
		}
		backpointer, bErr := readWorktreeBackpointer(target)
		if bErr != nil {
			return "unknown", bErr
		}
		wantPointer := filepath.Join(source, ".git")
		if pathutil.ResolveForCompare(backpointer) != pathutil.ResolveForCompare(wantPointer) {
			return "foreign", nil
		}
		return "ours", nil
	})
	if err != nil {
		return ResidueUnknown, err
	}
	switch encoded {
	case "foreign":
		return ResidueForeign, nil
	case "ours":
		return ResidueOurs, nil
	default:
		return ResidueUnknown, nil
	}
}

// verifyResidueCandidatePointerShape verifies a residue candidate's
// linked-worktree `.git` pointer without traversing the candidate by
// pathname. The directory is opened O_NOFOLLOW — a symlink swapped in place
// of the scanned directory fails the open outright — the descriptor's fstat
// identity is compared with a fresh lstat of the pathname, and the `.git`
// pointer is read by openat relative to the held descriptor so the read
// lands in the inspected object (R6).
//
// The identity comparison covers the open-to-lstat window only: a rename
// between the caller's directory scan and this open is not detected, so the
// bind describes the pathname's current occupant. That is the semantics the
// callers want — they act on the pathname itself (refusing and naming it to
// the operator), so binding whatever sits at the candidate at bind time is
// honest; the lstat check exists so a swap under inspection cannot make the
// verdict describe an object that has since left the pathname.
func verifyResidueCandidatePointerShape(candidate string) (string, error) {
	fd, err := unix.Open(
		candidate,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0,
	)
	if err != nil {
		return "", fmt.Errorf("residue candidate %s could not be opened without following links: %w", candidate, err)
	}
	dir := os.NewFile(uintptr(fd), candidate)
	defer dir.Close()
	scanned, err := dir.Stat()
	if err != nil {
		return "", fmt.Errorf("residue candidate %s could not be inspected: %w", candidate, err)
	}
	lst, err := os.Lstat(candidate)
	if err != nil {
		return "", fmt.Errorf("residue candidate %s could not be inspected: %w", candidate, err)
	}
	if !os.SameFile(scanned, lst) {
		return "", worktreeIdentityMismatchf("residue candidate %s changed under inspection", candidate)
	}
	return verifyWorktreePointerShapeAt(dir, candidate)
}

// verifyWorktreePointerShapeAt runs the same structural check as
// verifyWorktreePointerShape with the `.git` pointer opened by openat relative
// to a held descriptor on worktreePath: the pointer is read from the object
// the descriptor names, so a pathname swap of the worktree directory cannot
// redirect the read into another tree (R6).
func verifyWorktreePointerShapeAt(dir *os.File, worktreePath string) (string, error) {
	displayPath := filepath.Join(worktreePath, ".git")
	target, err := readGitdirPointerFileAt(dir, ".git", "archived worktree pointer", worktreePath, displayPath)
	if err != nil {
		return "", err
	}
	return requireWorktreesLeafGitdir(displayPath, target)
}

// readGitdirPointerFileAt reads a `.git` redirect file relative to an already
// held directory descriptor: `name` (always ".git" today) is opened with
// openat on dir without following links, so the file read comes from the
// object the descriptor names even if the candidate's pathname was swapped
// under inspection (R6). It carries the same fstat-regular, bounded-read and
// gitdir-parse checks as readGitdirPointerFile.
func readGitdirPointerFileAt(dir *os.File, name, label, baseDir, displayPath string) (string, error) {
	fd, err := unix.Openat(
		int(dir.Fd()), name,
		unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC,
		0,
	)
	if err != nil {
		return "", fmt.Errorf("%s %s could not be opened without following links: %w", label, displayPath, err)
	}
	f := os.NewFile(uintptr(fd), displayPath)
	defer f.Close()
	line, err := readBoundedPointerLineFd(f, label, displayPath)
	if err != nil {
		return "", err
	}
	return parseGitdirPointerLine(label, displayPath, baseDir, line)
}
