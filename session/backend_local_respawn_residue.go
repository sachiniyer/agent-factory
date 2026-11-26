package session

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
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
//
// The refusal has two forms on purpose (#7847). The daemon WARNING carries the
// full explanation and every residue path. The returned error's message is a
// compact record that leads with the remedy and the residue path, because the
// durable user-facing record (LostRestoreFailure.Error) is capped at 512 runes
// (lost_restore_failure.go) — and the respawn call site stores it leading the
// "%w (%w)" pair so the cap clips only the trailing stat frame, never the
// remedy (#7855). residueRebuildRefusal keeps the ErrRelocateStateUnknown
// verdict for the errors.Is classifiers without spending record runes on the
// sentinel's own sentence.
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

	// The daemon WARNING keeps the full explanation and the full residue list;
	// the returned record is the compact form below. The guidance wording
	// mirrors the archive endpoint's (archiveSessionSourceAbsent, #7804/#7807)
	// so the two surfaces read the same.
	var detail string
	switch {
	case len(oursSource) > 0:
		detail = fmt.Sprintf(
			"the complete worktree (including any uncommitted changes) is stranded at the .af-source-* quarantine %s — "+
				"inspect it and move it back to %s with 'mv' (or remove the residue once you have verified the work is saved)",
			strings.Join(oursSource, ", "), source)
		if len(oursCopy) > 0 {
			detail += fmt.Sprintf(
				"; a staged copy is also present at the .af-copy-* sibling %s beside the destination and may be incomplete "+
					"if the interrupted archive skipped unreadable files, so do not move it into %s as the worktree — "+
					"recover from the .af-source-* quarantine instead",
				strings.Join(oursCopy, ", "), source)
		}
	case len(oursCopy) > 0:
		detail = fmt.Sprintf(
			"a staged copy proven to be this session's is present at %s but its .af-source-* quarantine is absent, "+
				"and the copy may be incomplete if the interrupted archive skipped unreadable files — "+
				"inspect %s and do not move it into %s unless you have verified it is complete",
			strings.Join(oursCopy, ", "), strings.Join(oursCopy, ", "), source)
	default:
		detail = fmt.Sprintf(
			"af could not verify whether the residue is this session's worktree — it may belong to another session, "+
				"or its git binding could not be read; inspect %s, then move it out of %s to somewhere safe "+
				"(or remove it once you have confirmed it holds nothing you need) and retry the restore; "+
				"do not move it into %s unless you have proven it is this session's work (see #7819)",
			strings.Join(unverified, ", "), filepath.Dir(unverified[0]), source)
	}
	if len(unverified) > 0 && (len(oursSource) > 0 || len(oursCopy) > 0) {
		detail += fmt.Sprintf("; unverified residue is also present at %s", strings.Join(unverified, ", "))
	}
	log.WarningLog.Printf("restore of session %q refused: its worktree path %s is gone but af relocation residue is present at %s — "+
		"a cross-device archive move appears to have been cut off between its quarantine and publish renames; "+
		"the complete worktree may be stranded there and was not rebuilt from the branch; %s",
		i.Title, source, strings.Join(named, ", "), detail)

	// The user-facing record (LostRestoreFailure.Error) is capped at
	// lostRestoreFailureMaxRunes runes by sanitizeLostRestoreError, and it is
	// stored under the recover wrapper — WorktreeUnavailableError's
	// "recover: session %q worktree unavailable: %v" frame around the
	// "%w (%w)" pair this refusal LEADS, with the stat ENOENT trailing — so
	// the cap clips only the expendable stat tail (a path the record already
	// names), never the remedy (#7847, #7855). overhead mirrors that stored
	// skeleton; keep it in step with backend_local_errors.go and the respawn
	// call site.
	overhead := len("recover: session \"\" worktree unavailable: ") + len(" ()") +
		len([]rune(i.Title))
	var record strings.Builder
	room := func() int {
		return lostRestoreFailureMaxRunes - overhead - len([]rune(record.String()))
	}
	// tail appends a clause only while it fits whole: one the budget cannot
	// hold is dropped at its boundary rather than clipped mid-word by the
	// cap. The remedy itself is never a tail — it leads the record.
	tail := func(frag string) {
		if len([]rune(frag)) <= room() {
			record.WriteString(frag)
		}
	}
	// extraResidue appends "; <frag>" for a secondary residue category: the
	// category's paths are named while they fit the room the record has left,
	// and when even the first path no longer fits the fragment degrades to a
	// bare count that still points at the daemon log — counted, never
	// silently truncated mid-path.
	extraResidue := func(paths []string, frag string, fragArgs ...any) {
		if len(paths) == 0 {
			return
		}
		fixed := len([]rune(fmt.Sprintf(frag, append([]any{""}, fragArgs...)...)))
		if room() < fixed+len([]rune(paths[0])) {
			noun := "paths"
			if len(paths) == 1 {
				noun = "path"
			}
			tail(fmt.Sprintf("; %d more residue %s (full list in the daemon log)", len(paths), noun))
			return
		}
		fmt.Fprintf(&record, frag, append([]any{residueRecordList(paths, room()-fixed)}, fragArgs...)...)
	}
	// Residue paths go into the record as basenames: every candidate is a
	// sibling of the recorded path (the scan covered only its parent), so the
	// parent is named once and the basenames resolve against it — the same
	// information as the absolute paths at a fraction of the runes, which is
	// what keeps the remedy inside the cap at realistic path depths (#7855).
	parent := filepath.Dir(source)
	switch {
	case len(oursSource) > 0:
		// The literal command first: `cd <parent> && mv <quarantine> <leaf>`
		// IS the action and names every path it needs — basenames against the
		// parent keep the command inside the cap at path lengths where two
		// absolute paths alone would overflow it. It is rendered through
		// shellsuggest because the record's whole purpose is to be read and
		// pasted — a parent with spaces or metacharacters must still arrive as
		// exactly one operand.
		fmt.Fprintf(&record, "%s && %s to move this session's stranded worktree back",
			shellsuggest.Command("cd", parent),
			shellsuggest.Command("mv", "--", filepath.Base(oursSource[0]), filepath.Base(source)))
		tail(" — proven the complete worktree (incl. uncommitted changes), left by a cut-off archive move")
		tail("; or remove it once the work is saved, then retry")
		extraResidue(baseNames(oursSource[1:]), "; also proven ours: %s")
		extraResidue(baseNames(oursCopy), "; staged copy %s may be incomplete — recover from the quarantine, never the copy")
		extraResidue(baseNames(unverified), "; unverified residue %s also present — do not move it into the session path unless proven (#7819)")
	case len(oursCopy) > 0:
		const frag = "inspect %s in %s: a staged .af-copy-* copy proven to be this session's, " +
			"but possibly incomplete (its .af-source-* quarantine is absent)"
		list := residueRecordList(baseNames(oursCopy), room()-len([]rune(fmt.Sprintf(frag, "", parent))))
		fmt.Fprintf(&record, frag, list, parent)
		tail(" — do not move it into " + source + " unless verified complete, then retry")
		extraResidue(baseNames(unverified), "; unverified residue %s also present — do not move it into the session path unless proven (#7819)")
	default:
		const frag = "inspect %s in %s — af could not verify it is this session's; " +
			"move it out of that folder once checked, then retry"
		list := residueRecordList(baseNames(unverified), room()-len([]rune(fmt.Sprintf(frag, "", parent))))
		fmt.Fprintf(&record, frag, list, parent)
		tail(" (or remove it once confirmed unneeded)")
		tail("; do not move it into " + source + " unless proven (#7819)")
	}
	return &residueRebuildRefusal{msg: record.String()}
}

