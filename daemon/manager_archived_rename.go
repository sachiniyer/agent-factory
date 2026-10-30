// Reusing an archived session's title: the machinery that frees a name held
// only by an archived record. Split out of manager_create_titles.go, which
// keeps the ADMISSION side (can this title be used, what blocks it); this
// file holds the REUSE side — find the one archived row whose claim a rename
// may clear (findArchivedOnlyCollisionLocked), refuse a create the rename
// cannot rescue (refuseUnclaimableTitleReuseLocked), derive the disambiguated
// "(archived N)" replacement (uniqueArchivedTitleLocked), and run the
// rename + re-key + persist + rollback itself (renameArchivedForReuseLocked,
// through the reuseArchivedRenamePersist test seam).

package daemon

import (
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// refuseUnclaimableTitleReuseLocked refuses an explicit-title create BEFORE the
// archived-name-reuse rename touches anything, when the title cannot be claimed
// for a reason the rename has no effect on (#2415). Runs under m.mu.
//
// renameArchivedForReuseLocked clears exactly one thing: an af RECORD holding the
// title. It does not free the "root" name, an orphan tmux session left by a crash,
// or a hook slug another project owns — yet validateTitleAvailableLocked checks all
// of those, and it ran AFTER the rename. So a create could fail on a condition that
// was already true before anything happened, with the archived session left renamed
// to "<title> (archived)", its worktree physically relocated, its manager key
// changed, and its durable record rewritten — permanently, with no rollback, for a
// create that never occurred. That is the same invariant #2127 protects from the
// branch side, and the one reserveCreate's admission comment promises.
//
// The gap was structural rather than a missing case: the checks lived inline in a
// function called after the mutation, so every check added there inherited the bug.
// Splitting validateTitleAvailableLocked into a record-dependent half and a
// record-independent half is what makes "ask everything answerable before mutating"
// the default for future checks instead of a thing to remember.
//
// Two deliberate non-firings, mirroring refuseHeldBranchReuseLocked:
//
//   - No archived collision means no rename will happen, so there is no invariant
//     to protect and this must stay out of the way. The create proceeds and
//     validateTitleAvailableLocked reports the same refusal in the same words, at
//     the same point it always did.
//   - The archived row itself is excluded from the scans (the ignore argument).
//     It is the claim the rename is about to release, so counting it would refuse
//     a reuse that would have succeeded — turning a data-integrity fix into a
//     feature regression.
func (m *Manager) refuseUnclaimableTitleReuseLocked(repoID, repoPath, title, program string, namespace runtimeNameNamespace, allowReserved bool, diskData []session.InstanceData, inPlace bool, remedyPath ...string) error {
	archived, _, err := m.findArchivedOnlyCollisionLocked(repoID, repoPath, title, namespace, diskData)
	if err != nil || archived == nil {
		return err
	}
	if err := m.pruneRowFence(repoID, archived); err != nil {
		return fmt.Errorf("the title %q is held by an archived session: %w", archived.Title, err)
	}
	return m.validateTitleClaimableLocked(repoID, repoPath, title, program, namespace, allowReserved, diskData, archived, inPlace, remedyPath...)
}

// reuseArchivedRenamePersist is the durable title rewrite the archived-name-reuse
// rename runs. A package var so tests can force that write to fail in isolation —
// exercising the rollback, and the double-failure recovery branch behind it
// (#2106) — without disturbing any other persist. Mirrors archivePersist's and
// killTombstonePersist's precedent. Production points it at the real writer and
// never reassigns it.
var reuseArchivedRenamePersist = renameInstanceDataTitle

// renameArchivedForReuseLocked frees `title` for a new session when the ONLY thing
// holding it is an archived session, by renaming that archived session to a
// disambiguated "<title> (archived[ N])" (feat: reuse archived name). It returns
// the renamed archived session's data (for a session.updated event) or nil when no
// rename happened — no archived collision, or a LIVE/reserved session also holds
// the name, in which case the create is left to fail in validateTitleAvailableLocked
// exactly as before. Runs under m.mu.
func (m *Manager) renameArchivedForReuseLocked(repoID, repoPath, title, program string, namespace runtimeNameNamespace, diskData *[]session.InstanceData) (*session.InstanceData, error) {
	archived, oldKey, err := m.findArchivedOnlyCollisionLocked(repoID, repoPath, title, namespace, *diskData)
	if err != nil {
		return nil, err
	}
	if archived == nil {
		return nil, nil
	}
	// The rename relocates the archive. A pending prune intent owns that worktree
	// until it resolves (the startup resume or the next apply), so refuse before
	// anything moves (#5189).
	if err := m.pruneRowFence(repoID, archived); err != nil {
		return nil, fmt.Errorf("the title %q is held by an archived session: %w", archived.Title, err)
	}
	oldTitle := archived.Title
	// The replacement name must clear the same bar the archived row itself had:
	// if it is a HOOK session, restoring it later re-provisions with --name
	// Slugify(newTitle), so that slug has to be free in the GLOBAL hook namespace
	// too — otherwise the rename quietly parks it on a name another project's
	// sandbox already owns.
	archivedNamespace := runtimeNamespaceSandbox
	if archived.Capabilities().Workspace == session.WorkspaceLocalWorktree {
		archivedNamespace = runtimeNamespaceLocalTmux
	} else if archived.ToInstanceData().IsRemoteHook() {
		archivedNamespace = runtimeNamespaceRemoteHook
	}
	newTitle, err := m.uniqueArchivedTitleLocked(repoID, repoPath, oldTitle, program, archivedNamespace, *diskData)
	if err != nil {
		return nil, err
	}
	newDest, err := archivedWorktreePath(repoID, newTitle)
	if err != nil {
		return nil, err
	}
	// The rollback destination is the path the durable record actually claims,
	// captured before the move rewrites it — not a fresh derivation from the
	// title. A pre-upgrade archive can sit on a leaf sanitizeArchiveTitle no
	// longer produces (a title exactly archiveLeafNameMax bytes took the literal
	// path before the strict digest boundary), so re-deriving it would move the
	// worktree somewhere the restored record does not name and report the
	// rollback as done while the row points at a missing directory.
	origDest := archived.GetWorktreePath()

	// The branch moves aside with the title (#2127). Freeing the title alone left
	// the archived session holding <prefix><oldTitle>, which is exactly the branch
	// the new session derives — so the create the rename enabled then failed at
	// `git worktree add`, having already renamed the archived session for nothing.
	//
	// Empty when there is nothing safe to move, and reclaimArchivedBranchLocked
	// owns that judgement; RenameArchived treats empty as "leave the branch alone",
	// which is also the right answer for a workspace that has no local branch.
	origBranch := archived.GetBranch()
	newBranch := m.reclaimArchivedBranchLocked(repoPath, archived, newTitle)

	// Relocate the archived worktree + move its branch + update the title
	// atomically on the instance. The wrapper names neither the worktree nor the
	// branch as the culprit — RenameArchived does either step and reports which one
	// failed in the wrapped error, so a fixed "failed to relocate its worktree"
	// prefix would mislabel a branch-rename failure as a worktree one (the P3 on
	// #2465).
	if err := archived.RenameArchived(newTitle, newDest, newBranch); err != nil {
		if errors.Is(err, git.ErrRelocateStateUnknown) {
			// Third caller of the bounded relocate, and the one with no rollback to
			// fall back on. Preserve both possible pathnames plus their captured
			// identity under the unchanged title. A later name-reuse retry resolves
			// them and completes registration before the logical rename.
			//
			// Persist under the UNCHANGED title, which is what the on-disk row still
			// keys on: the rename did not happen, only the location moved.
			//
			// persistInstanceData DIRECTLY, never m.persistInstance (#2106): we are
			// on reserveCreate's stack, which holds m.mu across this whole call, and
			// the wrapper re-enters m.mu via startLockForRepo. See the longer note
			// on the same call below for why this is safe and why the repo start
			// lock must not be taken here either.
			if perr := persistInstanceData(repoID, archived.ToInstanceData()); perr != nil {
				return nil, fmt.Errorf("cannot free the archived name %q for reuse, and its worktree recovery candidates could not be recorded (%v); recovery location: %s — inspect those path(s) before restarting the daemon: %w", oldTitle, perr, worktreeRecoveryLocation(archived), err)
			}
			return nil, fmt.Errorf("cannot free the archived name %q for reuse; both possible worktree locations are recorded (%s), and retrying will resolve them: %w", oldTitle, worktreeRecoveryLocation(archived), err)
		}
		return nil, fmt.Errorf("cannot free the archived name %q for reuse: %w", oldTitle, err)
	}
	// Re-key the manager map so the archived row is addressable under its new title.
	newKey := daemonInstanceKey(repoID, newTitle)
	delete(m.instances, oldKey)
	m.instances[newKey] = archived

	// Persist the rename durably. On failure, roll the worktree + in-memory identity
	// back so disk and reality stay consistent (mirrors the archive commit rollback,
	// #1538): otherwise the on-disk record would point at the pre-rename path that no
	// longer exists, stranding the archive after a daemon restart.
	renamed := archived.ToInstanceData()
	if perr := reuseArchivedRenamePersist(repoID, oldTitle, renamed); perr != nil {
		// The rollback puts the BRANCH back too, or the archived row would be
		// restored to its old title and path while still holding the moved-aside
		// branch — the split state this rollback exists to prevent, just relocated
		// from the worktree to the branch.
		if rbErr := archived.RenameArchived(oldTitle, origDest, origBranch); rbErr != nil {
			// Could not move the worktree home: leave it re-keyed under the new title
			// (the bytes live at newDest) and surface both failures so the operator can
			// recover it. The new session create aborts.
			//
			// persistInstanceData DIRECTLY, never m.persistInstance (#2106): we are on
			// reserveCreate's stack, which holds m.mu across this whole call, and
			// m.persistInstance -> persistInstanceErr -> startLockForRepo takes m.mu
			// again. sync.Mutex is not reentrant, so that self-deadlocked the goroutine
			// on the manager lock and hung every other daemon operation behind it.
			//
			// Do NOT "fix" this by grabbing the repo start lock without m.mu either.
			// CreateSession holds repoStartLock across its body and takes m.mu under it
			// (the appendInstanceData critical section), so repoStartLock->m.mu is the
			// established order; adding m.mu->repoStartLock here would close an ABBA
			// cycle — the #2006 lock-inversion class, traded for the self-deadlock.
			// Nothing is lost by skipping it: the per-repo start lock serializes
			// spawn-then-persist sequences, while what actually serializes writers to
			// instances.json is the file lock inside config.UpdateRepoInstances, which
			// persistInstanceData takes on its own.
			//
			// This is the one site that calls persistInstanceData bare while holding
			// m.mu; the other callers reach it with m.mu NOT held (CreateTab under
			// the repo start lock, CloseTab under only its per-session
			// op lock), so do not read them as precedent for the call shape here. What
			// makes it safe is a property of the primitive rather than of any wrapper:
			// persistInstanceData never re-enters m.mu, which is exactly where the old
			// persistInstance -> persistInstanceErr -> startLockForRepo path went wrong.
			//
			// Best-effort by design: this is a recovery breadcrumb on an already-failing
			// path, so a write failure is logged rather than returned — the operator
			// error below is the real report. It commonly WILL fail with "not found in
			// storage", because the durable rewrite that just failed is what would have
			// moved the on-disk row to the new title; that is pre-existing behavior this
			// fix deliberately does not change, and the returned error covers it.
			if wErr := persistInstanceData(repoID, archived.ToInstanceData()); wErr != nil {
				m.warn().Printf("archived session %q was renamed to %q but could not be persisted or rolled back; recording its new identity also failed: %v",
					oldTitle, archived.Title, wErr)
			}
			return nil, fmt.Errorf("failed to durably rename archived session %q and could not roll it back (%v); it may need manual recovery: %w", oldTitle, rbErr, perr)
		}
		delete(m.instances, newKey)
		m.instances[oldKey] = archived
		return nil, fmt.Errorf("failed to durably rename archived session %q to free the name; rolled it back: %w", oldTitle, perr)
	}

	// Reflect the rename in the caller's diskData snapshot so the subsequent
	// title-availability check for the NEW session no longer sees the old record.
	for i := range *diskData {
		if (*diskData)[i].Title == oldTitle {
			(*diskData)[i] = renamed
			break
		}
	}
	m.info().Printf("renamed archived session %q -> %q (repo %s) to free the name for a new session", oldTitle, newTitle, repoID)
	return &renamed, nil
}

// findArchivedOnlyCollisionLocked returns the ONE loaded archived instance whose
// title collides with `title`, together with its manager-map key — but only when
// it is the sole claim across reservations, loaded instances, and durable rows,
// and only when no exclusive operation is already running against it (#2779).
// A live/reserved collision returns nil so ordinary availability validation
// reports it. Multiple claims return an error immediately: renaming an arbitrary
// loaded winner would mutate user state and still leave the requested runtime
// name unavailable.
// Runs under m.mu.
func (m *Manager) findArchivedOnlyCollisionLocked(repoID, repoPath, title string, namespace runtimeNameNamespace, diskData []session.InstanceData) (*session.Instance, string, error) {
	for key := range m.reservedTitles {
		rid, existing := splitDaemonInstanceKey(key)
		if rid == repoID && m.titlesCollide(existing, title) {
			// A concurrent create is reserving a colliding name; let the
			// availability check reject with errConcurrentCreate.
			return nil, "", nil
		}
	}
	if namespace == runtimeNamespaceLocalTmux {
		nameKey := daemonInstanceKey(repoID, tmux.SanitizedNameForRepo(title, repoPath))
		if _, reserved := m.reservedTmuxNames[nameKey]; reserved {
			return nil, "", nil
		}
	}
	var archived *session.Instance
	var archivedKey string
	for key, inst := range m.instances {
		rid, _ := splitDaemonInstanceKey(key)
		if rid != repoID || inst == nil {
			continue
		}
		bothUseLocalTmux := namespace == runtimeNamespaceLocalTmux && inst.Capabilities().Workspace == session.WorkspaceLocalWorktree
		if m.titleCollisionNamespace(repoPath, inst.Title, title, bothUseLocalTmux) == titleNamespaceNone {
			continue
		}
		if inst.GetLiveness() != session.LiveArchived {
			// A live session still holds the name — do not rename around it.
			return nil, "", nil
		}
		if archived != nil {
			return nil, "", fmt.Errorf("cannot reuse session name %q: archived sessions %q and %q both claim its runtime namespace; rename or permanently delete one before retrying",
				title, archived.Title, inst.Title)
		}
		archived = inst
		archivedKey = key
	}
	if archived == nil {
		// A disk-only claim will be rejected by the ordinary availability check.
		// With no loaded archived row there is nothing this helper could mutate,
		// so leave that path's established diagnostic in charge.
		return nil, "", nil
	}

	// An exclusive lifecycle operation already owns this archived session, so it
	// is not a free name to rename around (#2779). killsInFlight is that fence:
	// restore, kill, archive and the root-kill path each claim it under m.mu
	// before touching a session, and the reuse rename — which relocates a
	// worktree, rewrites a durable record and re-keys the manager map — is every
	// bit as exclusive, yet it was the one such mutation that never asked.
	//
	// What that cost: RestoreArchived claims the fence, takes the per-session op
	// lock, and then RELEASES m.mu before moving the worktree — it has to, because
	// the move is a bounded git subprocess and no manager-wide lock may be held
	// across it. reserveCreate holds m.mu across the rename, but m.mu was never
	// what the two contended on. Both ended up inside
	// GitWorktree.relocateWorktreeTo on the SAME worktree object, which has no
	// internal synchronization: one call reads the source path the other is
	// rewriting, and their filesystem steps interleave. In the observed ordering
	// the create won, the restoring session was renamed to "<title> (archived)"
	// with its worktree moved out from under the restore, and the restore then
	// failed against a source that no longer existed — the user's uncommitted work
	// left in an archive directory nothing had asked to move.
	//
	// Reading the fence HERE is what makes the check sound rather than
	// approximate. m.mu is the only thing ordering the two: reserveCreate holds it
	// unbroken from this read through the rename, so a claim either lands before
	// this read — and refuses the create — or cannot land until the rename has
	// finished and re-keyed the row, where the claimant's own
	// `m.instances[key] != instance` recheck catches it. There is no third
	// interleaving.
	//
	// It lives in this shared helper rather than in renameArchivedForReuseLocked
	// for the same reason #2415 moved the record-independent checks up: all three
	// callers ask this function whether an archived row may be renamed, and the
	// two that run BEFORE the rename are what turn this into a side-effect-free
	// refusal instead of one discovered after the worktree had already moved.
	//
	// Refusing, rather than quietly returning "no collision": the ordinary
	// availability error would say the title is taken without saying that
	// something is actively doing something about it. Naming the in-flight
	// operation is what makes "retry in a moment" the obvious next step.
	if _, busy := m.killsInFlight[archivedKey]; busy {
		return nil, "", fmt.Errorf("cannot reuse session name %q: an operation is already in progress for the archived session %q; retry once it finishes",
			title, archived.Title)
	}

	// diskData contains the persisted copy of the loaded archived row as well as
	// rows refreshLocked could not materialize. Consume exactly ONE matching copy
	// of the loaded row; every other colliding non-Loading record is an independent
	// namespace claim. Checking it before RenameArchived is load-bearing: the
	// later availability check also sees disk-only rows, but by then the archive's
	// worktree, title, manager key, and storage row have already been rewritten.
	matchedPersistedCopy := false
	for _, data := range diskData {
		bothUseLocalTmux := namespace == runtimeNamespaceLocalTmux && data.UsesLocalTmux()
		if m.titleCollisionNamespace(repoPath, data.Title, title, bothUseLocalTmux) == titleNamespaceNone || data.Status == session.Loading {
			continue
		}
		if !matchedPersistedCopy && data.Title == archived.Title && data.ID == archived.ID {
			matchedPersistedCopy = true
			continue
		}
		return nil, "", fmt.Errorf("cannot reuse session name %q: archived session %q and stored session %q both claim its runtime namespace; rename or permanently delete one before retrying",
			title, archived.Title, data.Title)
	}
	return archived, archivedKey, nil
}

// uniqueArchivedTitleLocked returns the first free disambiguated title for an
// archived session being renamed out of the way: "<base> (archived)", then
// "<base> (archived 2)", "(archived 3)", … skipping any that collide with an
// existing live or archived session (feat: reuse archived name). Runs under m.mu.
//
// No worktree-branch check here (#2091): this walk renames an EXISTING archived
// session, which keeps the branch it already has checked out. The new title is a
// label, not a branch to be created, so "is this branch checked out somewhere" is
// not a question about it.
func (m *Manager) uniqueArchivedTitleLocked(repoID, repoPath, base, program string, namespace runtimeNameNamespace, diskData []session.InstanceData) (string, error) {
	// Bound the base so the " (archived N)" suffix survives branch truncation and
	// each rung derives a DISTINCT branch; a long base otherwise collapses every
	// rung to the same branch and the walk spins to 10,000 (#2528). Availability
	// here is judged via TitlesCollide -> BranchForTitle even though no branch is
	// created for the rename, so the same injectivity is required.
	base = git.BoundTitleForDisambiguation(base)
	var archiveNames archiveDirectoryNames
	if namespace == runtimeNamespaceLocalTmux {
		var err error
		archiveNames, err = archiveRelocationSnapshot(repoID)
		if err != nil {
			return "", err
		}
	}
	for i := 1; i <= 10000; i++ {
		candidate := fmt.Sprintf("%s (archived)", base)
		if i > 1 {
			candidate = fmt.Sprintf("%s (archived %d)", base, i)
		}
		err := m.validateTitleAvailableLocked(repoID, repoPath, candidate, program, namespace, false, diskData, false)
		if err == nil && namespace == runtimeNamespaceLocalTmux {
			err = archiveNames.validateArchiveRelocationDestination(candidate)
		}
		if err == nil {
			return candidate, nil
		}
		if errors.Is(err, errTitleCheckFatal) {
			return "", err
		}
	}
	return "", fmt.Errorf("could not find an available archived name for %q", base)
}
