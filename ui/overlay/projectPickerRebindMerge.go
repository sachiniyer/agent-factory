package overlay

import (
	"path/filepath"
	"sort"
)

// MergePreservingSessionRows merges a fresh registry-side project list into
// rows whose session snapshot never answered (#4888 review): projects then
// carries only the registry-side union, so installing it wholesale would drop
// every session-derived row and zero the counts on registry rows — the daemon
// still runs those sessions; this client merely could not recount them. The
// old session-derived rows stay, each surviving registry row keeps the counts
// it last displayed, and only registry truth replaces registry truth: a record
// absent from a HEALTHY registry read still drops its stale row — the merge
// exists to remove exactly those — while a failed registry read (degraded)
// proves nothing about absence and keeps it.
//
// When the competing rebind also moved the registration onto a DIFFERENT
// repository (its RepoID changed — a repair re-clone can land on another
// remote), the merge splits rather than grafts: the old repository's row
// keeps its session counts as a session-only row, and the fresh registry row
// lands under the new repository with nothing carried over — counts tallied
// against one repo must never be attributed to another, and the old repo's
// live sessions must not lose their row (#4888 review).
//
// `old` is the half whose freshness failed: the full prior row set when the
// session snapshot errored, or only the registry-backed rows when the session
// snapshot landed but the registry read degraded — fresh session rows are
// live truth then, so old session rows must not shadow them.
func MergePreservingSessionRows(old, projects []Project, registryDegraded bool) []Project {
	freshByRegistryID := make(map[string]Project, len(projects))
	for _, row := range projects {
		if row.RegistryID != "" {
			freshByRegistryID[row.RegistryID] = row
		}
	}
	merged := make([]Project, 0, len(old)+len(projects))
	seenRepoIDs := make(map[string]bool, len(old)+len(projects))
	seenRoots := make(map[string]bool, len(old)+len(projects))
	// RepoIDs claimed by preserved session rows. A registry row landing on one
	// of these is the rebound-onto-a-live-repo case: it must NOT emit a second
	// row for the same repository — the coalesce below grafts its registration
	// fields onto the session row, which is the one holding the live counts
	// (#4888 review).
	sessionRepoIDs := make(map[string]bool, len(old))
	// RepoIDs of registry rows preserved only because the read was degraded —
	// mapped to their slot in merged. A fresh session row claiming one of
	// these repos carries live truth the stale row lacks, so the dedupe below
	// grafts the identity across rather than hiding the session row.
	staleRegistryRepoIDs := make(map[string]int, len(old))
	// Roots claimed by rows that carry only session provenance. A fresh
	// REGISTRY row colliding on one of these is a different repo's
	// registration at a path the old session list still describes — registry
	// truth must land beside the last-known session row rather than be
	// dropped behind it (#4888 review).
	sessionRoots := make(map[string]bool, len(old))
	// Roots the healthy registry read claims, mapped to the RepoID they now
	// host. A session row's only navigable target is its Root: when the
	// registry says that path belongs to a DIFFERENT repository, the preserved
	// row is stale outright — keeping it would shadow the fresh registration's
	// row and send its Enter to a checkout the old repo no longer occupies
	// (#4888 review). A claim is indexed by BOTH the union row's navigable
	// Root and its RegistryRoot: buildProjectListFrom may promote a session
	// workspace into Root while the registration still records its original
	// root, and either is the path a stale row can be squatting on (#4888
	// review).
	freshRootRepo := make(map[string]string, len(projects))
	for _, row := range projects {
		if row.RegistryID != "" {
			freshRootRepo[row.Root] = row.RepoID
			if row.RegistryRoot != "" {
				freshRootRepo[row.RegistryRoot] = row.RepoID
			}
		}
	}
	for _, row := range old {
		if row.RegistryID == "" {
			// A session-derived row: the snapshot read that produced it never
			// answered, so nothing fresher exists to replace it with — unless
			// the healthy registry just claimed its root for another repo.
			if !registryDegraded {
				if claimedRepo, claimed := freshRootRepo[row.Root]; claimed && claimedRepo != row.RepoID {
					continue
				}
			}
			merged = append(merged, row)
			seenRepoIDs[row.RepoID] = true
			seenRoots[row.Root] = true
			sessionRepoIDs[row.RepoID] = true
			sessionRoots[row.Root] = true
			continue
		}
		if fresh, ok := freshByRegistryID[row.RegistryID]; ok {
			if fresh.RepoID != row.RepoID {
				// RepoID changed: split rather than graft — counts tallied
				// against one repo must never be attributed to another. But a
				// session row is only worth keeping when the OLD repo still
				// has independent provenance behind it: live sessions at a
				// workspace the fresh registration does not claim. A same-Root
				// record means that path now hosts the other repository's
				// checkout, and a zero-session registry-only row has nothing
				// left to describe — either shape preserved would show a
				// phantom project that opens a checkout the registration no
				// longer names (#4888 review).
				if row.Root != fresh.Root && row.SessionCount > 0 {
					// Unless the healthy read claims the old row's root for the
					// replacement repository — the fresh union may prefer an
					// active workspace as its Root while its RegistryRoot is
					// the path this row still squats on, and a stale row kept
					// there would open the other repo's checkout under the old
					// name (#4888 review). The fresh row still lands either
					// way; only the stale session row is withheld.
					if claimedRepo, claimed := freshRootRepo[row.Root]; !claimed || claimedRepo == row.RepoID {
						sessionRow := row
						sessionRow.RegistryID = ""
						sessionRow.RegistryRoot = ""
						sessionRow.RegistryCheckoutID = ""
						sessionRow.MissingPath = false
						merged = append(merged, sessionRow)
						seenRepoIDs[sessionRow.RepoID] = true
						seenRoots[sessionRow.Root] = true
						sessionRepoIDs[sessionRow.RepoID] = true
						sessionRoots[sessionRow.Root] = true
					}
				}
				merged = append(merged, fresh)
				seenRepoIDs[fresh.RepoID] = true
				seenRoots[fresh.Root] = true
				continue
			}
			// The fresh registry row carries the record's new root, name and
			// path state. Its counts carry over from the row it replaces only
			// when the fresh row itself reports none — the same guard the
			// RegistryID-less graft below already applies.
			//
			// Two callers reach this branch with opposite old-count semantics.
			// The snapshot-FAILED caller (a sidebar poll whose fetchErr != nil,
			// or the picker's SetRebindConflictPreserving) passes old rows whose
			// counts are last-known while the fresh row's own are zero — the
			// snapshot could not recount — so the copy restores the live tallies
			// the failed read lost. The snapshot-LANDED caller (a degraded
			// registry poll via registryBackedRows) zeroed the old row's counts
			// precisely because the fresh row's own counts ARE the live truth;
			// and in remote mode buildRemoteProjectList stamps a RegistryID on
			// the fresh row from the stale cached registry even while degraded,
			// so it lands HERE rather than the RegistryID-less graft. An
			// unconditional copy would graft the zeros over the fresh live
			// counts, and the sidebar would read (0) for every registered project
			// for the life of the registry outage (#5833 over #4888). The fresh
			// row's own counts win when it has them; the preserved row's
			// last-known tallies survive only when the snapshot that produced the
			// fresh row did not.
			if fresh.SessionCount == 0 && fresh.InPlaceCount == 0 {
				fresh.SessionCount = row.SessionCount
				fresh.InPlaceCount = row.InPlaceCount
			}
			// So does the navigable workspace when the record's root is
			// missing. A healthy build ranks a live session workspace above an
			// unproven registry root, which is how the old row came to show a
			// Root other than its RegistryRoot; the failed snapshot only lost
			// that session source. Falling back to the missing path would
			// point the sidebar and picker at a directory that is not there
			// (#4888 review). A fresh Root that a higher-priority source (the
			// active workspace, root_agents) still supplies is left alone.
			if fresh.MissingPath && row.Root != "" &&
				filepath.Clean(row.Root) != filepath.Clean(row.RegistryRoot) &&
				filepath.Clean(fresh.Root) == filepath.Clean(fresh.RegistryRoot) {
				fresh.Root = row.Root
				fresh.Name = row.Name
			}
			merged = append(merged, fresh)
			seenRepoIDs[fresh.RepoID] = true
			seenRoots[fresh.Root] = true
			continue
		}
		if registryDegraded {
			// The registry read failed too — an absent record proves nothing
			// under a read that read nothing. Keep the row rather than empty
			// the list behind a warning it cannot confirm.
			staleRegistryRepoIDs[row.RepoID] = len(merged)
			merged = append(merged, row)
			seenRepoIDs[row.RepoID] = true
			seenRoots[row.Root] = true
			continue
		}
		// The healthy read proves the record is gone — but sessions are
		// provenance independent of the registration: the snapshot that
		// produced this row never answered, so its count is the last-known
		// evidence of live work the deletion does not touch. Strip the dead
		// registry identity and keep a session-only row; a zero-session row
		// has nothing left to describe and drops (#4888 review).
		if row.SessionCount > 0 {
			// Unless the healthy read claims this row's root for a DIFFERENT
			// repository: the converted session row's only navigable target is
			// stale outright then, and keeping it would shadow the fresh
			// registration's row and send its Enter to a checkout the old repo
			// no longer occupies — the same claim check the session-derived
			// rows above get (#4888 review).
			if claimedRepo, claimed := freshRootRepo[row.Root]; claimed && claimedRepo != row.RepoID {
				continue
			}
			sessionRow := row
			sessionRow.RegistryID = ""
			sessionRow.RegistryRoot = ""
			sessionRow.RegistryCheckoutID = ""
			sessionRow.MissingPath = false
			merged = append(merged, sessionRow)
			seenRepoIDs[sessionRow.RepoID] = true
			seenRoots[sessionRow.Root] = true
			sessionRepoIDs[sessionRow.RepoID] = true
			sessionRoots[sessionRow.Root] = true
		}
	}
	// Registry-side rows with no counterpart in the old list — a registration
	// newer than the picker, or the root_agents/active rows the snapshot used
	// to supply — still land; an existing row claiming the same identity or
	// root already wins. One exception: a registration whose repo a preserved
	// session row already claims still appends, so the coalesce can graft its
	// registration fields onto that row instead of losing them on the dedupe.
	for _, row := range projects {
		if row.RegistryID != "" && row.RepoID != "" && sessionRepoIDs[row.RepoID] {
			merged = append(merged, row)
			continue
		}
		if seenRepoIDs[row.RepoID] || seenRoots[row.Root] {
			// A fresh SESSION row can collide with a registry row preserved
			// only because the registry read degraded (the merge ran on
			// registry-backed old rows while the snapshot was healthy): the
			// session row is live truth and the registry row carries the
			// durable identity — graft rather than drop the live row behind a
			// stale one (#4888 review).
			if row.RegistryID == "" && row.RepoID != "" {
				if at, ok := staleRegistryRepoIDs[row.RepoID]; ok {
					// The incoming row's counts win only when it has them: a
					// synthesized zero-count fallback (the snapshot failed
					// alongside the registry) must not erase the preserved
					// row's last-known tallies (#4888 review).
					row.RegistryID = merged[at].RegistryID
					row.RegistryRoot = merged[at].RegistryRoot
					row.RegistryCheckoutID = merged[at].RegistryCheckoutID
					row.MissingPath = merged[at].MissingPath
					if row.SessionCount == 0 && row.InPlaceCount == 0 {
						row.SessionCount = merged[at].SessionCount
						row.InPlaceCount = merged[at].InPlaceCount
					}
					merged[at] = row
					continue
				}
				if !seenRepoIDs[row.RepoID] {
					// The collision is on ROOT alone: a preserved stale
					// registry row claims this path for a DIFFERENT repo
					// (the checkout was replaced by another repository).
					// The live session row is authority on what runs there
					// and must land; the stale row keeps its registration
					// claim beside it until a healthy read settles it
					// (#4888 review).
					merged = append(merged, row)
					seenRepoIDs[row.RepoID] = true
					sessionRoots[row.Root] = true
					continue
				}
			}
			// The mirror image: a fresh REGISTRY row — authoritative even in
			// a degraded merge, since it came from the read that DID answer —
			// colliding on root alone with session-derived rows must still
			// land. Dropping it would leave the stale session row naming a
			// repo the path no longer hosts (#4888 review).
			if row.RegistryID != "" && !seenRepoIDs[row.RepoID] && sessionRoots[row.Root] {
				merged = append(merged, row)
				seenRepoIDs[row.RepoID] = true
			}
			continue
		}
		merged = append(merged, row)
		seenRepoIDs[row.RepoID] = true
		seenRoots[row.Root] = true
		if row.RegistryID == "" {
			sessionRoots[row.Root] = true
		}
	}
	out := coalesceReboundRows(merged)
	// The preservation path substitutes and appends where the degraded reads
	// landed, so restore the ordering buildProjectListFrom documents — a
	// rebind that moved a name/root must not leave the list mis-sorted for
	// the life of the degraded refresh (#4888 review).
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Root < out[j].Root
	})
	return out
}

