package session

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
)

// crashWindowResidueRebuildRefusal refuses to rebuild a missing worktree when af
// private-move residue beside the recorded source path shows the worktree's
// absence is the record-free crash window of a cross-device archive move, not a
// deletion outside af. It is the lost-restore half of the #7804/#7807 contract:
// the archive endpoint already refuses over this residue on its bare-ENOENT
// route (archiveSessionSourceAbsent), but the rebuild makes that route
// unreachable by re-creating the recorded path — so the same arbitration must
// run here, before the rebuild commits, or the stranded complete worktree
// (with any uncommitted changes) is permanently orphaned under an unguessable
// `.af-source-<32hex>` name no record, log, or sweep references.
//
// It scans only the recorded source's parent directory: that is where af's
// cross-device relocate layer always strands the quarantine (the rename is
// `source → .af-source-<32hex>` sibling, before any durable record). Once the
// rebuild is refused the row stays Lost and the recorded path stays absent, so
// a later archive retry reaches the archive-side bare-ENOENT route, which scans
// BOTH parents and surfaces any `.af-copy-*` staged copy beside the destination
// as well — keeping the rescue window open without this rebuild having to know
// the archive destination.
//
// The source parent is shared by every session, so residue blocks this rebuild
// only when it is proven to be THIS session's own worktree. Another session's
// interrupted move, or a tree another session's archive retained on purpose, is
// foreign and must not block this rebuild — and the refusal must never tell the
// operator to move another session's tree into this session's path. Each
// candidate's git binding is verified against this session's recorded source
// (BindAFRelocationResidue); a candidate whose binding cannot be read still
// refuses fail-closed, but the message names it as unverified rather than this
// session's stranded work.
//
// Returns nil when no residue is present, or when only foreign residue is
// present, so a genuine deletion outside af still rebuilds from the kept branch.
func crashWindowResidueRebuildRefusal(i *Instance, gw *git.GitWorktree) error {
	source := gw.GetWorktreePath()
	if source == "" {
		// No recorded path to scan beside. The rebuild-path caller would already
		// have refused on an empty workDir, so this is unreachable in production;
		// treat as no residue rather than guessing.
		return nil
	}
	residue, resErr := git.ListAFRelocationResidue(filepath.Dir(source))
	if resErr != nil {
		return fmt.Errorf(
			"the worktree path %s is gone and af could not scan for relocation residue beside it, "+
				"so it cannot tell a deletion outside af from its own interrupted cross-device move "+
				"that may have stranded the complete worktree (with any uncommitted changes) under a .af-source-* quarantine: %w",
			source, resErr)
	}
	if len(residue) == 0 {
		return nil
	}
	repoPath := gw.GetRepoPath()
	// The source parent is shared by every session, so residue blocks this
	// rebuild only when it is proven to be THIS session's worktree. Verify each
	// candidate's git binding against this session's recorded source; a
	// candidate whose binding cannot be read still refuses (fail-closed), but
	// the message names it as unverified rather than this session's stranded
	// work.
	//
	// The proven-ours residue is split by purpose: `.af-source-*` is the renamed
	// source — the COMPLETE worktree, including any uncommitted changes — so
	// recovery is directed from it. `.af-copy-*` is a staged copy, which under
	// skipUnreadable may intentionally omit unreadable paths; it must never be
	// described as the complete worktree, and the operator is never told to
	// move it back as the worktree (doing so and then removing the source
	// quarantine as redundant would silently discard the skipped files).
	var oursSource, oursCopy, unverified []string
	for _, candidate := range residue {
		binding, bErr := git.BindAFRelocationResidue(candidate, source, repoPath)
		if bErr != nil || binding == git.ResidueUnknown {
			unverified = append(unverified, candidate)
			continue
		}
		if binding == git.ResidueOurs {
			if strings.HasPrefix(filepath.Base(candidate), ".af-source-") {
				oursSource = append(oursSource, candidate)
			} else {
				oursCopy = append(oursCopy, candidate)
			}
		}
	}
	if len(oursSource) == 0 && len(oursCopy) == 0 && len(unverified) == 0 {
		// Only foreign residue: do not block this session's rebuild.
		return nil
	}
	named := append(append(append([]string{}, oursSource...), oursCopy...), unverified...)
	log.WarningLog.Printf("restore of session %q refused: its worktree path %s is gone but af relocation residue is present at %s — "+
		"a cross-device archive move appears to have been cut off between its quarantine and publish renames; "+
		"the complete worktree may be stranded there and was not rebuilt from the branch",
		i.Title, source, strings.Join(named, ", "))
	if len(oursSource) > 0 || len(oursCopy) > 0 {
		// Direct recovery only from the `.af-source-*` quarantine (the complete
		// original); describe the `.af-copy-*` staging copy as possibly
		// incomplete and never as the worktree to move back.
		var recovery strings.Builder
		if len(oursSource) > 0 {
			fmt.Fprintf(&recovery,
				"the complete worktree (including any uncommitted changes) is stranded at the .af-source-* quarantine %s — "+
					"inspect it and move it back to %s with 'mv' (or remove the residue once you have verified the work is saved)",
				strings.Join(oursSource, ", "), source)
		} else {
			fmt.Fprintf(&recovery,
				"a staged copy proven to be this session's is present at %s but its .af-source-* quarantine is absent, "+
					"and the copy may be incomplete if the interrupted archive skipped unreadable files — "+
					"inspect %s and do not move it into %s unless you have verified it is complete",
				strings.Join(oursCopy, ", "), strings.Join(oursCopy, ", "), source)
		}
		if len(oursSource) > 0 && len(oursCopy) > 0 {
			fmt.Fprintf(&recovery,
				"; a staged copy is also present at the .af-copy-* sibling %s beside the destination and may be incomplete if the interrupted archive skipped unreadable files, "+
					"so do not move it into %s as the worktree — recover from the .af-source-* quarantine instead",
				strings.Join(oursCopy, ", "), source)
		}
		return fmt.Errorf(
			"the worktree path %s is gone, but af relocation residue proven to be this session's worktree is present at %s — "+
				"a cross-device archive move was cut off between its quarantine and publish renames and %s before retrying the restore: %w",
			source, strings.Join(named, ", "), recovery.String(), git.ErrRelocateStateUnknown)
	}
	return fmt.Errorf(
		"the worktree path %s is gone, but af relocation residue is present at %s and af could not verify whether it is this session's worktree — "+
			"it may belong to another session, or its git binding could not be read; "+
			"inspect %s, then move it out of %s to somewhere safe (or remove it once you have confirmed it holds nothing you need) and retry the restore; "+
			"do not move it into %s unless you have proven it is this session's work (see #7819): %w",
		source, strings.Join(unverified, ", "), strings.Join(unverified, ", "), filepath.Dir(unverified[0]), source, git.ErrRelocateStateUnknown)
}
