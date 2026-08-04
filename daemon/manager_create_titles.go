package daemon

import (
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Archived-title and held-branch reuse for session creation (#1145 split out of
// manager_create.go, which crossed the 1000-line limit). These helpers all run
// under the manager lock and answer one question between them: can this title be
// used, and if not, what should be done with the archived record holding it.

// refuseHeldBranchReuseLocked refuses an explicit-title create BEFORE the
// archived-name-reuse rename touches anything, when the branch the new session
// would derive is already checked out by a registered worktree (#2127). Runs
// under m.mu.
//
// renameArchivedForReuseLocked frees a TITLE. It does not free a BRANCH: per
// #2013 archiving relocates a worktree and repairs its registration rather than
// removing it, so the archived session keeps <prefix><title> checked out, and
// the new session derives that same branch. `git worktree add` then refuses it
// and the create fails — with the archived session already renamed out of the
// way for a create that never happened, the one state reserveCreate's own
// comment promises a refusal never leaves behind.
//
// So ask git first. This does not make reuse-archived-name WORK for a local
// session — only releasing or relocating the branch can, and that changes what
// happens to a user's branches, so it is a separate call tracked on #2127. What
// it does is turn a state-corrupting failure into an honest one that names the
// blocker and how to clear it.
//
// Three deliberate non-firings:
//
//   - No archived collision means renameArchivedForReuseLocked will not rename
//     anything, so there is no invariant to protect. The create proceeds and
//     fails at `git worktree add` exactly as before. Widening this into a
//     general "is the branch free" gate over every explicit title would refuse
//     creates that have nothing to do with this bug.
//   - Hook and --here creates never derive <prefix><title> at all: a hook
//     session takes no local worktree (backend_local is the only caller of
//     NewGitWorktree), and --here attaches to the repo's OWN working tree at ITS
//     current branch (NewGitWorktreeInPlace). A hold on a branch the create will
//     not use must not block it.
//   - A probe that could not RUN yields nil holds, and nil must never refuse.
//     "I could not ask git" is not "the branch is held"; treating it as one
//     would block a legitimate reuse on the strength of an answer git never
//     gave — the fabricated-negative failure this repo keeps paying for. On a
//     failed probe the create proceeds and, if the branch really is held, fails
//     loudly at `git worktree add`: precisely the pre-guard behavior, which
//     destroyed nothing. Only a branch git POSITIVELY reports as held refuses.
func (m *Manager) refuseHeldBranchReuseLocked(repoID, repoPath, title string, namespace runtimeNameNamespace, inPlace bool, diskData []session.InstanceData) error {
	if namespace != runtimeNamespaceLocalTmux || inPlace {
		return nil
	}
	archived, _, err := m.findArchivedOnlyCollisionLocked(repoID, repoPath, title, namespace, diskData)
	if err != nil {
		return err
	}
	if archived == nil {
		return nil
	}
	branch := m.branchForTitle(title)
	// Indexing a nil map is the nil-probe path: not held, so no refusal.
	holder, held := m.worktreeHeldBranchesLocked(repoPath, false)[branch]
	if !held {
		return nil
	}
	// The branch is held — but as of #2127 the rename can often take it with it,
	// which is the whole point of the durable fix. Refuse only when it cannot.
	//
	// This asks the same question renameArchivedForReuseLocked will act on, rather
	// than a cheaper approximation of it: if the two could disagree, the guard
	// would either refuse a reuse that would have worked, or wave through one that
	// then failed at `git worktree add` with the archived session already renamed —
	// the exact state this function exists to prevent.
	newTitle, terr := m.uniqueArchivedTitleLocked(repoID, repoPath, archived.Title, archived.Program, namespace, diskData)
	if terr == nil && m.reclaimArchivedBranchLocked(repoPath, archived, newTitle) != "" {
		return nil
	}
	return fmt.Errorf("cannot create session %q: the archived session %q still has branch %q checked out at %s, and the new session would derive that same branch. Its branch cannot be moved aside automatically (it is published, externally owned, or its state could not be determined), so freeing the name would not free the branch and the create would fail at `git worktree add` — permanently delete the archived session to release both (%s), or create this session under a different name",
		title, archived.Title, branch, config.ShellQuotePath(holder),
		shellsuggest.Command("af", "sessions", "kill", archived.Title))
}

// reclaimArchivedBranchLocked decides the branch name the archived session moves
// to when its title is reused, or "" for "leave the branch where it is" (#2127).
//
// This is the durable half of the fix. #2129 shipped an honest refusal because
// freeing a title never freed the branch; moving the branch aside WITH the title
// makes the reclaim complete, so reuse-archived-name actually works for a local
// session instead of always refusing.
//
// Instance.ArchivedBranchForReclaim owns whether the archived session's branch may
// be touched at all (not local, external, published, or unknown — it declines, and
// its doc comment carries the reasoning). This adds the two questions that are the
// daemon's to answer: what the new name should be, and whether it is free.
//
// Deriving the new branch from the new TITLE rather than suffixing the old branch
// keeps the archived row internally coherent: after the move its title, worktree
// directory, and branch all say the same thing, which is what a later restore
// presents to the user.
func (m *Manager) reclaimArchivedBranchLocked(repoPath string, archived *session.Instance, newTitle string) string {
	current, ok := archived.ArchivedBranchForReclaim()
	if !ok {
		return ""
	}
	candidate := m.branchForTitle(newTitle)
	if candidate == "" || candidate == current {
		return ""
	}
	held := m.worktreeHeldBranchesLocked(repoPath, false)
	// Only move a branch that is actually BLOCKING, and this narrowness is the
	// point rather than caution. A freed archived branch (its worktree detached, as
	// after a `git checkout --detach`) is not in anyone's way: `git worktree add
	// <path> <branch>` attaches the new session to it, which is the shipped
	// behaviour where reuse-archived-name already completes and where the new
	// session deliberately continues the archived branch's history.
	//
	// Renaming there would change that outcome — the new session would get a fresh
	// branch off base and the old history would move to a name nobody asked for —
	// which is a semantic change #2127 never asked for. #2127 is about the case
	// where the branch is HELD and the create therefore CANNOT proceed at all.
	if _, blocking := held[current]; !blocking {
		return ""
	}
	// The candidate name must be genuinely FREE, and "not checked out" is not the
	// same as "free": `git branch -m` refuses to rename onto ANY existing branch,
	// idle or held. Checking only the checked-out map (the P3 on #2465) let a plain
	// branch of the candidate's name through the guard, after which the rename
	// failed with exit 128 — the guard having promised a name it had not actually
	// cleared. BranchExists closes that, and its unknown answer declines, because a
	// name that cannot be ruled out must be treated as taken rather than renamed
	// onto.
	if _, taken := held[candidate]; taken {
		return ""
	}
	if !archived.ArchivedCandidateBranchIsFree(candidate) {
		return ""
	}
	return candidate
}

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
func (m *Manager) refuseUnclaimableTitleReuseLocked(repoID, repoPath, title, program string, namespace runtimeNameNamespace, allowReserved bool, diskData []session.InstanceData) error {
	archived, _, err := m.findArchivedOnlyCollisionLocked(repoID, repoPath, title, namespace, diskData)
	if err != nil || archived == nil {
		return err
	}
	return m.validateTitleClaimableLocked(repoID, repoPath, title, program, namespace, allowReserved, diskData, archived)
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
	origDest, err := archivedWorktreePath(repoID, oldTitle)
	if err != nil {
		return nil, err
	}

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
			// fall back on. A deadline tripped after the bytes reached newDest, so
			// the worktree object points there while the TITLE was never changed —
			// RenameArchived returns before that step — and everything below
			// (re-key, persist) is skipped. Without this, the durable row keeps
			// pointing at the old, now-missing archive directory and a restart
			// strands the worktree, exactly as archive and restore would have.
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
				return nil, fmt.Errorf("cannot free the archived name %q for reuse, and its worktree's new location %s could not be recorded (%v); check that path before restarting the daemon: %w", oldTitle, archived.GetWorktreePath(), perr, err)
			}
			return nil, fmt.Errorf("cannot free the archived name %q for reuse; its worktree was left at %s and recorded there, so it is not lost: %w", oldTitle, archived.GetWorktreePath(), err)
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
			// m.mu; the other callers reach it with m.mu NOT held (CreateTab and
			// SetPRInfo under the repo start lock, CloseTab under only its per-session
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
				log.WarningLog.Printf("archived session %q was renamed to %q but could not be persisted or rolled back; recording its new identity also failed: %v",
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
	log.InfoLog.Printf("renamed archived session %q -> %q (repo %s) to free the name for a new session", oldTitle, newTitle, repoID)
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
	for i := 1; i <= 10000; i++ {
		candidate := fmt.Sprintf("%s (archived)", base)
		if i > 1 {
			candidate = fmt.Sprintf("%s (archived %d)", base, i)
		}
		err := m.validateTitleAvailableLocked(repoID, repoPath, candidate, program, namespace, false, diskData)
		if err == nil {
			return candidate, nil
		}
		if errors.Is(err, errTitleCheckFatal) {
			return "", err
		}
	}
	return "", fmt.Errorf("could not find an available archived name for %q", base)
}

func (m *Manager) nextAvailableTitleLocked(repoID, repoPath, baseTitle, program string, namespace runtimeNameNamespace, diskData []session.InstanceData) (string, error) {
	// Shape errors belong to the base, not to any candidate's availability.
	// Validate once before the suffix walk so controls do not burn all 10,000
	// rungs and whitespace cannot turn into a punctuation-only "   -2" title.
	// allowReserved stays true here because a base of "root" deliberately skips
	// the reserved bare candidate and resolves to "root-2" below.
	if err := m.validateTitleShapeLocked(baseTitle, namespace, true); err != nil {
		return "", err
	}
	// Session records are not the only thing that can make a candidate unusable
	// (#2091). A branch already CHECKED OUT by some worktree cannot be checked
	// out again, and archiving relocates a worktree rather than removing it
	// (#2013), so an archived session keeps its branch — under a path no record
	// points at once its row has been renamed. The rot that produced: a daily
	// task walked to a suffix its own archived predecessor still held, `git
	// worktree add` refused it, and the task died that way every run, forever.
	// So ask the one component that knows which branches are checked out
	// somewhere, ONCE per walk, and skip those rungs instead of discovering the
	// collision at add time.
	heldBranches := m.worktreeHeldBranchesLocked(repoPath, namespace != runtimeNamespaceLocalTmux)
	// Bound the base before suffixing so a long title's "-N" survives branch
	// truncation and each rung derives a DISTINCT branch; otherwise every rung
	// collapses to the taken base's branch, and the walk skips all 10,000 rungs
	// under m.mu before failing (#2528). The bare base (i == 1) is unchanged.
	boundedBase := git.BoundTitleForDisambiguation(baseTitle)
	for i := 1; i <= 10000; i++ {
		candidate := baseTitle
		if i > 1 {
			candidate = fmt.Sprintf("%s-%d", boundedBase, i)
		}
		branch := m.branchForTitle(candidate)
		if holder, held := heldBranches[branch]; held {
			log.InfoLog.Printf("title %q derives branch %q, which the worktree at %s still has checked out; trying the next suffix",
				candidate, branch, holder)
			continue
		}
		err := m.validateTitleAvailableLocked(repoID, repoPath, candidate, program, namespace, false, diskData)
		if err == nil {
			return candidate, nil
		}
		// A check that could not RUN is not a taken candidate: no suffix would fare
		// any better, so surface the actionable error instead of spinning through
		// 10,000 of them under the lock.
		if errors.Is(err, errTitleCheckFatal) {
			return "", err
		}
	}
	return "", fmt.Errorf("could not find an available title for %q", baseTitle)
}
