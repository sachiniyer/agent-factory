package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
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

	// One config snapshot for the whole create (#2480): default_program,
	// session_env_passthrough, and every other per-op key read THIS generation, so
	// a config save that races the create can never pair one key's old value with
	// another key's new value. A per-use m.Config() inside the create is exactly the
	// torn read the op-entry rule prevents. (branch_prefix is a NAMED EXCEPTION — it
	// still reads the frozen m.cfg across the title-reservation helpers; making it
	// hot-reloadable is a fast-follow, so ApplyConfig reports it pending.)
	cfg := m.Config()

	if req.Program == "" {
		// Default from the repo-resolved config so an in-repo default_program
		// applies to daemon-created sessions (task runs, API creates) too.
		//
		// This is the ONE place the no-explicit-program default is decided, and
		// ListPrograms (#1970) answers by calling the same function rather than
		// restating the precedence — so the program a picker labels "repo default"
		// cannot disagree with the one a real create picks.
		req.Program = defaultProgramFor(cfg.DefaultProgram, req.RepoPath)
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

	// Session environment grants are security-sensitive. They come from the single
	// op-entry config snapshot (cfg) taken at the top of this create (#2480), so a
	// config save that races the create cannot mix generations. Its liveness is via
	// a save surface's ApplyConfig, not a per-create disk read: this used to do its
	// own config.LoadConfig() here, so a raw hand-edit of session_env_passthrough
	// was picked up on the next create; now it applies on save/ApplyConfig like
	// every other key — a deliberate, uniform change (see the #2480 release note).
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
		ResumeConversation:             req.resumeConversation,
		RestoreTabs:                    req.restoreTabs,
		PendingRecreateNotice:          req.pendingRecreateNotice,
		ProvisionSessionEnvPassthrough: append([]string(nil), cfg.SessionEnvPassthrough...),
	})
	if err != nil {
		return session.InstanceData{}, err
	}

	// Single creation flow (#930 PR 3): every instance owns its worktree 1:1.
	// InPlace only changes WHICH worktree that is — the repo's own working tree,
	// marked external — not the flow itself. finishCreateStart marks the instance
	// live, PARKS it at a usage-limit wall (#1146 PR4), or returns a fatal error.
	conversationCapture, startErr := task.StartAndSendPromptWithConversationCapture(ctx, instance, req.Prompt)
	if serr := finishCreateStart(instance, req.Prompt, startErr); serr != nil {
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
	// A heal (the root-agent ensure loop replacing a record it just reaped) marks
	// what became of the prior conversation, so a root that came back without its
	// history says so on its row instead of only in the application log (#2629).
	// Here, before the projection below, so the marker rides out on the create's
	// own persist and EventSessionCreated rather than in a second write behind
	// them. Never for an ordinary create: starting fresh is what those ARE.
	if req.replacesReapedRecord {
		instance.NoteRecreateContext()
	}
	data := instance.ToInstanceData()
	conversationToken := instance.AgentRuntimeToken()

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
		// Register the provider discovery in the same manager-lock critical
		// section that makes the instance visible. A concurrent status poll can
		// therefore never observe a newly created root without also seeing that
		// its initial conversation capture is still pending.
		m.startConversationCaptureLocked(repo.ID, key, instance, conversationCapture, conversationToken)
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
	creatingProjectionSettled = true
	// Publish from the Manager, not only the control-server wrapper: task delivery
	// and root-agent ensure call Manager.CreateSession directly. They announced the
	// same pending row above and therefore must settle it on the same events plane.
	m.publishEvent(agentproto.EventSessionCreated, data)

	return data, nil
}

// backendKindForCreate is the runtime resolution reserveCreate performs before
// taking the manager lock. A package var so a test can block inside it and prove
// m.mu is genuinely not held across it — the property is invisible otherwise,
// since the call succeeds either way and only its LOCK CONTEXT is the bug (#2931).
// Production never reassigns it.
var backendKindForCreate = session.BackendKindFor

// projectDeleteGenLocked reads the repo's completed-delete-attempt counter. The
// caller MUST already hold m.mu; a missing entry means no delete has ever been
// attempted for this repo and reads as 0, which is also what a first sample sees.
func (m *Manager) projectDeleteGenLocked(repoID string) uint64 {
	return m.projectDeleteGen[repoID]
}

