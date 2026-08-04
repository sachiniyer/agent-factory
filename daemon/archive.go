package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/task"
)

// ErrAlreadyArchived is returned by ArchiveSession when its target is already in
// the archived state. It is a SENTINEL — not just prose — because "already
// archived" is the one archive rejection that means the caller's goal is already
// met: a bulk caller that just wants the session archived (DeleteProject, #2108)
// must be able to tell it apart from a genuine failure (busy, op in flight,
// teardown error) with errors.Is, and count it as success instead of reporting a
// misleading partial failure. Only in-process callers can match it; over the
// control RPC it degrades to its (unchanged) message.
var ErrAlreadyArchived = errors.New("already archived")

// archiveTargetTasksChecked is a test seam for pinning the interval between the
// task snapshot and the archive fence. Production leaves it as a no-op.
var archiveTargetTasksChecked = func() {}

// loadTasksForRepoID is a test seam for proving lifecycle operations do not
// repeatedly reload and freshly resolve the same project task set.
var loadTasksForRepoID = task.LoadTasksForRepoIDWithBindingUpdates

// enabledTasksTargetingSession returns enabled tasks in repoID whose canonical
// target is exactly title. Names are sorted for a deterministic, complete
// archive refusal. A load error stays unknown and must fail the archive before
// any session mutation.
func (m *Manager) enabledTasksTargetingSession(repoID, title string) ([]task.Task, error) {
	targets, err := m.loadEnabledTaskTargets(repoID)
	if err != nil {
		return nil, err
	}
	return targets[title], nil
}

// loadEnabledTaskTargets loads and freshly scopes a project's task store once,
// then indexes every enabled target. DeleteProject reuses this snapshot for its
// preflight and each subsequent archive while holding taskTargetMu, so projects
// with many sessions do not repeat tasks x path-resolution work.
func (m *Manager) loadEnabledTaskTargets(repoID string) (map[string][]task.Task, error) {
	tasks, bindingUpdates, err := loadTasksForRepoID(repoID)
	// Publish BEFORE propagating: the load returns backfilled bindings alongside
	// a scope error (task.LoadTasksForRepoIDWithBindingUpdates), and those rows
	// were already committed durably. Dropping them on the error branch is not a
	// no-op — nothing re-publishes them, so a push-only client keeps a stale
	// repository scope for a projection the server has made authoritative.
	for _, updated := range bindingUpdates {
		m.publishEvent(agentproto.EventTaskUpdated, updated)
	}
	if err != nil {
		return nil, err
	}
	targets := make(map[string][]task.Task)
	for _, t := range tasks {
		target := task.CanonicalTargetSession(t.TargetSession)
		if t.Enabled && target != "" {
			targets[target] = append(targets[target], t)
		}
	}
	for target := range targets {
		targeted := targets[target]
		sort.Slice(targeted, func(i, j int) bool {
			if targeted[i].Name == targeted[j].Name {
				return targeted[i].ID < targeted[j].ID
			}
			return targeted[i].Name < targeted[j].Name
		})
	}
	return targets, nil
}

func describeTargetTasks(tasks []task.Task) string {
	parts := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if strings.TrimSpace(t.Name) == "" {
			parts = append(parts, fmt.Sprintf("task %s", t.ID))
			continue
		}
		parts = append(parts, fmt.Sprintf("%q (task %s)", t.Name, t.ID))
	}
	return strings.Join(parts, ", ")
}

// ArchiveSession archives a session (#1028): it tears down the session's tmux
// (agent + shell/process tabs; web tabs have none and are preserved with their
// URLs, #1809) while PRESERVING the record, relocates the
// worktree to the global archive dir (<AF_HOME>/archived/<repoID>/<title>/), and
// marks the instance Archived. The instance stays in the manager map as an inert
// row (unlike Kill, which deletes it); a later RestoreArchived brings it back.
//
// Concurrency mirrors KillSession: it registers in killsInFlight (so a
// concurrent kill or a second archive is rejected, and the Lost-restore /
// finish-kill passes skip it) and holds the per-session op-lock (so archive,
// kill, and Lost-recovery never interleave). Returns the relocated worktree's
// new path.
// ArchiveSession archives the resolved session and returns the relocated worktree
// path AND the full committed projection of the session it ACTUALLY resolved and
// acted on. It publishes that projection before releasing the per-session operation
// lock so a later restore cannot overtake it, and clients learn Archived directly
// instead of depending on a second Snapshot request (#2680). Its stable id still
// prevents cross-repo title misrouting (#1592 Phase 5).
func (m *Manager) ArchiveSession(req ArchiveSessionRequest) (string, session.InstanceData, error) {
	m.taskTargetMu.Lock()
	defer m.taskTargetMu.Unlock()
	return m.archiveSession(req, nil)
}

