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
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
		target, pErr := verifyWorktreePointerShape(path)
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