// projectDeleteRefusal builds the refusal every delete-fence arm returns, so the
// three of them cannot drift in wording or in what they promise the caller.
//
// inProgress distinguishes a delete still running from one that finished while
// this create was outside m.mu; the guidance differs, and a create told to
// "retry after deletion finishes" about a delete that already finished would be
// telling the user to wait for nothing.
//
// TaskOrigin is daemon-only provenance independent of retained identity or
// concurrency ownership. Legacy targeted rows can have neither TaskID nor
// TaskRepoID, but admission still knows this create came from automation.
// Nothing has reserved a name, created a runtime, or sent a prompt on any of
// these paths, so the refusal is provably not attempted and carries the
// wire-visible marker. Keep the older identity shapes as compatibility evidence
// for in-process callers constructed before TaskOrigin was added; ordinary
// client creates carry none of these fields and retain their plain error.
func projectDeleteRefusal(req CreateSessionRequest, repoID string, inProgress bool) error {
	err := fmt.Errorf("project %s is being deleted; retry the session create after deletion finishes", repoID)
	if !inProgress {
		err = fmt.Errorf("project %s was being deleted while this session create resolved its backend; nothing was created — retry if the project still exists", repoID)
	}
	if req.TaskOrigin || req.TaskID != "" || req.TaskRepoID != "" {
		err = notAttempted(fmt.Errorf("%w; %s", err, notDeliveredMarker))
	}
	return err
}

// admitTaskRunFast is the pre-resolver half of the task-run cap, for a caller
// that does not hold m.mu. It returns the SAME errAtConcurrencyLimit sentinel as
// the authoritative check, so the watch-delivery path cannot tell the two apart
// and parks the event either way.
func (m *Manager) admitTaskRunFast(repoID, taskID string, limit int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.admitTaskRunLocked(repoID, taskID, limit)
}

// projectDeleteStateFor samples BOTH halves of the repo's delete state for a
// caller that does not hold m.mu; it takes the lock itself.
//
// The two must be read together, in one acquisition. The generation alone
// answers "did a delete BEGIN after this point" — it cannot answer "was a delete
// already running AT this point", because such a delete bumped the counter
// before the sample and removes its fence before the re-check, leaving both
// readings identical while the delete ran to completion across the entire gap.
func (m *Manager) projectDeleteStateFor(repoID string) (active bool, generation uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, active = m.projectDeletes[repoID]
	return active, m.projectDeleteGenLocked(repoID)
}