// SetRebindConflictPreserving is SetRebindConflict for a conflict refresh whose
// session snapshot never answered (#4888 review): the picker's rows merge with
// the registry-side union rather than being replaced by it — see
// MergePreservingSessionRows, which the sidebar shares for the same failure.
func (p *ProjectPickerOverlay) SetRebindConflictPreserving(msg string, projects []Project, registryDegraded bool) {
	p.all = MergePreservingSessionRows(p.all, projects, registryDegraded)
	p.reseatAfterConflictRebuild()
	p.SetRebindError(msg)
}

// coalesceReboundRows resolves the rebound-onto-a-live-repo collision
// SetRebindConflictPreserving can produce: a registration that moved onto a
// repository the picker already listed from live sessions would otherwise
// appear twice — once as the fresh registry row (carrying counts tallied
// against the OLD root, before the snapshot failed) and once as the preserved
// session row holding the real counts. The session row owns the repo's slot
// and gains the registration fields — the durable id, the recorded root and
// checkout, the record's path state — and the duplicate registry row is
// dropped.
func coalesceReboundRows(rows []Project) []Project {
	sessionRowByRepoID := make(map[string]int, len(rows))
	for i, row := range rows {
		if row.RegistryID == "" && row.RepoID != "" {
			if _, dup := sessionRowByRepoID[row.RepoID]; !dup {
				sessionRowByRepoID[row.RepoID] = i
			}
		}
	}
	// Graft in place before the output slice is built: the registry row can
	// sort before OR after the session row it merges into, so the copy into
	// out must happen only after every graft has landed.
	dropped := make([]bool, len(rows))
	for i, row := range rows {
		if at, ok := sessionRowByRepoID[row.RepoID]; ok && at != i && row.RegistryID != "" && row.RepoID != "" {
			rows[at].RegistryID = row.RegistryID
			rows[at].RegistryRoot = row.RegistryRoot
			rows[at].RegistryCheckoutID = row.RegistryCheckoutID
			rows[at].MissingPath = row.MissingPath
			dropped[i] = true
		}
	}
	out := make([]Project, 0, len(rows))
	for i, row := range rows {
		if !dropped[i] {
			out = append(out, row)
		}
	}
	return out
}