// archiveSession performs ArchiveSession while taskTargetMu is already held.
// DeleteProject uses it inside its wider, preflight-to-commit task transaction
// and passes the project-wide target index it loaded once during preflight.
// A nil taskTargets means the caller has not supplied a serialized snapshot.
func (m *Manager) archiveSession(req ArchiveSessionRequest, taskTargets map[string][]task.Task) (string, session.InstanceData, error) {
	instance, repoID, title, _, _, err := m.resolveActionSession(req.ID, req.Title, req.RepoID)
	if err != nil {
		return "", session.InstanceData{}, err
	}
	// Canonicalize to the resolved session's title so every guard, the
	// killsInFlight key, and the relocation key off the id-resolved identity,
	// not the request's title. req is a value copy, so this is local.
	req.Title = title
	if session.IsReservedTitle(req.Title) {
		return "", session.InstanceData{}, fmt.Errorf("cannot archive the reserved %q session", req.Title)
	}
	if instance == nil {
		// A ghost disk record with no live instance has no in-memory worktree to
		// relocate; there is nothing coherent to archive.
		return "", session.InstanceData{}, fmt.Errorf("cannot archive session %q: it is not currently active", req.Title)
	}
	if instance.StartupStateUnknown() {
		return "", session.InstanceData{}, fmt.Errorf("cannot archive session %q: its startup state is unknown, so af cannot safely move its workspace through an unconfirmed runtime binding; inspect it and explicitly remove it instead", req.Title)
	}
	if !instance.Capabilities().Archive {
		return "", session.InstanceData{}, fmt.Errorf("cannot archive remote session %q: it has no local worktree to relocate", req.Title)
	}
	if instance.IsExternalWorktree() {
		// An in-place/external worktree (`af sessions create --here`, #1107 — also
		// how root is set up) IS the user's own working tree; archive relocates
		// the worktree, which MoveWorktree refuses for external worktrees. Reject
		// it HERE, in the upfront guard, so nothing is torn down for a session
		// that can never be archived — otherwise the rejection would only surface
		// in the move step, after tmux is already down, leaving a broken
		// half-archive that rolls back to Lost.
		return "", session.InstanceData{}, fmt.Errorf("cannot archive an in-place/external worktree session %q — archive relocates the worktree, which isn't supported for in-place sessions", req.Title)
	}
	if instance.GetLiveness() == session.LiveArchived {
		// Sentinel-wrapped, and returned WITH the resolved identity: a caller that
		// only wants the session archived (DeleteProject, #2108) can treat this as
		// idempotent success and still report the right {id, title}, while a caller
		// archiving one named session — the CLI/TUI verbs — keeps the same message
		// and the same failure it has always shown.
		return "", instance.ToInstanceData(), fmt.Errorf("session %q is %w", req.Title, ErrAlreadyArchived)
	}
	if instance.GetInFlightOp() != session.OpNone {
		return "", session.InstanceData{}, fmt.Errorf("session %q is busy (%v); try again in a moment", req.Title, instance.GetStatus())
	}

	// Archiving is deliberate quiescence. Never auto-restore the target behind
	// the user's back, and never silently disable an automation as an archive
	// side effect. Refuse once, before the archive fence or teardown, and name
	// every enabled task the caller must disable or retarget. This also makes
	// DeleteProject fail visibly for the affected session rather than leaving an
	// enabled task in the same permanent retry loop (#2646).
	targeted := taskTargets[req.Title]
	if taskTargets == nil {
		var taskErr error
		targeted, taskErr = m.enabledTasksTargetingSession(repoID, req.Title)
		if taskErr != nil {
			return "", session.InstanceData{}, fmt.Errorf("cannot archive session %q: could not determine whether enabled tasks target it; nothing was changed: %w", req.Title, taskErr)
		}
	}
	if len(targeted) > 0 {
		return "", session.InstanceData{}, fmt.Errorf("cannot archive session %q: enabled task(s) target it: %s; disable or retarget them, then archive again", req.Title, describeTargetTasks(targeted))
	}
	archiveTargetTasksChecked()

	key := daemonInstanceKey(repoID, req.Title)
	m.mu.Lock()
	if _, busy := m.killsInFlight[key]; busy {
		m.mu.Unlock()
		return "", session.InstanceData{}, fmt.Errorf("an operation is already in progress for session %q", req.Title)
	}
	m.killsInFlight[key] = struct{}{}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.killsInFlight, key)
		m.mu.Unlock()
	}()

	opLock, err := m.lockSessionOperationWithin(key, "archive", req.Title)
	if err != nil {
		return "", session.InstanceData{}, err
	}
	defer opLock.Unlock()

	// Re-verify under the op-lock: findSession released m.mu, so a racing kill
	// may have torn the session down (map entry gone/replaced) or tombstoned it.
	m.mu.Lock()
	current := m.instances[key]
	m.mu.Unlock()
	if current != instance || instance.UserKilled() {
		return "", session.InstanceData{}, fmt.Errorf("session %q changed state before archive could start", req.Title)
	}

	// An off-box session (docker/ssh/hook) archives by pushing its branch to origin
	// and reaping the sandbox, not by relocating a worktree it does not have (#1592
	// Phase 4 PR6). Route it to the remote body, which shares this method's guards,
	// locks, and archive fence.
	if instance.Capabilities().Workspace == session.WorkspaceRemote {
		archivedPath, rerr := m.archiveRemoteSession(repoID, instance, req.Title)
		if rerr != nil {
			return "", session.InstanceData{}, rerr
		}
		archived := instance.ToInstanceData()
		m.publishEvent(agentproto.EventSessionArchived, archived)
		return archivedPath, archived, nil
	}

	dest, err := archivedWorktreePath(repoID, req.Title)
	if err != nil {
		return "", session.InstanceData{}, err
	}
	// The pre-archive worktree location, captured before the move, so a persist
	// failure after the commit can roll the worktree back home (#1538).
	origPath := instance.GetWorktreePath()

	// Raise the archive fence through the chokepoint (#1195 Phase 2d): BeginArchive
	// sets OpArchiving (I4) so the status poll skips this instance (and the
	// checkpoint save skips persisting a mid-op row) while tmux is down and the
	// worktree is mid-move — never misread as Lost. started is left true
	// throughout so a move failure self-heals via the Lost loop. OpArchiving is a
	// DISTINCT op from a kill, so the fence can never be confused with a TUI
	// optimistic kill (#1187). The up-front guards (not archived, op==None) plus
	// the op-lock guarantee this edge is legal; a rejection would surface here
	// before any teardown.
	if err := instance.Transition(session.BeginArchive()); err != nil {
		return "", session.InstanceData{}, fmt.Errorf("cannot archive session %q: %w", req.Title, err)
	}

	// Stop this session's VS Code editor BEFORE the worktree moves. Ordering is
	// load-bearing for the same reason the pane-exit wait is: the editor's cwd is
	// the worktree, and moving a directory out from under a live code-server
	// leaves it serving a path that no longer exists. It is daemon-owned
	// infrastructure, not a tab, so ArchiveTeardown does not cover it. No-ops when
	// the session never had a vscode tab.
	//
	// The tab itself is KEPT by ArchiveTeardown (it is metadata-only: stopping the
	// editor destroys nothing the tab holds) and persisted with the archived
	// record, so a restore renders it again and the next proxy request lazily
	// respawns an editor on a fresh port. That is what makes stopping here safe —
	// it is a stop, not a delete.
	//
	// The final sweep is the belt to that brace: the webtab proxy may spawn an
	// editor without this op-lock (a spawn is too slow to hold it), so a request
	// racing this teardown could start one after the stop below. BeginArchive above
	// already makes ensureVSCodeServer refuse, so this should never fire — it is
	// here so the "archived ⇒ no editor" invariant does not depend on that being
	// true forever. Its result is checked below before the durable archive commit.
	vscodeKey := daemonInstanceKey(repoID, req.Title)
	if err := m.stopVSCodeForInstance(vscodeKey, instance.ID); err != nil {
		_ = instance.Transition(session.CancelArchive())
		m.persistInstance(repoID, instance)
		return "", session.InstanceData{}, fmt.Errorf("cannot archive session %q because its VS Code editor teardown could not be confirmed; no session teardown was started: %w", req.Title, err)
	}

	// Tear down tmux and relocate the worktree in one call: the move is folded
	// into the teardown core immediately after the pane-exit wait (#1195 Ph2b),
	// so no live pane is cwd'd in the worktree during the move (previously a
	// separate MoveArchivedWorktree step relying on duplicated ordering prose).
	hookErr, err := archiveTeardown(instance, dest, func() error {
		return runOnArchiveHook(onArchiveHookContext{
			sessionID:   instance.ID,
			title:       req.Title,
			repoRoot:    instance.GetRepoPath(),
			worktree:    origPath,
			archivePath: dest,
		})
	})
	if err != nil {
		// The worktree is still at a valid location (the git layer guarantees
		// worktreePath points at the actual bytes even on a repair failure).
		// Roll the fence back to Lost — started is still true and the agent tmux
		// binding was kept — so the Lost-restore loop re-spawns the agent in place.
		// Persist the recovery-eligible state, then surface the failure.
		//
		// The error-returning persist, for the same reason as the restore path
		// below: since the relocate became bounded, this branch is reachable with
		// the worktree already moved to dest and only the in-memory object knowing
		// it. A logged-and-swallowed write failure would leave the record pointing
		// at the pre-archive path, so a restart would send the Lost-restore loop to
		// rebuild a worktree whose bytes are sitting in the archive directory.
		//
		// The hook ran BEFORE the move, so this branch is reachable with both
		// failing. The move error stays the primary cause — it is what the caller
		// must act on — but the hook error is not redundant with it and must not be
		// dropped (#2763): a broken cleanup command is invisible otherwise, since
		// this path never reaches the committed-archive warning below, and every
		// later attempt fails the same way and reports the same single cause. Log
		// it as well as returning it, matching the committed path, so an archive
		// driven by a task still leaves the diagnosis in the daemon log.
		hookNote := ""
		if hookErr != nil {
			log.WarningLog.Printf("archive of session %q failed and its on-archive hook also failed: %v", req.Title, hookErr)
			hookNote = fmt.Sprintf(" (its on-archive hook also failed: %v)", hookErr)
		}
		_ = instance.Transition(session.AbortArchiveToLost())
		if perr := m.persistInstanceErr(repoID, instance); perr != nil {
			return "", session.InstanceData{}, fmt.Errorf("failed to archive session %q AND could not record its recovered state on disk (%v); its worktree is at %s — check that path before restarting the daemon: %w%s", req.Title, perr, instance.GetWorktreePath(), err, hookNote)
		}
		return "", session.InstanceData{}, fmt.Errorf("failed to archive session %q (its agent will be restored in place): %w%s", req.Title, err, hookNote)
	}

	// Success: worktree relocated, tmux down. Commit the inert Archived state
	// (started=false, op cleared) — reachable only from the fence (I2) — then
	// persist the new path + status DURABLY.
	//
	// Unlike the best-effort status poll, a swallowed persist failure here is
	// unsafe (#1538): the on-disk record would still point at the pre-archive
	// worktree with started=true, so after a daemon restart the Lost-restore loop
	// would try to rebuild the worktree at the old path and hit "branch already
	// checked out at <archivedPath>", orphaning the archive — the user can no
	// longer reach it via af. So on a persist failure, undo the physical archive:
	// move the worktree back home and drop the session to Lost, leaving the
	// on-disk record and reality consistent again and letting the #1108 loop heal
	// it in place. (The tiny window between the move and this persist completing —
	// a crash there — is inherent without a write-ahead journal; the reproducible
	// persist-error cause this issue reports is fully closed.)
	_ = instance.Transition(session.CommitArchive())
	archivedPath := instance.GetWorktreePath()
	if stopErr := m.stopVSCodeForInstance(vscodeKey, instance.ID); stopErr != nil {
		log.ErrorLog.Printf("archive of session %q: final VS Code editor teardown was not confirmed (%v); rolling back the moved worktree", req.Title, stopErr)
		if rbErr := m.undoCommittedArchive(repoID, instance, origPath); rbErr != nil {
			// The worktree cannot be returned home. Keep and best-effort persist the
			// committed archive rather than claiming a live Lost session at a path
			// that no longer exists, while surfacing both unknown outcomes.
			m.persistInstance(repoID, instance)
			return "", session.InstanceData{}, fmt.Errorf("archived session %q to %s but could not confirm its final VS Code editor teardown (%v) and could not roll the worktree back (%v); it may need manual recovery", req.Title, archivedPath, stopErr, rbErr)
		}
		return "", session.InstanceData{}, fmt.Errorf("could not confirm final VS Code editor teardown for session %q; rolled the archive back and left it Lost for retry: %w", req.Title, stopErr)
	}
	if perr := archivePersist(m, repoID, instance); perr != nil {
		log.ErrorLog.Printf("archive of session %q: failed to durably record the Archived state (%v); rolling back to keep the on-disk record consistent", req.Title, perr)
		if rbErr := m.undoCommittedArchive(repoID, instance, origPath); rbErr != nil {
			// Could not move the worktree home: the committed archive is the
			// safest remaining state. Persist it best-effort and surface both
			// failures so the operator can recover it manually.
			m.persistInstance(repoID, instance)
			return "", session.InstanceData{}, fmt.Errorf("archived session %q to %s but failed to record it durably (%v) and could not roll it back (%v); it may need manual recovery", req.Title, archivedPath, perr, rbErr)
		}
		return "", session.InstanceData{}, fmt.Errorf("failed to durably archive session %q; rolled it back and left it Lost to be restored in place: %w", req.Title, perr)
	}
	log.InfoLog.Printf("archived session %q (repo %s): tmux torn down, worktree moved to %s", req.Title, repoID, archivedPath)
	archived := instance.ToInstanceData()
	// Still inside opLock: lifecycle event order must match committed operation
	// order. Publishing after this method returns lets an immediate restore finish
	// and publish before this older Archived projection (#2680 Codex review).
	m.publishEvent(agentproto.EventSessionArchived, archived)
	if hookErr != nil {
		committedErr := &mutationCommittedError{err: fmt.Errorf(
			"archive committed, but on-archive hook failed: %w", hookErr)}
		log.WarningLog.Printf("%v", committedErr)
		return archivedPath, archived, committedErr
	}
	return archivedPath, archived, nil
}

