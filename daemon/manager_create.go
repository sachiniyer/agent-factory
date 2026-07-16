package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

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
		// Default from the repo-resolved config so an in-repo
		// default_program applies to daemon-created sessions (task runs,
		// API creates) too. Falls back to the daemon's global config when
		// the repo path can't be resolved — reserveCreate will surface
		// that error with more context right after.
		req.Program = m.cfg.DefaultProgram
		if req.RepoPath != "" {
			if repo, err := config.RepoFromPath(req.RepoPath); err == nil {
				if resolved, rerr := config.ResolveConfig(repo.Root); rerr == nil {
					req.Program = resolved.DefaultProgram
				}
			}
		}
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

	repoStartLock := m.startLockForRepo(repo.ID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()

	instance, err := session.NewInstance(session.InstanceOptions{
		Title:       title,
		TaskID:      req.TaskID,
		Path:        repo.Root,
		Program:     req.Program,
		AutoYes:     req.AutoYes,
		InPlace:     req.InPlace,
		ForceRemote: req.ForceRemote,
		Backend:     session.BackendKind(req.Backend),
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
		// The create failed, so this instance is about to be discarded — it was never
		// registered or persisted, and the deferred release() hands its title straight
		// back out. That is only safe if the cleanup below actually removed what the
		// create built (#1917).
		//
		// Kill swallows everything tmux and git ANSWER for, so an error here means it
		// could NOT: a pane whose liveness is unknown, or a worktree removal cut off
		// mid-delete. Releasing the title over those leftovers means the next create
		// with this name collides with — or removes — a workspace nobody can address,
		// since no record points at it. So keep the record instead: it holds the title
		// and gives the user something to inspect and kill.
		if killErr := instance.Kill(); killErr != nil {
			if keepErr := m.keepFailedCreate(repo.ID, title, instance); keepErr != nil {
				return session.InstanceData{}, fmt.Errorf("failed to start instance %q, and its cleanup could not complete safely — its workspace may still be on disk at %s and could not be recorded, so it must be cleaned up by hand: %w",
					title, instance.GetWorktreePath(), errors.Join(serr, killErr, keepErr))
			}
			return session.InstanceData{}, fmt.Errorf("failed to start instance %q, and its cleanup could not complete safely, so its workspace was left in place; the session has been kept so you can inspect or kill it: %w",
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
	key := daemonInstanceKey(repo.ID, title)
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
		if killErr := instance.Kill(); killErr != nil {
			return session.InstanceData{}, fmt.Errorf("failed to record session %q, and its cleanup could not complete safely — its workspace may still be on disk at %s and must be cleaned up by hand: %w",
				title, instance.GetWorktreePath(), errors.Join(persistErr, killErr))
		}
		return session.InstanceData{}, persistErr
	}
	m.captureAgentConversationAsync(repo.ID, key, instance, conversationCapture)

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

	// Whether this create lands on the HOOK backend — the one runtime whose name
	// is a global namespace. ForceRemote is only ONE of three ways to get there:
	// `--backend hook` and a repo's `backend = "hook"` config both select it with
	// ForceRemote false, and gating the hook-name checks on ForceRemote alone let
	// those creates hand scripts a colliding --name. Ask the factory the same
	// question it will answer at provision time.
	usesHook := req.ForceRemote
	if kind, kerr := session.BackendKindFor(session.InstanceOptions{
		Backend:     session.BackendKind(req.Backend),
		ForceRemote: req.ForceRemote,
	}, repo.Root); kerr == nil {
		usesHook = kind == session.BackendHook
	}
	// A kerr here means an invalid `backend` value; leave usesHook at the legacy
	// ForceRemote answer and let NewInstance surface the canonical error rather
	// than duplicating its wording at a different point in the flow.

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
		title, err = m.nextAvailableTitleLocked(repo.ID, repo.Root, base, req.Program, usesHook, diskData)
		if err != nil {
			return nil, "", nil, nil, err
		}
	} else {
		// When the requested title is held ONLY by an archived session, rename that
		// archived session out of the way so the new session can take the name
		// (feat: reuse archived name). A LIVE collision is left untouched, so
		// validateTitleAvailableLocked below still rejects it exactly as before.
		renamedArchived, err = m.renameArchivedForReuseLocked(repo.ID, repo.Root, title, req.Program, &diskData)
		if err != nil {
			return nil, "", nil, nil, err
		}
		if err := m.validateTitleAvailableLocked(repo.ID, repo.Root, title, req.Program, usesHook, req.allowReserved, diskData); err != nil {
			return nil, "", nil, nil, err
		}
	}

	key := daemonInstanceKey(repo.ID, title)
	remoteName := ""
	if usesHook {
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
	if remoteName != "" {
		m.reservedRemoteNames[remoteName] = struct{}{}
	}
	m.reserveTaskRunLocked(repo.ID, req.TaskID, req.MaxConcurrentRuns)
	release := func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.reservedTitles, key)
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

// renameArchivedForReuseLocked frees `title` for a new session when the ONLY thing
// holding it is an archived session, by renaming that archived session to a
// disambiguated "<title> (archived[ N])" (feat: reuse archived name). It returns
// the renamed archived session's data (for a session.updated event) or nil when no
// rename happened — no archived collision, or a LIVE/reserved session also holds
// the name, in which case the create is left to fail in validateTitleAvailableLocked
// exactly as before. Runs under m.mu.
func (m *Manager) renameArchivedForReuseLocked(repoID, repoPath, title, program string, diskData *[]session.InstanceData) (*session.InstanceData, error) {
	archived, oldKey := m.findArchivedOnlyCollisionLocked(repoID, title)
	if archived == nil {
		return nil, nil
	}
	oldTitle := archived.Title
	// The replacement name must clear the same bar the archived row itself had:
	// if it is a HOOK session, restoring it later re-provisions with --name
	// Slugify(newTitle), so that slug has to be free in the GLOBAL hook namespace
	// too — otherwise the rename quietly parks it on a name another project's
	// sandbox already owns.
	archivedIsHook := archived.ToInstanceData().IsRemoteHook()
	newTitle, err := m.uniqueArchivedTitleLocked(repoID, repoPath, oldTitle, program, archivedIsHook, *diskData)
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
	if perr := renameInstanceDataTitle(repoID, oldTitle, renamed); perr != nil {
		if rbErr := archived.RenameArchived(oldTitle, origDest); rbErr != nil {
			// Could not move the worktree home: leave it re-keyed under the new title
			// (the bytes live at newDest) and surface both failures so the operator can
			// recover it. The new session create aborts.
			m.persistInstance(repoID, archived)
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

// findArchivedOnlyCollisionLocked returns the archived instance whose title
// collides with `title`, together with its manager-map key — but ONLY when nothing
// else claims the name: no LIVE (non-archived) instance and no in-flight
// reservation collide with it. A live or reserved collision returns nil, so the
// archived-name-reuse rename never runs around a name a real session still holds.
// Runs under m.mu.
func (m *Manager) findArchivedOnlyCollisionLocked(repoID, title string) (*session.Instance, string) {
	for key := range m.reservedTitles {
		rid, existing := splitDaemonInstanceKey(key)
		if rid == repoID && m.titlesCollide(existing, title) {
			// A concurrent create is reserving a colliding name; let the
			// availability check reject with errConcurrentCreate.
			return nil, ""
		}
	}
	var archived *session.Instance
	var archivedKey string
	for key, inst := range m.instances {
		rid, _ := splitDaemonInstanceKey(key)
		if rid != repoID || inst == nil {
			continue
		}
		if !m.titlesCollide(inst.Title, title) {
			continue
		}
		if inst.GetLiveness() != session.LiveArchived {
			// A live session still holds the name — do not rename around it.
			return nil, ""
		}
		archived = inst
		archivedKey = key
	}
	return archived, archivedKey
}

// uniqueArchivedTitleLocked returns the first free disambiguated title for an
// archived session being renamed out of the way: "<base> (archived)", then
// "<base> (archived 2)", "(archived 3)", … skipping any that collide with an
// existing live or archived session (feat: reuse archived name). Runs under m.mu.
func (m *Manager) uniqueArchivedTitleLocked(repoID, repoPath, base, program string, remote bool, diskData []session.InstanceData) (string, error) {
	for i := 1; i <= 10000; i++ {
		candidate := fmt.Sprintf("%s (archived)", base)
		if i > 1 {
			candidate = fmt.Sprintf("%s (archived %d)", base, i)
		}
		err := m.validateTitleAvailableLocked(repoID, repoPath, candidate, program, remote, false, diskData)
		if err == nil {
			return candidate, nil
		}
		if errors.Is(err, errTitleCheckFatal) {
			return "", err
		}
	}
	return "", fmt.Errorf("could not find an available archived name for %q", base)
}

func (m *Manager) nextAvailableTitleLocked(repoID, repoPath, baseTitle, program string, remote bool, diskData []session.InstanceData) (string, error) {
	for i := 1; i <= 10000; i++ {
		candidate := baseTitle
		if i > 1 {
			candidate = fmt.Sprintf("%s-%d", baseTitle, i)
		}
		err := m.validateTitleAvailableLocked(repoID, repoPath, candidate, program, remote, false, diskData)
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

func (m *Manager) validateTitleAvailableLocked(repoID, repoPath, title, program string, remote, allowReserved bool, diskData []session.InstanceData) error {
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
	if existing, kind := m.findTitleConflictLocked(repoID, title, diskData); existing != "" {
		switch {
		case existing == title:
			if kind == titleConflictReserved {
				return fmt.Errorf("session with title %q is already reserved: %w", title, errConcurrentCreate)
			}
			return fmt.Errorf("session with title %q already exists: %w", title, errConcurrentCreate)
		default:
			return fmt.Errorf("session titled %q conflicts with existing session %q: both sanitize to the same git branch %q", title, existing, m.branchForTitle(title))
		}
	}
	if remote {
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
	if tmuxSession := tmux.NewTmuxSessionForRepo(title, repoPath, program); tmuxSession.DoesSessionExist() {
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

type titleConflictKind int

const (
	titleConflictNone titleConflictKind = iota
	titleConflictReserved
	titleConflictLive
	titleConflictDisk
)

// findTitleConflictLocked returns the existing title that conflicts with the
// given candidate, along with the source of the conflict. An empty result means
// the title is available. Two titles conflict when they derive the same git
// branch name: branches are produced by git.SanitizeBranchName, which lowercases
// and normalizes (spaces -> dashes, unsafe chars stripped, dashes collapsed),
// so distinct titles like "MyApp"/"myapp" (#605) or "A B"/"a-b" (#741) can map
// to one branch. Rejecting the collision here keeps the second worktree create
// from failing with a cryptic git error.
func (m *Manager) findTitleConflictLocked(repoID, title string, diskData []session.InstanceData) (string, titleConflictKind) {
	for key := range m.reservedTitles {
		rid, existing := splitDaemonInstanceKey(key)
		if rid == repoID && m.titlesCollide(existing, title) {
			return existing, titleConflictReserved
		}
	}
	for key, inst := range m.instances {
		rid, _ := splitDaemonInstanceKey(key)
		if rid != repoID || inst == nil {
			continue
		}
		if m.titlesCollide(inst.Title, title) {
			return inst.Title, titleConflictLive
		}
	}
	for _, data := range diskData {
		if !m.titlesCollide(data.Title, title) {
			continue
		}
		// Loading entries are transient TUI state with an empty worktree
		// path and cannot be restored. Older TUI binaries (#551) could
		// persist them to disk on quit, where they would block title
		// reuse forever. Treat them as ghosts that the next save will
		// reap rather than as live reservations.
		if data.Status == session.Loading {
			continue
		}
		return data.Title, titleConflictDisk
	}
	return "", titleConflictNone
}

// titlesCollide reports whether two session titles cannot coexist in the same
// repo because they would derive the same git branch. It delegates to the shared
// git.TitlesCollide helper so the daemon's authoritative validation and the
// TUI's naming pre-check stay in lockstep (#936).
func (m *Manager) titlesCollide(a, b string) bool {
	return git.TitlesCollide(a, b, m.cfg.BranchPrefix)
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
// the next create under that name collides with or deletes them. Keeping the record
// makes the leftovers addressable (the user can see and kill the session) and holds
// the title against reuse. Mirrors the register-then-persist ordering of the success
// path: the map entry goes in first so the refresh loop cannot build a duplicate
// Instance from disk, and is rolled back if the write fails. The caller holds the
// repo start lock.
func (m *Manager) keepFailedCreate(repoID, title string, instance *session.Instance) error {
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