// residueRebuildRefusal is the crash-window rebuild refusal's user-facing
// form (#7847). Its message is the compact record text — leading with the
// remedy and the residue path, and stored leading the "%w (%w)" pair so the
// LostRestoreFailure rune cap clips only the trailing stat frame (#7855) —
// while Unwrap carries the ErrRelocateStateUnknown verdict for the errors.Is
// classifiers (daemon/lostrestore_environmental.go) without spending record
// runes on the sentinel's own sentence.
type residueRebuildRefusal struct{ msg string }

func (e *residueRebuildRefusal) Error() string { return e.msg }

func (e *residueRebuildRefusal) Unwrap() error { return git.ErrRelocateStateUnknown }

// baseNames reduces residue paths to their basenames for the capped record:
// every candidate is a sibling of the recorded source path, whose parent the
// record names once, so the basenames carry the same locating information as
// the absolute paths without spending record runes on the shared directory
// prefix (#7855).
func baseNames(paths []string) []string {
	names := make([]string, len(paths))
	for i, p := range paths {
		names[i] = filepath.Base(p)
	}
	return names
}

// residueRecordList joins residue paths into the user-facing record: the
// first path always whole — it is the thing the remedy acts on — more whole
// paths while they fit room runes, then "and N more (full list in the daemon
// log)", so a longer list is counted rather than silently truncated: the
// daemon WARNING names every candidate (#7847).
//
// The ", and N more" suffix is itself part of the record, so each additional
// path is admitted only while room remains for that suffix behind it —
// spending the whole budget on paths would let the suffix push the record
// past the cap and re-clip the remedy the record exists to carry.
func residueRecordList(paths []string, room int) string {
	var b strings.Builder
	included, used := 0, 0
	for i, p := range paths {
		pl := len([]rune(p))
		sep := len(", ")
		if included == 0 {
			sep = 0
		}
		reserve := 0
		if more := len(paths) - i - 1; more > 0 {
			reserve = len(fmt.Sprintf(", and %d more (full list in the daemon log)", more))
		}
		if included > 0 && used+sep+pl+reserve > room {
			break
		}
		if included > 0 {
			b.WriteString(", ")
		}
		b.WriteString(p)
		used += sep + pl
		included++
	}
	if more := len(paths) - included; more > 0 {
		fmt.Fprintf(&b, ", and %d more (full list in the daemon log)", more)
	}
	return b.String()
}
