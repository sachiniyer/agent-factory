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
// remedy (#7855). Within the record every MANDATORY clause — each residue
// named or counted, the remedy, the warning against moving residue into this
// session's path, the #7819 citation — is written unconditionally: what yields
// to the cap is only the form each clause takes, through a fixed ladder of
// abbreviations (full path → leaf resolved against the named parent →
// "the session path"; named residue → counted residue → the daemon log), so
// nothing mandatory is dropped at realistic title or path lengths (#7855,
// second round — a droppable tail is how the citation and the warning were
// lost on macOS). residueRebuildRefusal keeps the ErrRelocateStateUnknown
// verdict for the errors.Is classifiers without spending record runes on the
// sentinel's own sentence.
//
// leafInference selects the route's authority (#7819 R6). The lost-respawn
// rebuild passes true: that route's crash-window arbitration predates the
// record work, so master's leaf inference (BindAFRelocationResidue) still
// refuses candidates no durable record names. RebuildAbsentArchivedWorktree
// and adoptExistingWorktree pass false: the base ran no residue scan on
// those routes at all, so only a candidate THIS session's own durable
// record names may refuse — leaf inference, unverifiable candidates, a
// failed scan and an enumeration problem all proceed exactly as the base
// did, and one pruned-leaf `.af-copy-*` must never refuse every
// absent-archive restore in a repo.
func crashWindowResidueRebuildRefusal(i *Instance, gw *git.GitWorktree, leafInference bool) error {
	source := gw.GetWorktreePath()
	if source == "" {
		// No recorded path to scan beside. The rebuild-path caller would already
		// have refused on an empty workDir, so this is unreachable in production;
		// treat as no residue rather than guessing.
		return nil
	}
	residue, resErr := git.ListAFRelocationResidue(filepath.Dir(source))
	if resErr != nil {
		if !leafInference {
			// The base ran no scan on this route; a failed one adds no refusal.
			return nil
		}
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
	// rebuild only when it is proven to be THIS session's worktree. Durable
	// records answer first (#7819): a candidate named by this session's own
	// move record is proven ours without touching git's leaf; a candidate
	// named by this session's retained-tree report is kept work already
	// accounted for — carried forward, never refused; a candidate named by
	// ANOTHER session's record or report is foreign and never blocks. Only
	// residue no record names falls to master's leaf inference
	// (BindAFRelocationResidue), and a candidate whose binding cannot be
	// read still refuses fail-closed — named as unverified rather than this
	// session's stranded work.
	//
	// The proven-ours residue is split by purpose: `.af-source-*` is the renamed
	// source — the COMPLETE worktree, including any uncommitted changes — so
	// recovery is directed from it. `.af-copy-*` is a staged copy, which under
	// skipUnreadable may intentionally omit unreadable paths; it must never be
	// described as the complete worktree, and the operator is never told to
	// move it back as the worktree (doing so and then removing the source
	// quarantine as redundant would silently discard the skipped files).
	arbiter := NewResidueArbiter(i)
	var oursSource, oursCopy, unverified []string
	for _, candidate := range residue {
		switch arbiter.Classify(candidate) {
		case ResidueOursQuarantine:
			oursSource = append(oursSource, candidate)
			continue
		case ResidueOursStaging:
			oursCopy = append(oursCopy, candidate)
			continue
		case ResidueOursRetained, ResidueForeignSession:
			continue
		}
		if !leafInference {
			// Only a record-named candidate may refuse on this route (#7819
			// R6): leaf evidence — including a leaf-proven ours a recycled
			// leaf can fabricate — changes nothing the base did.
			continue
		}
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
	enumStores, enumErr := arbiter.EnumerationProblem()
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
		enumNote := ""
		if enumErr != nil {
			enumNote = fmt.Sprintf("; %v, so other sessions' records could not rule it out either", enumErr)
		}
		detail = fmt.Sprintf(
			"af could not verify whether the residue is this session's worktree — it may belong to another session, "+
				"or its git binding could not be read%s; inspect %s, then move it out of %s to somewhere safe "+
				"(or remove it once you have confirmed it holds nothing you need) and retry the restore; "+
				"do not move it into %s unless you have proven it is this session's work (see #7819)",
			enumNote, strings.Join(unverified, ", "), filepath.Dir(unverified[0]), source)
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
	// cap. Only OPTIONAL context may ride a tail — every mandatory clause is
	// written below unconditionally at the fullest of its abbreviation
	// ladder, because a droppable tail is exactly how the #7819 citation and
	// the do-not-move warning were lost at deep paths (#7855, second round).
	tail := func(frag string) {
		if len([]rune(frag)) <= room() {
			record.WriteString(frag)
		}
	}
	parent := filepath.Dir(source)
	srcLeaf := filepath.Base(source)
	unverifiedNames := baseNames(unverified)

	// mustResidue writes a mandatory secondary-residue clause: the category's
	// names while they fit the room left after the floors the clauses behind
	// it must keep, else its floor — presence plus its warning — appended
	// unconditionally. A mandatory clause is never dropped: at the extreme
	// where even the floor overflows, the cap clips the record's end rather
	// than the clause being silently lost.
	mustResidue := func(reserve int, paths []string, namedFrag string, floor func(int) string) {
		if len(paths) == 0 {
			return
		}
		fixed := len([]rune(fmt.Sprintf(namedFrag, "")))
		if s := fmt.Sprintf(namedFrag,
			residueRecordList(paths, room()-reserve-fixed)); len([]rune(s)) <= room()-reserve {
			record.WriteString(s)
			return
		}
		record.WriteString(floor(len(paths)))
	}
	floorLen := func(floor func(int) string, n int) int {
		if n == 0 {
			return 0
		}
		return len([]rune(floor(n)))
	}
	// enumRecordClause appends the mandatory unreadable-store clause once an
	// unverified candidate exists that the broken enumeration could have
	// cleared (#7819 review): every store path while they fit, then the floor
	// naming the repo each unreadable file belongs to — the
	// <home>/instances/<repoID> directory name is the identifying part at cap
	// depth. It runs LAST in every branch and its floor joins the reserves
	// the earlier clauses keep, because a clause written unconditionally
	// could otherwise push a mandatory floor behind it over the cap.
	enumRepoNames := func() []string {
		names := make([]string, len(enumStores))
		for i, store := range enumStores {
			if dir := filepath.Dir(store); dir != "." && dir != "" {
				names[i] = filepath.Base(dir)
			} else {
				names[i] = store
			}
		}
		return names
	}
	enumFloor := func() string {
		return fmt.Sprintf("; could not read session store for repo %s — its records could not rule this out",
			strings.Join(enumRepoNames(), ", "))
	}
	enumFloorRunes := func() int {
		if enumErr == nil || len(unverified) == 0 {
			return 0
		}
		return len([]rune(enumFloor()))
	}
	enumRecordClause := func() {
		if enumErr == nil || len(unverified) == 0 {
			return
		}
		record.WriteString(fitResidueForm(room(),
			func(int) string {
				return fmt.Sprintf("; could not read session store %s — its records could not rule this out",
					strings.Join(enumStores, ", "))
			},
			func(int) string { return enumFloor() },
		))
	}
	switch {
	case len(oursSource) > 0:
		// The literal command first: `cd <parent> && mv <quarantine> <leaf>`
		// IS the remedy and names every path it needs — basenames against the
		// parent keep the command inside the cap at path lengths where two
		// absolute paths alone would overflow it. It is rendered through
		// shellsuggest because the record's whole purpose is to be read and
		// pasted — a parent with spaces or metacharacters must still arrive as
		// exactly one operand. The command is sized against the floors the
		// mandatory clauses behind it keep — the unverified warning and its
		// #7819 citation first, then the staged-copy warning and any extra
		// proven-ours quarantines — so a fuller command never starves them.
		// The ladder's only other rung is the parentless mv floor: an elided
		// parent would put a non-path `…` inside a command meant to be pasted,
		// and every operand stays shellsuggest-quoted on both rungs so a leaf
		// with spaces or metacharacters still arrives as one operand.
		quarantineLeaf := filepath.Base(oursSource[0])
		unverifiedFloorLen := floorLen(unverifiedExtraFloor, len(unverified))
		copyFloorLen := floorLen(copyExtraFloor, len(oursCopy))
		oursFloorLen := floorLen(oursExtraFloor, len(oursSource)-1)
		record.WriteString(fitResidueForm(
			room()-unverifiedFloorLen-copyFloorLen-oursFloorLen-enumFloorRunes(),
			func(int) string {
				return shellsuggest.Command("cd", parent) + " && " +
					shellsuggest.Command("mv", "--", quarantineLeaf, srcLeaf) +
					" to move this session's stranded worktree back, then retry"
			},
			func(int) string {
				return shellsuggest.Command("mv", "--", quarantineLeaf, srcLeaf) +
					" — move this session's stranded worktree back (parent in the daemon log), then retry"
			},
		))
		mustResidue(copyFloorLen+oursFloorLen+enumFloorRunes(), unverifiedNames,
			"; unverified residue %s also present — do not move it into the session path unless proven (#7819)",
			unverifiedExtraFloor)
		mustResidue(oursFloorLen+enumFloorRunes(), baseNames(oursCopy),
			"; staged copy %s may be incomplete — recover from the quarantine, never the copy",
			copyExtraFloor)
		mustResidue(enumFloorRunes(), baseNames(oursSource[1:]),
			"; also proven ours: %s",
			oursExtraFloor)
		enumRecordClause()
		tail("; or remove it once the work is saved")
		tail(" — proven the complete worktree (incl. uncommitted changes), left by a cut-off archive move")
	case len(oursCopy) > 0:
		// No quarantine — only a staged copy proven ours. It may be
		// incomplete, so the record must keep the warnings: the copy is
		// proven this session's but is never the thing to move back in.
		copyNames := baseNames(oursCopy)
		unverifiedFloorLen := floorLen(unverifiedExtraFloor, len(unverified))
		record.WriteString(fitResidueForm(room()-unverifiedFloorLen-enumFloorRunes(),
			residueForm(residueCopyRecordFull, parent, source, copyNames),
			residueForm(residueCopyRecordFull, parent, srcLeaf, copyNames),
			residueForm(residueCopyRecordCompact, parent, source, copyNames),
			residueForm(residueCopyRecordCompact, parent, srcLeaf, copyNames),
			residueForm(residueCopyRecordCompact, parent, residueSessionPathRef, copyNames),
			residueElidedParentForm(residueCopyRecordCompact, parent, srcLeaf, copyNames),
			residueElidedParentForm(residueCopyRecordCompact, parent, residueSessionPathRef, copyNames),
			residueFloorForm(residueCopyRecordFloor, copyNames),
		))
		mustResidue(enumFloorRunes(), unverifiedNames,
			"; unverified residue %s also present — do not move it into the session path unless proven (#7819)",
			unverifiedExtraFloor)
		enumRecordClause()
	default:
		// Unverified residue only: the fail-closed refusal names the
		// residue, says the binding could not be read, gives the move-it-out
		// remedy, and keeps the warning and #7819 citation — every one
		// mandatory at every rung of the ladder.
		record.WriteString(fitResidueForm(room(),
			residueForm(residueUnverifiedRecordFull, parent, source, unverifiedNames),
			residueForm(residueUnverifiedRecordFull, parent, srcLeaf, unverifiedNames),
			residueForm(residueUnverifiedRecordCompact, parent, source, unverifiedNames),
			residueForm(residueUnverifiedRecordCompact, parent, srcLeaf, unverifiedNames),
			residueForm(residueUnverifiedRecordCompact, parent, residueSessionPathRef, unverifiedNames),
			residueElidedParentForm(residueUnverifiedRecordCompact, parent, srcLeaf, unverifiedNames),
			residueElidedParentForm(residueUnverifiedRecordCompact, parent, residueSessionPathRef, unverifiedNames),
			residueFloorForm(residueUnverifiedRecordFloor, unverifiedNames),
		))
		enumRecordClause()
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

const (
	// residueSessionPathRef is the record's last-resort reference to this
	// session's own worktree path: the leaf normally resolves against the
	// parent the record already named, but when even that cannot fit the
	// warning still has to name what not to move residue into — the daemon
	// log keeps the literal path.
	residueSessionPathRef = "the session path"

	// residueParentFloorRunes is the smallest elided parent still worth
	// writing: below it the stub locates nothing, so the rung that would
	// have written it renders the full parent instead — guaranteed to fail
	// its room check and hand the record to the parentless floor.
	residueParentFloorRunes = 24

	// The unverified-only record's abbreviation ladder — fullest first,
	// floor last. Every rung carries the mandatory parts (#7855): the
	// residue's name or count, the move-it-out remedy, the warning against
	// moving it into this session's path, and the #7819 citation.
	residueUnverifiedRecordFull = "inspect %s in %s — af could not verify it is this session's; " +
		"move it out of that folder once checked, then retry " +
		"(or remove it once confirmed unneeded); do not move it into %s " +
		"unless you have proven it is this session's work (see #7819)"
	residueUnverifiedRecordCompact = "inspect %s in %s — could not verify it is ours; " +
		"move it out of that folder once checked, then retry; " +
		"do not move it into %s unless proven (#7819)"
	residueUnverifiedRecordFloor = "%s — could not verify it is ours (paths in the daemon log); " +
		"move it out once checked, then retry; " +
		"do not move it into the session path unless proven (#7819)"

	// The copy-only record's abbreviation ladder: the copy's name, that it
	// is proven this session's yet possibly incomplete, and the warning
	// never to move it into the session path as the worktree.
	residueCopyRecordFull = "inspect %s in %s: a staged .af-copy-* copy proven to be this session's, " +
		"but possibly incomplete (its .af-source-* quarantine is absent) — " +
		"do not move it into %s unless verified complete, then retry"
	residueCopyRecordCompact = "inspect %s in %s — staged .af-copy-* copy proven ours, " +
		"possibly incomplete; do not move it into %s unless verified complete, then retry"
	residueCopyRecordFloor = "%s — staged copy proven ours, possibly incomplete " +
		"(paths in the daemon log); do not move it into the session path " +
		"unless verified complete, then retry"
)

// fitResidueForm renders a mandatory clause's abbreviation ladder in
// preference order — fullest first, floor last — and returns the first rung
// that fits room runes. The last candidate is the floor and is returned even
// when it still overflows: the rune cap may clip the record's end, but a
// mandatory clause is never silently dropped (#7855, second round).
func fitResidueForm(room int, candidates ...func(int) string) string {
	for _, candidate := range candidates {
		if s := candidate(room); len([]rune(s)) <= room {
			return s
		}
	}
	return candidates[len(candidates)-1](room)
}

// residueForm renders one rung of a record clause's ladder: frag's first
// placeholder takes the residue list — sized to whatever room the rest of
// the clause leaves — and the remaining two take the parent and the
// session-path reference, which earlier rungs name in full and later rungs
// abbreviate to the leaf or to "the session path".
func residueForm(frag, parentRef, srcRef string, names []string) func(int) string {
	return func(room int) string {
		fixed := len([]rune(fmt.Sprintf(frag, "", parentRef, srcRef)))
		return fmt.Sprintf(frag, residueRecordList(names, room-fixed), parentRef, srcRef)
	}
}

// residueElidedParentForm is the ladder rung where the residue's PARENT is
// the field that gives: the list renders at its floor — first name whole,
// the rest counted — and the parent keeps every rune they leave, eliding
// only its middle.
func residueElidedParentForm(frag, parent, srcRef string, names []string) func(int) string {
	return func(room int) string {
		list := residueRecordList(names, 0)
		rest := len([]rune(fmt.Sprintf(frag, list, "", srcRef)))
		parentRef := elideMiddle(parent, room-rest)
		if len([]rune(parentRef)) < residueParentFloorRunes {
			parentRef = parent
		}
		return fmt.Sprintf(frag, list, parentRef, srcRef)
	}
}

// residueFloorForm renders the parentless floor of a record clause — the
// mandatory text alone — around the residue list at whatever fullness room
// runes still allow.
func residueFloorForm(frag string, names []string) func(int) string {
	return func(room int) string {
		fixed := len([]rune(fmt.Sprintf(frag, "")))
		return fmt.Sprintf(frag, residueRecordList(names, room-fixed))
	}
}

// elideMiddle shortens s to at most max runes by replacing its middle with
// an ellipsis, so head and tail still locate what it named — the same mark
// the sanitize cap leaves on overrun, applied deliberately while there is
// still a choice about what to keep.
func elideMiddle(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 0 {
		return ""
	}
	if max == 1 {
		return "…"
	}
	head := (max - 1) / 2
	return string(r[:head]) + "…" + string(r[len(r)-(max-1-head):])
}

// unverifiedExtraFloor is the smallest mandatory clause recording unverified
// residue beside a proven one: its presence, the warning against moving it
// into this session's path, and the #7819 citation.
func unverifiedExtraFloor(n int) string {
	if n == 1 {
		return "; unverified residue also present — do not move it into the session path unless proven (#7819)"
	}
	return fmt.Sprintf("; %d unverified residue paths also present — do not move any into the session path unless proven (#7819)", n)
}

// copyExtraFloor is the smallest mandatory clause recording a staged
// .af-copy-* beside a proven quarantine: its presence and the warning never
// to recover from the possibly-incomplete copy.
func copyExtraFloor(n int) string {
	if n == 1 {
		return "; a staged copy also present — recover from the quarantine, never the copy"
	}
	return fmt.Sprintf("; %d staged copies also present — recover from the quarantine, never a copy", n)
}

// oursExtraFloor is the smallest mandatory clause counting extra quarantines
// proven to be this session's beyond the one the remedy names.
func oursExtraFloor(n int) string {
	if n == 1 {
		return "; another quarantine also proven ours (name in the daemon log)"
	}
	return fmt.Sprintf("; %d more quarantines proven ours (names in the daemon log)", n)
}