func (m *Manager) reserveCreate(req CreateSessionRequest) (*config.RepoContext, string, func(), *session.InstanceData, error) {
	if req.RepoPath == "" {
		return nil, "", nil, nil, fmt.Errorf("repo path is required")
	}
	repo, err := config.RepoFromPath(req.RepoPath)
	if err != nil {
		return nil, "", nil, nil, err
	}
	if req.TaskRepoID != "" && req.TaskRepoID != repo.ID {
		return nil, "", nil, nil, fmt.Errorf("task is bound to repo %s, but project path %q now resolves to repo %s; session was not created and prompt not delivered — rebind the task to use this project", req.TaskRepoID, req.RepoPath, repo.ID)
	}

	// Resolve the runtime BEFORE taking the manager lock (#2931).
	//
	// Both resolvers below reach session.resolveBackendKind -> resolveRepoConfig ->
	// config.RepoFromPath, which shells out to `git rev-parse` with no context, no
	// timeout, and no WaitDelay. Under m.mu that turned one unreachable repo — a
	// stalled NFS/FUSE mount, a spun-down disk — into a daemon-wide outage: the
	// exec never returns, the deferred unlock never runs, and every operation that
	// needs m.mu blocks behind it. Snapshot (so every TUI and web client), the
	// liveness poll, kill, archive, delete-project, task delivery: all of them, in
	// every project, not just the broken one.
	//
	// Neither resolver reads any manager state — they are pure functions of the
	// request and the repo root — so hoisting them costs nothing and bounds the
	// damage to the one create that asked. The sibling git call this function makes
	// under the lock (branch holds) is already bounded with its own deadline (#856);
	// this was the path that was not.
	// Sample the repo's delete state first, and refuse an ALREADY-RUNNING delete
	// right here — ahead of the resolvers, not after them.
	//
	// Deferring this refusal until after the resolution would make a create that
	// is already known to be impossible wait on the unbounded git below, which is
	// exactly the stall this hoist exists to bound. Before the hoist the fence
	// check ran ahead of backend resolution and such a create returned promptly;
	// keeping the refusal early preserves that. It is still behind everything a
	// refusal needs — repo identity and the task-binding check — and still ahead
	// of every mutation, so admission order is unchanged.
	//
	// The sampled state plus the post-lock check cover EVERY delete overlapping
	// the unlocked region. Writing the delete's fence interval as
	// [install, remove] and this region as [sample, check]:
	//
	//   - install before sample, remove after sample -> refused immediately below
	//   - install within the region                  -> the generation moved
	//   - remove after check                         -> the fence is still up
	//   - install after check                        -> the create has reserved by
	//     then, so DeleteProject's own fail-closed preflight refuses instead
	//
	// The first arm is why the fence is sampled and not just the counter: such a
	// delete bumped the generation BEFORE the sample and drops its fence BEFORE
	// the check, so both readings match while it ran across the whole gap (#2937
	// review). A delete that finished entirely before the sample is not an overlap
	// at all — that is an ordinary create after a completed delete.
	deleteActive, deleteGen := m.projectDeleteStateFor(repo.ID)
	if deleteActive {
		return nil, "", nil, nil, projectDeleteRefusal(req, repo.ID, true)
	}

	// Same reasoning for the task-run cap, which the hoist also moved behind the
	// resolvers. A capped watch delivery for a stalled repo would otherwise hang
	// in unbounded git instead of returning errAtConcurrencyLimit promptly and
	// parking the event on the durable queue — the cap's whole contract is that a
	// refusal is cheap and the event retries when a slot frees.
	//
	// ADVISORY, and one-way on purpose. It reads the counts without the
	// refreshLocked below, so it may only REFUSE early, never admit: the
	// authoritative check still runs post-refresh, under the same lock hold as the
	// reservation. refreshLocked replaces m.instances wholesale, so a stale count
	// can be high as well as low, and a spurious refusal here costs one park and
	// retry — the same tradeoff releaseTaskRunLocked already documents for its
	// momentary over-count, and the opposite of admitting one too many.
	if err := m.admitTaskRunFast(repo.ID, req.TaskID, req.MaxConcurrentRuns); err != nil {
		return nil, "", nil, nil, err
	}

	runtimeKind := session.BackendLocal
	if req.ForceRemote {
		runtimeKind = session.BackendHook
	}
	backendOpts := session.InstanceOptions{
		Backend:     session.BackendKind(req.Backend),
		ForceRemote: req.ForceRemote,
		InPlace:     req.InPlace,
	}
	if kind, kerr := backendKindForCreate(backendOpts, repo.Root); kerr == nil {
		runtimeKind = kind
	}
	// A kerr means an invalid backend value. Leave the conservative default above
	// and let NewInstance surface the canonical error rather than duplicating it.
	//
	// The in-place/off-box contradiction is resolved here too but REPORTED below,
	// inside the lock, at the point it was refused before. Hoisting the resolution
	// must not hoist the refusal: reserveCreate's admission order is load-bearing
	// (#2778/#2415), and this check has to stay ahead of the archived-name-reuse
	// rename and behind the project-delete fence, exactly where it was.
	inPlaceConflict := session.InPlaceBackendConflict(backendOpts, repo.Root)

	m.mu.Lock()
	defer m.mu.Unlock()
	// The remaining two arms: a delete still holding its fence, and one that both
	// started and finished while this create resolved. The already-running case
	// returned above, before the resolvers.
	_, deleting := m.projectDeletes[repo.ID]
	if deleting || m.projectDeleteGenLocked(repo.ID) != deleteGen {
		return nil, "", nil, nil, projectDeleteRefusal(req, repo.ID, deleting)
	}
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

	nameNamespace := runtimeNamespaceForKind(runtimeKind)

	// An in-place session and an off-box runtime are contradictory, and
	// NewInstance is where that is enforced — but it runs long after this
	// function, and this function MUTATES (#2778). For an explicit title held
	// only by an archived session, the reuse rename below relocates that
	// session's worktree and rewrites its durable record; a refusal raised later
	// at NewInstance would leave that rename standing for a create that could
	// never have succeeded, which is the one state reserveCreate's admission
	// comment promises it never produces (#2127, #2415).
	//
	// It also refuses the plain case earlier and better: `--here` against a
	// docker/ssh/hook repo now fails before a title is reserved, rather than
	// after provisioning work has begun.
	//
	// Through session.InPlaceBackendConflict rather than a local comparison
	// against runtimeKind, so the daemon's answer and NewInstance's cannot drift
	// — including on the deliberate non-firing for a backend value that will not
	// resolve, which belongs to the factory's canonical error.
	if inPlaceConflict != nil {
		return nil, "", nil, nil, inPlaceConflict
	}

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
		// And refuse for every other reason the rename cannot clear, still ahead
		// of it (#2415). Freeing the title does not free an orphan tmux session of
		// the same name, a hook slug another project owns, or the reserved "root"
		// name — so validateTitleAvailableLocked below could refuse AFTER the
		// rename had already relocated the archived worktree and rewritten its
		// durable record, leaving exactly the state this function promises never to
		// produce. Asking the record-independent half first turns those into
		// side-effect-free refusals.
		if err := m.refuseUnclaimableTitleReuseLocked(repo.ID, repo.Root, title, req.Program, nameNamespace, req.allowReserved, diskData); err != nil {
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