// taskTargetValidationContext carries the root-agent reachability fact that can
// require git resolution. Control callers prepare it while holding taskTargetMu
// but before entering the tasks-file lock; validators must not shell out there.
type taskTargetValidationContext struct {
	rootRepoID          string
	rootWillMaterialize bool
}

func (m *Manager) prepareTaskTargetValidation(repoID, target string, enabled bool) taskTargetValidationContext {
	ctx := taskTargetValidationContext{}
	if enabled && repoID != "" && session.IsReservedTitle(target) {
		ctx.rootRepoID = repoID
		ctx.rootWillMaterialize = m.repoRootAgentWillMaterialize(repoID)
	}
	return ctx
}

// validateEnabledTaskTarget is the task-writer side of taskTargetMu's fence.
// Missing ordinary targets remain legal (delivery auto-creates them), but a
// reserved target needs a proven daemon materialization path. A known target
// that is archiving or already archived would retry forever and is refused
// before the task-store commit. RepoID has already been derived by the checked
// add path or supplied from a freshly resolved legacy update. Unknown identity
// stays unknown for enabled targeted writes; accepting it would bypass every
// repository-scoped lifecycle check.
func (m *Manager) validateEnabledTaskTarget(t task.Task, ctx taskTargetValidationContext) error {
	target := task.CanonicalTargetSession(t.TargetSession)
	if !t.Enabled || target == "" {
		return nil
	}
	// During restore, absence from m.instances is unknown rather than proof that
	// the target does not exist. Keep the historical warm-up allowance for
	// disabled/untargeted task writes, but fail target-dependent writes closed.
	if !m.Ready() {
		return errDaemonStarting()
	}
	if t.RepoID == "" {
		return fmt.Errorf("cannot determine project identity for enabled task %q target %q; nothing was changed", t.ID, target)
	}
	// Reserved root delivery is safe only while the daemon owns its future, not
	// merely because a process happens to exist now. A disabled root-agent policy
	// deliberately leaves a surviving live root alone, but will not recreate it
	// after the next kill/outage; accepting a task there would defer the permanent
	// failure until that disappearance.
	if session.IsReservedTitle(target) {
		if target != session.RootSessionTitle {
			return fmt.Errorf("cannot enable task %q: reserved target session %q cannot materialize under that spelling; use %q exactly; nothing was changed", t.ID, target, session.RootSessionTitle)
		}
		if ctx.rootRepoID != t.RepoID {
			return fmt.Errorf("cannot enable task %q: could not determine whether reserved target session %q will materialize because its project identity changed during validation; retry; nothing was changed", t.ID, target)
		}
		if !ctx.rootWillMaterialize {
			return fmt.Errorf("cannot enable task %q: target session %q is reserved, but af could not establish that its root agent will materialize (it may be unconfigured, unresolved, or its project may be deleted); configure or re-register the root agent and restart the daemon, or choose a different target; nothing was changed", t.ID, target)
		}
	}
	m.mu.Lock()
	instance := m.instances[daemonInstanceKey(t.RepoID, target)]
	m.mu.Unlock()
	loaded := instance != nil
	var state session.InstanceData
	if loaded {
		state = instance.ToInstanceData()
	} else {
		persisted, _, err := findInstanceDataByTitle(target, t.RepoID)
		if errors.Is(err, errSessionNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("cannot enable task %q: could not determine target session %q state from storage; nothing was changed: %w", t.ID, target, err)
		}
		state = *persisted
	}
	switch {
	case state.InFlightOp == session.OpArchiving:
		return fmt.Errorf("cannot enable task %q: target session %q is being archived; wait for the archive, then restore it or choose a different target", t.ID, target)
	case state.Liveness == session.LiveArchived:
		return fmt.Errorf("cannot enable task %q: target session %q is archived; restore it or choose a different target", t.ID, target)
	case !loaded:
		return fmt.Errorf("cannot enable task %q: could not determine whether target session %q can receive prompts; it is recorded on disk but unavailable in the daemon — repair or remove that session, or choose a different target; nothing was changed", t.ID, target)
	default:
		return nil
	}
}

