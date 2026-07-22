package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
)

func (m *Manager) CreateSession(ctx context.Context, req CreateSessionRequest) (session.InstanceData, error) {
	// Own the create's lifetime: cancel derives a child context that is cancelled
	// the instant this returns (success, failure, or panic), so the readiness poll
	// StartAndSendPrompt runs can never outlive the create and keep capturing the
	// pane — the amp hang, where a create that never reached ready left a poll
	// spinning under the per-repo start lock and pinned the daemon. A caller
	// context cancelled early (an abandoned create) tears it down even sooner.
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if req.Program == "" {
		// Default from the repo-resolved config so an in-repo default_program
		// applies to daemon-created sessions (task runs, API creates) too.
		//
		// This is the ONE place the no-explicit-program default is decided, and
		// ListPrograms (#1970) answers by calling the same function rather than
		// restating the precedence — so the program a picker labels "repo default"
		// cannot disagree with the one a real create picks.
		req.Program = defaultProgramFor(m.cfg.DefaultProgram, req.RepoPath)
	}
	repo, title, release, renamedArchived, err := m.reserveCreate(req)
	if err != nil {
		return session.InstanceData{}, err
	}
	defer release()

	// reserveCreate may have renamed a colliding archived session to free this
	// title (feat: reuse archived name). Publish its new name onto the events plane
	// so the TUI + web rail relabel the archived row (it stays selectable/restorable
	// under the new title). Done after reserveCreate released m.mu so the fan-out
	// never runs under the manager lock.
	if renamedArchived != nil {
		m.publishEvent(agentproto.EventSessionUpdated, *renamedArchived)
	}

	// Publish the daemon's real in-flight state before anything that can be slow:
	// waiting behind another create in this repo, provisioning docker/ssh/hook in
	// NewInstance, creating the local worktree, and waiting for agent readiness.
	// A raw projection lives separately from m.instances because off-box runtime
	// provisioning happens inside NewInstance — there may be no concrete Instance
	// to register yet. It still carries the final stable id and creation time, which
	// the completed Instance inherits below, so clients upsert rather than replacing
	// one identity with another.
	createdAt := time.Now()
	pending := session.InstanceData{
		ID:            session.NewInstanceID(),
		TaskID:        req.TaskID,
		Title:         title,
		Path:          repo.Root,
		Status:        session.Loading,
		Liveness:      session.LiveReady,
		InFlightOp:    session.OpCreating,
		TaskRunActive: req.TaskID != "",
		CreatedAt:     createdAt,
		UpdatedAt:     createdAt,
		Prompt:        req.Prompt,
		Program:       req.Program,
		Worktree:      session.GitWorktreeData{RepoPath: repo.Root},
	}
	key := daemonInstanceKey(repo.ID, title)
	m.mu.Lock()
	m.pendingCreates[key] = pending
	m.mu.Unlock()
	m.publishEvent(agentproto.EventSessionUpdated, pending)

	// Tracks whether the provisional client row was replaced by any durable
	// outcome, not merely whether CreateSession returns nil. Retained failures are
	// real rows too: deleting their provisional identity from live clients would
	// hide the only handle that can inspect or clean up the uncertain workspace.
	creatingProjectionSettled := false
	defer func() {
		m.mu.Lock()
		delete(m.pendingCreates, key)
		m.mu.Unlock()
		if !creatingProjectionSettled {
			// Delete-class events are id-keyed, so a client removes exactly the
			// provisional row even when another repo has the same title. A missed
			// event is repaired by Snapshot, which no longer contains the pending row.
			m.publishEvent(agentproto.EventSessionKilled, session.InstanceData{ID: pending.ID, Title: title})
		}
	}()
	settleRetainedCreate := func(instance *session.Instance) {
		creatingProjectionSettled = true
		m.publishEvent(agentproto.EventSessionUpdated, instance.ToInstanceData())
	}

	repoStartLock := m.startLockForRepo(repo.ID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()

	// Session environment grants are security-sensitive and configurable while
	// the daemon is running. Reload them for every new create rather than pinning
	// the values that happened to be present when this Manager started.
	currentConfig, err := config.LoadConfig()
	if err != nil {
		return session.InstanceData{}, fmt.Errorf("load current session environment configuration: %w", err)
	}

	instance, err := session.NewInstance(session.InstanceOptions{
		ID:                             pending.ID,
		CreatedAt:                      pending.CreatedAt,
		Title:                          title,
		TaskID:                         req.TaskID,
		Path:                           repo.Root,
		Program:                        req.Program,
		InPlace:                        req.InPlace,
		ForceRemote:                    req.ForceRemote,
		Backend:                        session.BackendKind(req.Backend),
		ProvisionSessionEnvPassthrough: append([]string(nil), currentConfig.SessionEnvPassthrough...),
	})
	if err != nil {
		return session.InstanceData{}, err
	}

	conversationCapture := session.BeginConversationCapture()

	// Single creation flow (#930 PR 3): every instance owns its worktree 1:1.
	// InPlace only changes WHICH worktree that is — the repo's own working tree,
	// marked external — not the flow itself. finishCreateStart marks the instance
	// live, PARKS it at a usage-limit wall (#1146 PR4), or returns a fatal error.
	if serr := finishCreateStart(instance, req.Prompt, task.StartAndSendPrompt(ctx, instance, req.Prompt)); serr != nil {
		// An unknown startup outcome is already a teardown boundary. Launch may have
		// failed because the name it probed is not the name tmux stored; asking Kill
		// through that same binding can then answer "absent" for the wrong name and
		// delete a worktree whose real pane is still using it (#2207). A second probe
		// cannot turn "I do not know" into proof that the session never started, so
		// do not attempt cleanup in that case. Keep an inert, durable record of the
		// uncertain workspace instead; unlike a kill tombstone, it never schedules an
		// automatic retry through that suspect identity.
		if session.TeardownStateUnknown(serr) {
			if keepErr := m.keepUncertainCreate(repo.ID, title, instance); keepErr != nil {
				return session.InstanceData{}, fmt.Errorf("failed to start instance %q, and its startup outcome could not be determined safely — its workspace may still be on disk at %s and could not be recorded, so it must be inspected and cleaned up by hand: %w",
					title, instance.GetWorktreePath(), errors.Join(serr, keepErr))
			}
			settleRetainedCreate(instance)
			return session.InstanceData{}, fmt.Errorf("failed to start instance %q, and its startup outcome could not be determined safely, so its workspace was left in place; the session is recorded for inspection and no automatic cleanup will run: %w",
				title, serr)
		}

		// The create failed, so this instance would normally be discarded — it was
		// never registered or persisted, and the deferred release() hands its title
		// straight back out. That is only safe if startup was known not to have left a
		// runtime and cleanup actually removed what the create built (#1917/#2207).
		//
		// Kill swallows everything tmux and git ANSWER for, so an error here means it
		// could NOT: a pane whose liveness is unknown, or a worktree removal cut off
		// mid-delete. Releasing the title over those leftovers means the next create
		// with this name collides with — or removes — a workspace nobody can address,
		// since no record points at it. So keep the record instead: it holds the title
		// and gives the user something to inspect and kill.
		// The SAME classifier deleteSessionRecord uses (#1917 round 7). A non-nil
		// Kill is not enough: a remote create failure returns the in-sandbox
		// endpoint's error even when the sandbox teardown SUCCEEDED, so the
		// workspace is already gone — tombstoning a row, holding the title and
		// telling the user a workspace may remain would all be false.
		killErr := instance.Kill()
		if killErr != nil && !session.TeardownStateUnknown(killErr) {
			log.WarningLog.Printf("create of session %q: cleanup reported an error that does not leave its workspace state unknown; discarding the session as normal: %v", title, killErr)
		}
		if session.TeardownStateUnknown(killErr) {
			if keepErr := m.keepFailedCreate(repo.ID, title, instance); keepErr != nil {
				return session.InstanceData{}, fmt.Errorf("failed to start instance %q, and its cleanup could not complete safely — its workspace may still be on disk at %s and could not be recorded, so it must be cleaned up by hand: %w",
					title, instance.GetWorktreePath(), errors.Join(serr, killErr, keepErr))
			}
			settleRetainedCreate(instance)
			return session.InstanceData{}, fmt.Errorf("failed to start instance %q, and its cleanup could not complete safely, so its workspace was left in place; the session is recorded and the daemon will keep retrying the cleanup — it will clear once that succeeds: %w",
				title, errors.Join(serr, killErr))
		}
		return session.InstanceData{}, fmt.Errorf("failed to start instance: %w", serr)
	}
	data := instance.ToInstanceData()

	// Register the in-memory instance and persist it to disk inside the
	// same critical section. The daemon refresh loop rebuilds
	// session.Instance objects from disk for any key it does not already
	// see in m.instances, so a window where the entry exists on disk but
	// not in memory would let refresh construct a duplicate Instance
	// (opening a fresh PTY in the tmux backend) that gets orphaned when
	// the original is later stored under the same key.
	persistErr := func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.instances[key] = instance
		if err := appendInstanceData(repo.ID, data); err != nil {
			delete(m.instances, key)
			return err
		}
		return nil
	}()
	if persistErr != nil {
		// Same rule as the start-failure path above, minus the remedy: the record
		// write is what just failed, so keeping a record is not available. Report the
		// leftovers loudly instead of silently releasing the title over them (#1917).
		if killErr := instance.Kill(); session.TeardownStateUnknown(killErr) {
			return session.InstanceData{}, fmt.Errorf("failed to record session %q, and its cleanup could not complete safely — its workspace may still be on disk at %s and must be cleaned up by hand: %w",
				title, instance.GetWorktreePath(), errors.Join(persistErr, killErr))
		}
		return session.InstanceData{}, persistErr
	}
	m.captureAgentConversationAsync(repo.ID, key, instance, conversationCapture)
	creatingProjectionSettled = true
	// Publish from the Manager, not only the control-server wrapper: task delivery
	// and root-agent ensure call Manager.CreateSession directly. They announced the
	// same pending row above and therefore must settle it on the same events plane.
	m.publishEvent(agentproto.EventSessionCreated, data)

	return data, nil
}