// reseatAfterConflictRebuild restores the cursor and rebind target after the
// row set changed under a conflict: clamp the index into the navigable rows,
// then land on the rebound row wherever the rebuild sorted it.
func (p *ProjectPickerOverlay) reseatAfterConflictRebuild() {
	// len(p.all) is the trailing add-project row — still a valid cursor spot.
	if p.selectedIdx > len(p.all) {
		p.selectedIdx = len(p.all)
	}
	for i := range p.all {
		if p.all[i].RegistryID != "" && p.all[i].RegistryID == p.rebindTarget.RegistryID {
			p.rebindTarget = p.all[i]
			// The rebuild can reorder rows — the record's name, root, or
			// missing-path flag feed the sort — so land the cursor on the
			// rebound row rather than leaving it on whatever now occupies the
			// old numeric position: an Esc returning to the list must
			// highlight THIS registration (#4888 review).
			p.selectedIdx = i
			return
		}
	}
	// No matching registration survived the rebuild — the record was deleted
	// between the refusal and the refresh. The clamp above only keeps the
	// index in range; leaving it lands Esc on whatever unrelated row occupies
	// the old position, and Enter/`b` would then act on THAT project. Park the
	// cursor on the trailing add-project row instead (#4888 review).
	p.selectedIdx = len(p.all)
}