// undoCommittedArchive rolls a committed-but-unpersisted archive back to a
// self-healing live state (#1538). It moves the worktree back to its pre-archive
// location (origPath) and returns the instance to a plain Lost with started=true,
// via the only legal edge path out of the committed Archived state: BeginRestore
// (Archived -> Lost + OpRestoring, started=true) then AbortRestoreToLost (op
// cleared). With the worktree home and the record dropped to Lost, the on-disk
// record — still the pre-archive one, since the archive persist failed — matches
// reality, so a daemon restart re-spawns at the right path and the #1108 restore
// loop heals the agent in place. The rolled-back state is persisted best-effort;
// even if that write also fails, the worktree being home already keeps disk and
// reality consistent. Returns an error only when the move home itself fails.
func (m *Manager) undoCommittedArchive(repoID string, instance *session.Instance, origPath string) error {
	if err := instance.RestoreArchivedWorktree(origPath); err != nil {
		return err
	}
	_ = instance.Transition(session.BeginRestore())
	_ = instance.Transition(session.AbortRestoreToLost())
	m.persistInstance(repoID, instance)
	return nil
}

// archiveRemoteSession archives an off-box session (docker/ssh/hook) by pushing
// its branch to origin then reaping the sandbox (#1592 Phase 4 PR6) — the remote
// analogue of ArchiveSession's worktree-move body, sharing its guards, locks, and
// archive fence. GitHub holds the durable branch, so unlike the local path there
// is no worktree to move and no rollback-home on a persist failure: an archived
// remote record is already recoverable from origin.
func (m *Manager) archiveRemoteSession(repoID string, instance *session.Instance, title string) (string, error) {
	// Raise the archive fence (OpArchiving) so the status poll skips this instance
	// while its sandbox is torn down, exactly as the local path does.
	if err := instance.Transition(session.BeginArchive()); err != nil {
		return "", fmt.Errorf("cannot archive session %q: %w", title, err)
	}

	branch, err := instance.ArchiveSandbox()
	if err != nil {
		// Push and/or teardown failed. Roll the fence back to Lost so the session
		// stays recovery-eligible, persist that, and surface the failure.
		_ = instance.Transition(session.AbortArchiveToLost())
		m.persistInstance(repoID, instance)
		return "", fmt.Errorf("failed to archive session %q: %w", title, err)
	}

	// Success: branch is durable on origin, sandbox reaped. Commit the inert
	// Archived state (started=false, op cleared) and persist it durably.
	_ = instance.Transition(session.CommitArchive())
	if perr := archivePersist(m, repoID, instance); perr != nil {
		// The sandbox is already reaped and the branch is on origin, so there is
		// nothing to undo — the Archived record is recoverable from origin either
		// way. Persist best-effort and surface the durability failure; even a lost
		// best-effort write leaves the on-disk record naming the pushed branch, so a
		// restart loads the session Lost and an explicit restore re-provisions it.
		log.ErrorLog.Printf("archive of remote session %q: failed to durably record the Archived state (%v); branch %q is on origin, so the session stays restorable", title, perr, branch)
		m.persistInstance(repoID, instance)
		return "", fmt.Errorf("archived remote session %q (branch %q pushed to origin) but failed to record it durably: %w", title, branch, perr)
	}
	log.InfoLog.Printf("archived remote session %q (repo %s): branch %q pushed to origin, sandbox reaped", title, repoID, branch)
	return branch, nil
}