func (m *Manager) reserveCreate(req CreateSessionRequest) (*config.RepoContext, string, func(), *session.InstanceData, error) {
	if req.RepoPath == "" {
		return nil, "", nil, nil, fmt.Errorf("repo path is required")
	}
	repo, err := config.RepoFromPath(req.RepoPath)
	if err != nil {
		return nil, "", nil, nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refreshLocked(); err != nil {
		return nil, "", nil, nil, err
	}

	diskData, err := loadRepoInstanceData(repo.ID)
	if err != nil {
		return nil, "", nil, nil, err
	}

	// Admission control for a task's session-per-event deliveries (#1892), read-
	// only and placed BEFORE any title mutation. refreshLocked above populated
	// m.instances, so the count sees every session already on disk — which is what
	// makes the cap survive a daemon restart with sessions still in flight. Running
	// it here, ahead of the archived-name-reuse rename below, means a refusal never
	// leaves an archived session renamed for a create that then did not happen. The
	// matching reserveTaskRunLocked runs only once the create is committed to
	// succeeding; m.mu is held unbroken between the two, so the count cannot move in
	// the gap. On refusal the watch-task delivery path parks the event on the
	// durable queue and retries when a slot frees, so nothing is dropped by the cap.
	if err := m.admitTaskRunLocked(repo.ID, req.TaskID, req.MaxConcurrentRuns); err != nil {
		return nil, "", nil, nil, err
	}

	// Resolve the runtime once for every namespace decision below. Hook creates
	// claim a global external slug; local creates claim a repo-scoped tmux name;
	// docker/ssh claim neither on this host. ForceRemote is only one way to select
	// hook, so ask the same resolver NewInstance uses.
	runtimeKind := session.BackendLocal
	if req.ForceRemote {
		runtimeKind = session.BackendHook
	}
	if kind, kerr := session.BackendKindFor(session.InstanceOptions{
		Backend:     session.BackendKind(req.Backend),
		ForceRemote: req.ForceRemote,
	}, repo.Root); kerr == nil {
		runtimeKind = kind
	}
	// A kerr means an invalid backend value. Leave the conservative default above
	// and let NewInstance surface the canonical error rather than duplicating it.
	nameNamespace := runtimeNamespaceForKind(runtimeKind)

	var renamedArchived *session.InstanceData
	title := req.Title
	if title == "" {
		base := req.TitleBase
		if base == "" {
			return nil, "", nil, nil, fmt.Errorf("session title is required")
		}
		// A derived title_base keeps auto-suffixing around every existing session,
		// archived rows included — the archived-name-reuse rename is reserved for an
		// EXPLICIT title the caller asked for by name (below).
		title, err = m.nextAvailableTitleLocked(repo.ID, repo.Root, base, req.Program, nameNamespace, diskData)
		if err != nil {
			return nil, "", nil, nil, err
		}
	} else {
		// When the requested title is held ONLY by an archived session, rename that
		// archived session out of the way so the new session can take the name
		// (feat: reuse archived name). A LIVE collision is left untouched, so
		// validateTitleAvailableLocked below still rejects it exactly as before.
		//
		// Ahead of that rename, refuse when the branch this create would derive is
		// already checked out somewhere (#2127) — freeing the title does not free
		// the branch, and moving that guaranteed failure EARLIER is the whole point:
		// discovering it at `git worktree add` leaves the archived session renamed
		// for a create that then did not happen, which is exactly the state the
		// admission comment above promises this function never produces.
		if err := m.refuseHeldBranchReuseLocked(repo.ID, repo.Root, title, nameNamespace, req.InPlace, diskData); err != nil {
			return nil, "", nil, nil, err
		}
		renamedArchived, err = m.renameArchivedForReuseLocked(repo.ID, repo.Root, title, req.Program, nameNamespace, &diskData)
		if err != nil {
			return nil, "", nil, nil, err
		}
		if err := m.validateTitleAvailableLocked(repo.ID, repo.Root, title, req.Program, nameNamespace, req.allowReserved, diskData); err != nil {
			return nil, "", nil, nil, err
		}
	}

	key := daemonInstanceKey(repo.ID, title)
	tmuxReservationKey := ""
	if nameNamespace == runtimeNamespaceLocalTmux {
		tmuxReservationKey = daemonInstanceKey(repo.ID, tmux.SanitizedNameForRepo(title, repo.Root))
	}
	remoteName := ""
	if nameNamespace == runtimeNamespaceRemoteHook {
		// Keyed by the BARE slug on purpose: it is the exact string the hook
		// scripts receive as --name, and that namespace is global (see
		// reservedRemoteNames).
		remoteName = session.Slugify(title)
		if _, ok := m.reservedRemoteNames[remoteName]; ok {
			return nil, "", nil, nil, fmt.Errorf("remote hook name %q is already reserved", remoteName)
		}
	}

	// Everything that could refuse this create has now passed (admission above,
	// title/remote-name availability). Record the concurrency reservation on the
	// committed-to-succeed path so no later error return leaks it (reserveCreate
	// returns the release() only on success); m.mu has been held unbroken since
	// admitTaskRunLocked, so the count is exactly what admission saw.
	m.reservedTitles[key] = struct{}{}
	if tmuxReservationKey != "" {
		if m.reservedTmuxNames == nil {
			m.reservedTmuxNames = make(map[string]string)
		}
		m.reservedTmuxNames[tmuxReservationKey] = title
	}
	if remoteName != "" {
		m.reservedRemoteNames[remoteName] = struct{}{}
	}
	m.reserveTaskRunLocked(repo.ID, req.TaskID, req.MaxConcurrentRuns)
	release := func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.reservedTitles, key)
		if tmuxReservationKey != "" {
			delete(m.reservedTmuxNames, tmuxReservationKey)
		}
		if remoteName != "" {
			delete(m.reservedRemoteNames, remoteName)
		}
		// CreateSession defers release(), so this runs only after the new instance
		// is registered in m.instances and counts against the cap on its own —
		// handing the slot over with no gap. On a failed create nothing was
		// registered, and dropping the reservation is exactly the right refund.
		m.releaseTaskRunLocked(repo.ID, req.TaskID)
	}

	return repo, title, release, renamedArchived, nil
}

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
	return fmt.Errorf("cannot create session %q: the archived session %q still has branch %q checked out at %s, and the new session would derive that same branch. Renaming the archived session aside frees its name but not its branch, so the create would fail at `git worktree add` — permanently delete the archived session to release both (%s), or create this session under a different name",
		title, archived.Title, branch, config.ShellQuotePath(holder),
		shellsuggest.Command("af", "sessions", "kill", archived.Title))
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

	// Relocate the archived worktree + update the title atomically on the instance.
	if err := archived.RenameArchived(newTitle, newDest); err != nil {
		return nil, fmt.Errorf("cannot free the archived name %q for reuse: failed to relocate its worktree: %w", oldTitle, err)
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
		if rbErr := archived.RenameArchived(oldTitle, origDest); rbErr != nil {
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
// it is the sole claim across reservations, loaded instances, and durable rows.
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
	for i := 1; i <= 10000; i++ {
		candidate := baseTitle
		if i > 1 {
			candidate = fmt.Sprintf("%s-%d", baseTitle, i)
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

func (m *Manager) validateTitleAvailableLocked(repoID, repoPath, title, program string, namespace runtimeNameNamespace, allowReserved bool, diskData []session.InstanceData) error {
	// Whitespace-only titles (e.g. "   ") are non-empty and so slip past a bare
	// == "" check, creating sessions with effectively blank names (#973). Trim
	// before the emptiness gate; the TUI naming flow applies the same check.
	if strings.TrimSpace(title) == "" {
		return fmt.Errorf("session title is required")
	}
	// The "root" title belongs to the daemon-managed root agent (#1106).
	// Every creation path lands here — TUI, `af sessions create`, task
	// spawns, DeliverPrompt auto-creates — so this single gate reserves the
	// name everywhere. Only the daemon's own ensure loop passes
	// allowReserved; title-base derivation (nextAvailableTitleLocked) never
	// does, so a base of "root" skips to "root-2" instead of erroring.
	if !allowReserved && session.IsReservedTitle(title) {
		return fmt.Errorf("session title %q is reserved for the daemon-managed root agent; pick another name (to run a root agent on this repo, add it to root_agents in ~/.agent-factory/config.json)", title)
	}
	// Titles are sanitized into git branch names (git.SanitizeBranchName
	// lowercases, turns spaces into dashes, strips unsafe chars, and collapses
	// dashes), so distinct titles can map to the same branch: "MyApp"/"myapp"
	// (#605) or "A B"/"a-b" (#741) both collide. The second worktree create
	// would otherwise fail with a cryptic git error, so reject the conflict
	// here, before any worktree or tmux setup runs.
	if existing, kind, collisionNamespace := m.findTitleConflictLocked(repoID, repoPath, title, namespace == runtimeNamespaceLocalTmux, diskData); existing != "" {
		switch {
		case existing == title:
			if kind == titleConflictReserved {
				return fmt.Errorf("session with title %q is already reserved: %w", title, errConcurrentCreate)
			}
			return fmt.Errorf("session with title %q already exists: %w", title, errConcurrentCreate)
		case collisionNamespace == titleNamespaceTmux:
			return fmt.Errorf("session titled %q conflicts with existing session %q: both map to tmux session %q", title, existing, tmux.SanitizedNameForRepo(title, repoPath))
		default:
			return fmt.Errorf("session titled %q conflicts with existing session %q: both sanitize to the same git branch %q", title, existing, m.branchForTitle(title))
		}
	}
	if namespace == runtimeNamespaceRemoteHook {
		candidate := session.Slugify(title)
		// Hook names are the ONE namespace that stays global while titles go
		// per-repo: launch_cmd/delete_cmd receive `--name <slug>` verbatim, with
		// no repo component, and external provisioners tag and reap real
		// VMs/containers by it. Two repos handing scripts the same name would
		// clobber one sandbox and let either delete reap the other's. So every
		// check below spans ALL repos, unlike the per-repo title rules above.
		if _, ok := m.reservedRemoteNames[candidate]; ok {
			return fmt.Errorf("remote hook name %q is already reserved", candidate)
		}
		// Guard against in-memory remote sessions that are not (yet) on disk.
		// refreshDaemonInstances preserves a running remote instance in
		// m.instances even after its repo directory is deleted externally (a
		// recoverable inconsistency), yet loadRepoInstanceData returns nothing
		// for it — so a disk-only slug check would let a second title that
		// slugifies to the same hook name through. The branch-collision check
		// above misses this pair because Slugify drops underscores while branch
		// sanitization keeps them as dashes ("My_App"->branch "my-app"/slug
		// "myapp" vs "MyApp"->branch "myapp"/slug "myapp"). The TUI pre-check
		// (FindSlugCollision over Snapshot()) catches it, but the HTTP
		// CreateSession path bypasses that, so the daemon-side check must be
		// complete (#1636).
		for _, inst := range m.instances {
			if inst == nil {
				continue
			}
			data := inst.ToInstanceData()
			if !data.IsRemoteHook() {
				continue
			}
			if session.Slugify(data.Title) == candidate {
				return fmt.Errorf("remote session titled %q already maps to hook name %q", data.Title, candidate)
			}
		}
		for _, data := range diskData {
			if !data.IsRemoteHook() {
				continue
			}
			if session.Slugify(data.Title) == candidate {
				return fmt.Errorf("remote session titled %q already maps to hook name %q", data.Title, candidate)
			}
		}
		// diskData holds only THIS repo's rows, so a settled hook session in
		// another repo would otherwise slip through — the hole that let two repos
		// create the same hook name sequentially (they just could not race).
		owner, ownerRepo, err := hookSlugOwnerInOtherRepos(candidate, repoID)
		if err != nil {
			return err
		}
		if owner != "" {
			return fmt.Errorf("remote session titled %q in project %s already maps to hook name %q; remote hook names are shared across projects because the hook scripts receive them verbatim as --name — pick another title for this remote session", owner, ownerRepo, candidate)
		}
		return nil
	}
	if namespace != runtimeNamespaceLocalTmux {
		return nil
	}
	tmuxSession := tmux.NewTmuxSessionForRepo(title, repoPath, program)
	// Existence gates the create here, so read the tri-state, not the lossy bool
	// (#1962): only a CONFIRMED existing session (known && exists) blocks. A
	// wedged/timed-out has-session is NOT proof of a name collision — reporting
	// "exists" through ExistsOrUnknown would refuse a legitimate create against a
	// merely-wedged server. An unanswered probe (!known) falls through and lets the
	// create proceed. That never silently clobbers a real orphan: the create's own
	// `tmux new-session -s <name>` fails on a duplicate name, and Start's existence
	// check (now ProbeSession too) surfaces it as "already exists" once the server
	// answers, or as ErrTmuxTimeout while it stays wedged — either way non-
	// destructive. Blocking here on a guess is the only unsafe option.
	if exists, known := tmuxSession.ProbeSession(); known && exists {
		// A tmux session exists with no daemon reservation, in-memory instance,
		// or disk record — an orphan left by a crash or an external process.
		// No creator will ever finish it, so this stays a plain error (not
		// errConcurrentCreate): DeliverPrompt must fail fast with cleanup
		// guidance rather than wait out waitForTargetSession's timeout (#916).
		return fmt.Errorf("conflicting tmux session %q is already running; no agent-factory session owns it. Clean it up with: %s", title, shellsuggest.Command("tmux", "kill-session", "-t", tmuxSession.SanitizedName()))
	}
	return nil
}

// hookSlugOwnerInOtherRepos reports the title (and repo path) of a persisted
// remote-hook session in ANY repo other than repoID whose slug equals candidate,
// or "" when the hook name is free.
//
// The caller already scans this repo's rows and every in-memory instance; this
// covers the remaining case — a settled hook session in a different repo, whose
// rows loadRepoInstanceData(repoID) never returns. Without it the global
// hook-name namespace is only enforced against concurrent creates (the in-flight
// reservation), so two repos could take the same name sequentially and hand both
// sandboxes the identical --name.
//
// Corrupted per-repo files are surfaced rather than skipped: a hidden hook
// session would otherwise let a colliding name through, and the cost of a false
// refusal here is a clear error, while the cost of a miss is two provisioned
// sandboxes fighting over one name.
func hookSlugOwnerInOtherRepos(candidate, repoID string) (string, string, error) {
	allInstances, err := config.LoadAllRepoInstances()
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", errTitleCheckFatal, err)
	}
	var corrupted []string
	for rid, raw := range allInstances {
		if rid == repoID {
			continue
		}
		var rows []session.InstanceData
		if err := json.Unmarshal(raw, &rows); err != nil {
			corrupted = append(corrupted, rid)
			continue
		}
		for i := range rows {
			if !rows[i].IsRemoteHook() {
				continue
			}
			if session.Slugify(rows[i].Title) == candidate {
				return rows[i].Title, rows[i].Path, nil
			}
		}
	}
	if len(corrupted) > 0 {
		sort.Strings(corrupted)
		return "", "", fmt.Errorf("%w: cannot verify remote hook name %q is free: %d repo(s) have a corrupted instances.json that may be hiding a session using it: %s",
			errTitleCheckFatal, candidate, len(corrupted), strings.Join(corrupted, ", "))
	}
	return "", "", nil
}

// errTitleCheckFatal marks a title-availability failure that is NOT "this
// candidate is taken" but "the check itself could not be completed" — today, a
// corrupted instances.json that might be hiding a hook session using the name.
//
// The distinction is load-bearing for nextAvailableTitleLocked, which walks
// candidates (base, base-2, base-3 …) and reads ANY error as "taken, try the
// next". Without the marker a fatal error makes it burn all 10,000 candidates
// while holding the manager lock and then report a misleading "could not find an
// available title", swallowing the actionable corruption message. Callers check
// errors.Is and surface it instead of suffixing around it.
var errTitleCheckFatal = errors.New("cannot verify title availability")

// branchesHeldByWorktrees is the git query worktreeHeldBranchesLocked runs. A
// package var so tests can force the probe to FAIL in isolation — the answer
// that must NOT block a create (#2127) — without breaking the repo out from
// under the rest of the create path, which needs it readable to get that far.
// Mirrors reuseArchivedRenamePersist's precedent. Production points it at the
// real query and never reassigns it.
var branchesHeldByWorktrees = git.BranchesHeldByWorktrees

// worktreeHeldBranchesLocked answers "which branches are already checked out by
// a worktree of this repo" for the title walk (#2091), mapping each to the
// worktree holding it. Runs under m.mu.
//
// Two deliberate non-answers:
//
//   - Hook sessions (remote) never take a local worktree — backend_local is the
//     only caller of NewGitWorktree — so no local branch can block their name,
//     and probing the repo for them would be answering a question nobody asked.
//   - A probe that could not RUN returns nil, not an empty map with a shrug.
//     Nil means "no holds known", which leaves the pre-#2091 behavior exactly as
//     it was: the create proceeds, and if the name really is held, `git worktree
//     add` refuses it loudly and changes nothing. That is the right failure for
//     an unanswerable question. The destructive reading would be to treat an
//     unreadable repo as "everything is held" and walk a recurring task's name
//     to an ever-growing suffix on the strength of a probe that never answered.
func (m *Manager) worktreeHeldBranchesLocked(repoPath string, remote bool) map[string]string {
	if remote {
		return nil
	}
	held, err := branchesHeldByWorktrees(repoPath)
	if err != nil {
		log.WarningLog.Printf("could not list worktree branch holds for %s; resolving the session title without them (a name an archived worktree holds will fail at worktree add instead of being skipped): %v", repoPath, err)
		return nil
	}
	return held
}

// branchForTitle derives the git branch name for a session title using the same
// prefix and sanitization the git worktree layer applies, so the daemon can
// detect branch collisions before worktree setup runs.
func (m *Manager) branchForTitle(title string) string {
	return git.BranchForTitle(m.cfg.BranchPrefix, title)
}

// keepFailedCreate registers and persists an instance whose create FAILED but
// whose cleanup could not complete safely, so its tmux and/or worktree are still
// on disk (#1917).
//
// A create normally discards a failed instance and lets reserveCreate's release()
// hand the title back out — correct, because the cleanup removed everything the
// create built. When the cleanup could NOT complete, that same release puts the
// title back in circulation on top of live leftovers that no record points at, so
// the next create under that name collides with or deletes them.
//
// The record is TOMBSTONED, not merely written. Retention is a claim on two other
// layers, and a row that just sits there satisfies neither (#1917 round 5):
//
//   - SaveInstances drops any non-started, non-Archived instance on the next
//     wholesale checkpoint — which fires whenever ANY other started session in the
//     repo is saved. An untombstoned row here would be silently erased, orphaning
//     the leftovers it exists to hold. The tombstone is what makes that writer keep
//     it.
//   - Nothing else would ever finish the cleanup. refreshInstanceStatus routes a
//     tombstoned record to finishUserKill on every poll, which retries the teardown
//     and drops the record once it completes safely — so the leftovers are reaped
//     when the cause clears, rather than waiting on the user.
//
// The tombstone is honest here: it records "a teardown is committed for this
// record; finish it, never restore it", which is exactly what a failed create's
// cleanup is. Mirrors the register-then-persist ordering of the success path — the
// map entry goes in first so the refresh loop cannot build a duplicate Instance from
// disk, and is rolled back if the write fails. The caller holds the repo start lock,
// so this appends directly rather than going through persistKillTombstone (which
// takes that same non-reentrant lock).
func (m *Manager) keepFailedCreate(repoID, title string, instance *session.Instance) error {
	instance.MarkUserKilled()
	return m.persistFailedCreate(repoID, title, instance)
}

// keepUncertainCreate retains a create whose runtime may exist under an identity
// af could not confirm. It deliberately does NOT mark a kill tombstone: the
// daemon must not retry teardown automatically through the same suspect binding
// and let a false "absent" answer authorize workspace deletion (#2207).
func (m *Manager) keepUncertainCreate(repoID, title string, instance *session.Instance) error {
	instance.MarkStartupStateUnknown()
	return m.persistFailedCreate(repoID, title, instance)
}

func (m *Manager) persistFailedCreate(repoID, title string, instance *session.Instance) error {
	key := daemonInstanceKey(repoID, title)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.instances[key] = instance
	if err := appendInstanceData(repoID, instance.ToInstanceData()); err != nil {
		delete(m.instances, key)
		return err
	}
	return nil
}