// restoreRemoteSession restores an archived off-box session (docker/ssh/hook) by
// re-provisioning a fresh sandbox that clones the pushed branch back and
// relaunching the agent (#1592 Phase 4 PR6) — the remote analogue of
// RestoreArchived's worktree-move body, sharing its guards + locks. It reuses
// RestoreFromArchive unchanged: BeginRestore fences the restore, then the
// backend's Recover (recoverSandbox) re-provisions + relaunches + flips the
// session live. The code survives via origin; a fresh agent runs on the pushed
// branch (the pre-archive conversation lived only in the disposed sandbox).
func (m *Manager) restoreRemoteSession(repoID string, instance *session.Instance, title string) (string, error) {
	if err := instance.RestoreFromArchive(); err != nil {
		// On a re-provision/relaunch failure RestoreFromArchive left the instance
		// Lost; persist that and surface the failure (an explicit retry re-provisions
		// from the still-pushed branch).
		m.persistInstance(repoID, instance)
		return "", fmt.Errorf("failed to restore remote session %q (re-provisioning its sandbox): %w", title, err)
	}
	// A FRESH sandbox now backs this session, so its accumulated remote-loss
	// failures describe a sandbox that is gone (#1794). Reset BEFORE the persist
	// and log below, not after: RestoreFromArchive has already flipped the session
	// live and dropped OpRestoring, and the poll goroutine neither takes the
	// op-lock nor checks killsInFlight — so it can probe the new sandbox while
	// this call is still writing to disk, and a blip in that window would be
	// judged against the OLD sandbox's threshold-satisfying count and mark the
	// fresh runtime Lost. The instance keeps the same ID across the re-provision
	// (same session, new runtime), so nothing else can notice the swap; only this
	// site knows it happened.
	m.noteRuntimeReplaced(repoID, instance)
	m.persistInstance(repoID, instance)
	log.InfoLog.Printf("restored remote session %q (repo %s): fresh sandbox provisioned, branch cloned back, agent relaunched", title, repoID)
	return title, nil
}

// RestoreArchived restores an archived session (#1028): it moves the worktree
// back to where session creation would place it under the configured
// worktree_root (a free sibling path, or under $AF_HOME/worktrees for
// subdirectory users — #1540), re-registers it, re-spawns the agent, and marks
// the instance Running. It returns both the worktree path and the canonical
// stable identity it actually restored, so lifecycle events cannot be rebuilt
// from stale request display fields. The agent session is brought back, as are any web tabs
// (pure metadata: they were never torn down, so their URLs ride back on the
// record and render again, #1809); shell/process tabs were dropped at archive
// time. Returns the restored worktree path.
//
// Concurrency mirrors ArchiveSession/KillSession (killsInFlight + op-lock). On a
// repo-gone failure the archive is left intact with an actionable error; on a
// re-spawn failure the worktree is already back in place and the instance is
// left Lost so the #1108 restore loop heals it.
func (m *Manager) RestoreArchived(req RestoreArchivedRequest) (string, session.InstanceData, error) {
	instance, repoID, title, resolvedID, _, err := m.resolveActionSession(req.ID, req.Title, req.RepoID)
	if err != nil {
		return "", session.InstanceData{}, err
	}
	resolved := session.InstanceData{ID: resolvedID, Title: title}
	if instance == nil {
		return "", session.InstanceData{}, fmt.Errorf("cannot restore session %q: no such session", title)
	}
	path, restoreErr := m.restoreArchivedInstance(instance, repoID, title)
	return path, resolved, restoreErr
}

// restoreArchivedInstance performs the restore for an identity already resolved
// by either RestoreArchived or the liveness-agnostic RestoreSession route. Keeping
// one body avoids a second title lookup between those sibling paths.
func (m *Manager) restoreArchivedInstance(instance *session.Instance, repoID, title string) (string, error) {
	req := RestoreArchivedRequest{ID: instance.ID, Title: title, RepoID: repoID}
	if err := instance.ValidateRuntimeAction(session.RuntimeActionRestoreArchived); err != nil {
		return "", fmt.Errorf("cannot restore: %w", err)
	}

	key := daemonInstanceKey(repoID, req.Title)
	if err := m.claimRestoreOperation(repoID, key, req.Title); err != nil {
		return "", err
	}
	defer m.releaseRestoreOperation(key)

	opLock, err := m.lockSessionOperationWithin(key, "restore", req.Title)
	if err != nil {
		return "", err
	}
	defer opLock.Unlock()

	// Re-verify under the op-lock (findSession released m.mu).
	m.mu.Lock()
	current := m.instances[key]
	m.mu.Unlock()
	if current != instance {
		return "", fmt.Errorf("session %q changed state before restore could start", req.Title)
	}
	// Re-run the shared runtime-entry guard under the operation lock, before the
	// worktree move. A durable kill tombstone is terminal intent: restoring
	// around it would appear to succeed, then finishUserKill would reap the
	// replacement on the next poll (#2208).
	if err := instance.ValidateRuntimeAction(session.RuntimeActionRestoreArchived); err != nil {
		return "", fmt.Errorf("cannot restore: %w", err)
	}

	// An off-box session (docker/ssh/hook) restores by re-provisioning a fresh
	// sandbox that clones the pushed branch back, not by moving a worktree it does
	// not have
	// (#1592 Phase 4 PR6). Route it to the remote body, which shares this method's
	// guards + locks.
	if instance.Capabilities().Workspace == session.WorkspaceRemote {
		return m.restoreRemoteSession(repoID, instance, req.Title)
	}

	repoPath := instance.GetRepoPath()
	if repoPath == "" {
		return "", fmt.Errorf("cannot restore session %q: no repo path on record", req.Title)
	}
	// Repo-gone check up front: SiblingWorktreePath and the worktree move both
	// need the origin repo, so surface the actionable message (archive left
	// intact) before either fails with a generic error.
	if _, statErr := os.Stat(repoPath); statErr != nil {
		return "", fmt.Errorf("cannot restore session %q: its origin repo %s is gone; the archived worktree is intact at %s — recover it manually with git", req.Title, repoPath, instance.GetWorktreePath())
	}
	// Honor the configured worktree_root placement, exactly as session creation
	// does (#1540): a subdirectory user's worktree is restored under
	// $AF_HOME/worktrees/<branch>, not stranded beside the repo. The branch is
	// needed only for subdirectory placement.
	dest, err := sessiongit.RestoreWorktreePath(repoPath, req.Title, instance.GetBranch())
	if err != nil {
		return "", fmt.Errorf("cannot determine restore location for %q: %w", req.Title, err)
	}

	// Move the worktree back next to the repo. A repo-gone failure leaves the
	// archive intact (the git layer guarantees this) and surfaces an actionable
	// message; the instance stays Archived.
	if err := instance.RestoreArchivedWorktree(dest); err != nil {
		if errors.Is(err, sessiongit.ErrRepoGone) {
			return "", fmt.Errorf("cannot restore session %q: its origin repo is gone; the archived worktree is intact at %s — recover it manually with git: %w", req.Title, instance.GetWorktreePath(), err)
		}
		if errors.Is(err, sessiongit.ErrRelocateStateUnknown) {
			// The relocate was cut off by its deadline AFTER the bytes reached
			// dest: the git layer commits the new location to the worktree object
			// before repairing, precisely because the registration can be stale
			// while the location is not. That new location only exists in memory,
			// and this failure path returns without persisting — so a daemon
			// restart would reload the session as Archived pointing at an archive
			// path that is no longer there, and every later restore would fail the
			// source-exists guard while the user's work sat at dest.
			//
			// Persist first, then report — and take the error-returning persist,
			// not the log-and-continue one. The whole point of this branch is that
			// the location must survive a restart, so a write that did NOT happen
			// (full or read-only disk) reproduces the exact stranding it exists to
			// prevent. Reporting the relocate error alone would tell the operator
			// their worktree is safely recorded when nothing recorded it.
			restoredPath := instance.GetWorktreePath()
			if perr := m.persistInstanceErr(repoID, instance); perr != nil {
				return "", fmt.Errorf("restore of %q was cut off mid-relocate AND its new location %s could not be written to disk (%v); the worktree is there but nothing durable points at it — move it back or re-register it manually before restarting the daemon: %w", req.Title, restoredPath, perr, err)
			}
			return "", fmt.Errorf("restore of %q was cut off mid-relocate; its worktree is recorded at %s so it is not lost, but its git registration is unverified — check that path and retry: %w", req.Title, restoredPath, err)
		}
		return "", fmt.Errorf("failed to restore worktree for %q: %w", req.Title, err)
	}

	// Worktree is back in place. Re-spawn the agent and flip Running. On a
	// re-spawn failure RestoreFromArchive leaves the instance started + Lost, so
	// the Lost-restore loop keeps retrying against the now-restored worktree.
	if err := instance.RestoreFromArchive(); err != nil {
		m.persistInstance(repoID, instance)
		return "", fmt.Errorf("restored worktree for %q but failed to re-spawn its agent (it will be retried): %w", req.Title, err)
	}

	worktreePath := instance.GetWorktreePath()
	m.persistInstance(repoID, instance)
	log.InfoLog.Printf("restored session %q (repo %s): worktree moved back to %s, agent re-spawned", req.Title, repoID, worktreePath)
	return worktreePath, nil
}

// archiveExclusiveTabLock serializes a tab spawn against an archive/kill/restore
// teardown for the session keyed by key (#1195). Those ops hold the per-session
// op-lock across their whole tmux teardown + worktree move, and — unlike Kill,
// which flips started=false — an archive keeps started=true throughout (so the
// #1108 rollback can self-heal a failed move to Lost), which means the
// instance-level #990 started guard never fires during archive. Without this, a
// CreateTab racing an ArchiveSession could spawn a tmux session into the worktree
// being moved out from under it, orphaning it.
//
// It takes the op-lock BEFORE any per-repo start lock the caller holds, matching
// the opLock→repoStartLock ordering the kill/archive paths use (persistInstance
// et al), so no ABBA deadlock is introduced. On success it returns the LOCKED
// op-lock — the caller must Unlock it. On rejection it releases the lock and
// returns an error: an archive/kill that beat us to the op-lock has fully
// completed by the time we acquire it (it held the lock across its entire
// teardown+move), so a mid-archive Deleting is never observed — only the terminal
// Archived. A restored kill tombstone has no in-flight op-lock state and may
// still be started, so UserKilled must be checked explicitly under this newly
// acquired lock rather than left to the instance-level started guard.
func (m *Manager) archiveExclusiveTabLock(key string, instance *session.Instance) (*sync.Mutex, error) {
	opLock := m.opLockFor(key)
	opLock.Lock()
	if instance.UserKilled() {
		opLock.Unlock()
		return nil, fmt.Errorf("cannot create a tab on killed session %q", instance.Title)
	}
	if err := instance.TabSpawnBlocked(); err != nil {
		opLock.Unlock()
		return nil, err
	}
	return opLock, nil
}

// persistInstanceErr writes one instance's authoritative data through the
// targeted per-repo writer under the repo start lock, mirroring
// refreshInstanceStatus's persist, and returns any write error. persistInstance
// wraps it for the best-effort callers; the archive commit uses this variant to
// make the persist durable (#1538).
//
// LOCK CONTRACT (#2106): it goes through startLockForRepo, which takes m.mu, so
// it must NEVER be called with m.mu held — see startLockForRepo. Under m.mu, call
// persistInstanceData directly instead.
func (m *Manager) persistInstanceErr(repoID string, instance *session.Instance) error {
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()
	// Snapshot only AFTER serialization. Taking ToInstanceData as an argument to
	// a locking helper evaluates it before that helper acquires the lock, letting
	// a stale status write erase a kill tombstone (#1917).
	return persistInstanceData(repoID, instance.ToInstanceData())
}

// persistHandoffCheckpointErr is persistInstanceErr for the one writer whose
// durable and live projections differ ON PURPOSE: the post-swap handoff
// checkpoint. Disk takes the caller's atomic projection — the crash-recovery
// posture, which cannot retain a process-local operation — while memory keeps the
// OpReplacing delivery fence. It is intentionally separate from persistInstanceErr:
// ordinary writers must take their Instance snapshot only after acquiring this
// serialization lock, which is exactly why the caller's snapshot is a parameter here.
//
// The EVENT carries the live projection, not the checkpoint (#2782). Other clients
// have to render the truth — incoming agent, replacement still in flight — and the
// fence is what closes their action gates (CanHandoff/CanKill project off it).
// Announcing the checkpoint would tell them the swap had already settled and
// re-open gates the daemon is still holding shut.
func (m *Manager) persistHandoffCheckpointErr(repoID string, instance *session.Instance, checkpoint session.InstanceData) error {
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()
	err := persistInstanceData(repoID, checkpoint)
	m.publishEvent(agentproto.EventSessionUpdated, instance.ToInstanceData())
	return err
}

// persistInstance is the best-effort form of persistInstanceErr: a failed write
// only logs. Used everywhere the persist is a checkpoint that the next poll/tick
// will re-attempt, never where the write's durability gates correctness.
//
// LOCK CONTRACT (#2106): inherits persistInstanceErr's — never call it with m.mu
// held. Under m.mu, call persistInstanceData directly.
func (m *Manager) persistInstance(repoID string, instance *session.Instance) {
	if err := m.persistInstanceErr(repoID, instance); err != nil {
		log.WarningLog.Printf("failed to persist instance %q: %v", instance.Title, err)
	}
}

// persistAndPublishInstance is persistInstance plus the session.updated
// announcement a committed change owes every OTHER client (#1812, #2782).
//
// The two belong together wherever a mutation — not a poll — is what changed the
// session. A client that did not make the change learns of it ONLY from the events
// plane: it re-Snapshots after its own mutations, never after anyone else's. And
// the status poll cannot be relied on to repair the gap, because it deliberately
// skips a session with an operation in flight and, once the operation finishes,
// snapshots the already-final state as its own baseline — seeing no change to
// report.
//
// Published INSIDE the repo ordering lock, in the same critical section as the
// persist that produced it, for the reason spelled out at the poll's publish in
// limit.go: session.updated carries a WHOLE InstanceData and every client
// re-projects the session wholesale from it, so publishing after the unlock lets an
// older payload land last and revert newer state.
//
// The announcement goes out even when the write failed. That is deliberate and
// matches the poll: what the other clients must converge on is MEMORY, which has
// already changed. persistInstance is best-effort by contract — the next checkpoint
// retries the write — so a failed write is not a reason to leave every other client
// stale about a change that really happened.
//
// LOCK CONTRACT (#2106): inherits persistInstance's — never call it with m.mu held.
func (m *Manager) persistAndPublishInstance(repoID string, instance *session.Instance) {
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	// Snapshot only AFTER serialization, for the #1917 reason on persistInstanceErr:
	// a projection taken before the lock can erase state a writer committed while we
	// waited. The same snapshot is both persisted and announced, so disk and the
	// events plane can never disagree about which one this was.
	data := instance.ToInstanceData()
	err := persistInstanceData(repoID, data)
	m.publishEvent(agentproto.EventSessionUpdated, data)
	repoStartLock.Unlock()
	if err != nil {
		log.WarningLog.Printf("failed to persist instance %q: %v", instance.Title, err)
	}
}

// archivePersist is the durable persist ArchiveSession runs at its commit. A
// package var so tests can force a persist failure in isolation (exercising the
// rollback in #1538) without disturbing any other persist. Production points it
// at persistInstanceErr.
var archivePersist = (*Manager).persistInstanceErr

// archiveTeardown is the physical teardown+hook+move ArchiveSession runs before
// its durable commit. Indirected so race tests can install an editor in the
// exact post-move window without weakening the production ordering.
var archiveTeardown = (*session.Instance).ArchiveTeardownWithHook

// archivedWorktreePath returns the global archive location for a session's
// relocated worktree: <AGENT_FACTORY_HOME>/archived/<repoID>/<safeTitle>/. The
// repoID namespace prevents cross-repo title collisions; the title is sanitized
// for filesystem safety (the same scheme NewGitWorktree uses for session dirs).
func archivedWorktreePath(repoID, title string) (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("failed to resolve archive directory: %w", err)
	}
	return filepath.Join(dir, "archived", repoID, sanitizeArchiveTitle(title)), nil
}

// sanitizeArchiveTitle makes a session title safe as a single path segment,
// mirroring NewGitWorktree's safeSessionName handling (strip "..", "/"→"-",
// trim leading separators), falling back to "session" when nothing remains.
func sanitizeArchiveTitle(title string) string {
	s := strings.ReplaceAll(title, "..", "")
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.TrimLeft(s, "-.")
	if s == "" {
		s = "session"
	}
	return s
}
