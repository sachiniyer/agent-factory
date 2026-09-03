package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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
	// StartAndSendPromptWithConversationCapture runs can never outlive the create
	// and keep capturing the pane — the amp hang, where a create that never
	// reached ready left a poll spinning under the per-repo start lock and pinned
	// the daemon. A caller context cancelled early (an abandoned create) tears it
	// down even sooner.
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
	workspace := repo.WorkspacePath()

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
		Path:          workspace,
		Status:        session.Loading,
		Liveness:      session.LiveReady,
		InFlightOp:    session.OpCreating,
		TaskRunActive: req.TaskID != "",
		CreatedAt:     createdAt,
		UpdatedAt:     createdAt,
		Prompt:        req.Prompt,
		Program:       req.Program,
		Worktree:      session.GitWorktreeData{RepoPath: repo.IdentityPath()},
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
	// branch_prefix is EffectNextDaemonStart. Pass the same frozen snapshot
	// used by the collision check; letting worktree creation reload the saved
	// value here makes the guard and mutation name different branches.
	frozenBranchPrefix := m.cfg.BranchPrefix

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
		Path:                           workspace,
		Program:                        req.Program,
		Account:                        req.Account,
		InPlace:                        req.InPlace,
		ForceRemote:                    req.ForceRemote,
		Backend:                        session.BackendKind(req.Backend),
		ResumeConversation:             req.resumeConversation,
		RestoreTabs:                    req.restoreTabs,
		PendingRecreateNotice:          req.pendingRecreateNotice,
		BranchPrefix:                   &frozenBranchPrefix,
		ProvisionSessionEnvPassthrough: append([]string(nil), cfg.SessionEnvPassthrough...),
		// Mints and revokes this session's credential, driven by the RUNTIME's
		// lifetime rather than by this call site (#3068): the session runtime mints
		// when it provisions a sandbox — original or replacement — and revokes when
		// it reaps one. Invoked only for a kind that provisions off-box (#2999), so
		// a local create neither mints nor inherits the require_token refusal.
		//
		// Post-provision revalidation lives with the mint, in session, so the create
		// and replacement paths cannot differ on it.
		SandboxCredentials: newSandboxCredentials(m, pending.ID),
	})
	// A create that does not finish must not leave a live credential behind
	// (#3012 review). The mint happens INSIDE NewInstance, so a provisioning
	// failure after it — or any later abandonment on this path — would otherwise
	// leave an orphaned sandbox authenticating indefinitely, with no session left
	// for an operator to kill. Deferred and armed rather than repeated at each
	// exit: the exits are many and one forgotten `return` is a credential that
	// never dies. Disarmed only once the session is committed to the roster,
	// where KillSession and archive take over revocation.
	createCommitted := false
	defer func() {
		if !createCommitted {
			m.sandboxTokens.revoke(pending.ID)
		}
	}()
	if err != nil {
		// A create that provisioned a sandbox and then could NOT confirm it torn
		// down carries a cleanup-only record out through its error (#3480). Keep it,
		// so the reap becomes the retry the daemon already runs rather than a
		// sentence in a CLI response that nothing can act on.
		//
		// Deliberately the SAME helper as the failed-Start branch below, because the
		// obligation is the same one: keepFailedCreate is what makes SaveInstances
		// retain the row through a wholesale checkpoint and what routes it to
		// finishUserKill on every poll, which retries the teardown and drops the
		// record once it completes. Nothing new is scheduled here.
		//
		// createCommitted stays FALSE, matching that branch and not the
		// uncertain-start one above: cleanup has been attempted and the daemon keeps
		// retrying it, so this row is on its way out and its credential goes with it.
		var orphan *session.SandboxOrphanError
		if errors.As(err, &orphan) {
			if keepErr := m.keepFailedCreate(repo.ID, title, orphan.OrphanRecord()); keepErr != nil {
				return session.InstanceData{}, fmt.Errorf("failed to create instance %q, and the sandbox provisioned for it could not be confirmed torn down or recorded — it may still be running and must be found and cleaned up by hand: %w",
					title, errors.Join(err, keepErr))
			}
			// SETTLE the provisional projection, exactly as the retained-Start branches
			// do. Without this, creatingProjectionSettled stays false and the deferred
			// cleanup publishes EventSessionKilled for the pending row — so live
			// clients delete the only visible handle to a tombstone that IS durable on
			// disk, which is the opposite of what retaining it was for.
			settleRetainedCreate(orphan.OrphanRecord())
			// COMMITTED on the wire (#3233): the row is durable and holds the title,
			// so a plain error with an empty InstanceData would read as
			// failed-nothing-committed and invite an immediate retry against a name
			// this record still owns.
			return orphan.OrphanRecord().ToInstanceData(), &mutationCommittedError{err: fmt.Errorf("failed to create instance %q, and the sandbox provisioned for it could not be confirmed torn down; it is recorded and the daemon will keep retrying that cleanup — the record clears once it succeeds: %w",
				title, err)}
		}
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
			// A COMMITTED outcome, not an abandonment (#3012 review). The whole point
			// of this branch is that the runtime may still be alive — that is why no
			// cleanup runs — so revoking its callback credential would sever a
			// possibly-running agent from the daemon while deliberately keeping its
			// session for inspection and later lifecycle operations. KillSession and
			// archive own its revocation now, exactly as for a clean start.
			//
			// Deliberately NOT applied to the keepFailedCreate branch below: there
			// cleanup has been attempted and the daemon keeps retrying it, so that
			// session is on its way out and its credential should go with it. And not
			// applied when keepUncertainCreate FAILS, because a credential with no
			// durable record is one no operator surface could ever revoke.
			createCommitted = true
			settleRetainedCreate(instance)
			// COMMITTED on the wire too (#3233): the row and workspace are durable,
			// so the retained projection rides back with the marker — a plain error
			// with an empty InstanceData reads as failed-nothing-committed and
			// invites a free retry against a title this row still holds.
			return instance.ToInstanceData(), &mutationCommittedError{err: fmt.Errorf("failed to start instance %q, and its startup outcome could not be determined safely, so its workspace was left in place; the session is recorded for inspection and no automatic cleanup will run: %w",
				title, serr)}
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
			m.warn().Printf("create of session %q: cleanup reported an error that does not leave its workspace state unknown; discarding the session as normal: %v", title, killErr)
		}
		if session.TeardownStateUnknown(killErr) {
			if keepErr := m.keepFailedCreate(repo.ID, title, instance); keepErr != nil {
				return session.InstanceData{}, fmt.Errorf("failed to start instance %q, and its cleanup could not complete safely — its workspace may still be on disk at %s and could not be recorded, so it must be cleaned up by hand: %w",
					title, instance.GetWorktreePath(), errors.Join(serr, killErr, keepErr))
			}
			settleRetainedCreate(instance)
			// Same committed contract as the unknown-start branch above (#3233):
			// the tombstoned row is durable and the daemon owns its cleanup retry.
			return instance.ToInstanceData(), &mutationCommittedError{err: fmt.Errorf("failed to start instance %q, and its cleanup could not complete safely, so its workspace was left in place; the session is recorded and the daemon will keep retrying the cleanup — it will clear once that succeeds: %w",
				title, errors.Join(serr, killErr))}
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
		instance.PinStorageRepoID(repo.ID)
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
	// The session is on the roster and persisted, so its credential is now owned
	// by the ordinary lifecycle — KillSession and archive revoke it from here.
	createCommitted = true
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

// stampProjectDeleteLocked records that this repo's delete fence just changed
// state. The caller MUST already hold m.mu — the stamp has to be atomic with the
// fence mutation it describes, or a create could observe one without the other.
func (m *Manager) stampProjectDeleteLocked(repoID string) {
	if m.projectDeleteLastSeq == nil {
		m.projectDeleteLastSeq = make(map[string]uint64)
	}
	m.projectDeleteSeq++
	m.projectDeleteLastSeq[repoID] = m.projectDeleteSeq
}

// projectDeleteSeqNow samples the fence-transition counter for a caller that
// does not hold m.mu.
//
// It takes NO repo id, which is the entire point: reserveCreate must sample
// before config.RepoFromPath, and that call is what produces the id (#2947).
func (m *Manager) projectDeleteSeqNow() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.projectDeleteSeq
}

// projectDeleteMovedLocked reports whether this repo's fence has changed state
// since the given sample. The caller MUST already hold m.mu.
//
// Strictly greater, deliberately. A delete whose last transition is EQUAL to the
// sample completed before the sample was taken, which is ordinary history rather
// than a race — treating it as one would strand every project that is ever
// deleted and re-created.
func (m *Manager) projectDeleteMovedLocked(repoID string, since uint64) bool {
	return m.projectDeleteLastSeq[repoID] > since
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

// projectDeleteStateFor answers, in ONE acquisition, whether this repo has a
// delete running now or has had one transition since the given sample. Both
// readings must come from the same acquisition: taken separately they could
// describe different instants and a delete could slip between them.
func (m *Manager) projectDeleteStateFor(repoID string, since uint64) (active, moved bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, active = m.projectDeletes[repoID]
	return active, m.projectDeleteMovedLocked(repoID, since)
}

// repoFromPathForCreate resolves the request's path to a repo identity. A
// package var so a test can block inside it and drive a delete through the
// window where reserveCreate does not yet know which repo it is talking about —
// a window nothing repo-keyed can be sampled in, which is what #2947 is about.
// Production never reassigns it.
var repoFromPathForCreate = config.RepoFromPath

// warnLegacyBareCloneSessions makes the #3358 identity transition explicit.
// Old rows cannot be migrated safely: the old writer persisted the unrelated
// parent as both Path and Worktree.RepoPath, discarding the linked worktree the
// user originally requested. That parent may itself own real sessions, and
// several bare repositories may share it. Preserve those rows under their old
// key, where all-repo listing and stable-ID actions still reach them, and name
// the compatibility path instead of silently pretending the new identity has
// no history.
func warnLegacyBareCloneSessions(repo *config.RepoContext) {
	legacyRoot, legacyID := repo.LegacyBareRepoIdentity()
	if legacyID == "" || legacyID == repo.ID {
		return
	}
	rows, err := loadRepoInstanceData(legacyID)
	if err != nil {
		log.WarningLog.Printf("bare repository %s now uses repo identity %s, but its pre-#3358 parent-keyed session store %s could not be read: %v; inspect that repo ID before assuming it is empty", repo.IdentityPath(), repo.ID, legacyID, err)
		return
	}
	if len(rows) == 0 {
		return
	}
	log.WarningLog.Printf("bare repository %s now uses repo identity %s; preserving %d pre-#3358 session record(s) under former parent identity %s (%s) because those records discarded the requesting worktree and cannot be re-attributed safely — they remain available through all-repo listing and stable session IDs", repo.IdentityPath(), repo.ID, len(rows), legacyID, legacyRoot)
}

// warnLegacyBareCloneTasks is the automation half of the same #3358 transition.
// A task created from a bare linked worktree before the fix retained the
// unrelated parent in BOTH ProjectPath and RepoID, so the corrected project no
// longer lists it — while an enabled cron/watch task keeps firing under the old
// identity. When that parent is itself a repository, each delivery keeps making
// sessions in it, invisible from the bare project the user now works in.
//
// Sessions are inert once preserved; automation is not, so this names the tasks
// and what to do about them. Rebinding them here would be the same unsafe
// re-attribution the session rows are preserved to avoid: the old rows discarded
// the requesting worktree, several bare clones can share one parent, and a real
// repository may live there and legitimately own these tasks.
//
// The scan is a pure read of the task file — no ProjectPath resolution and no
// binding backfill (LoadTasksForRepoID durably rewrites bindings and hands the
// caller a publish obligation, neither of which belongs on a create path).
// Matching is therefore textual: the retained RepoID, or a legacy row whose
// RepoID was never written and whose ProjectPath still spells the old parent.
func warnLegacyBareCloneTasks(repo *config.RepoContext) {
	legacyRoot, legacyID := repo.LegacyBareRepoIdentity()
	if legacyID == "" || legacyID == repo.ID {
		return
	}
	all, err := loadTasksForLegacyScan()
	if err != nil {
		log.WarningLog.Printf("bare repository %s now uses repo identity %s, but its pre-#3358 tasks could not be read: %v; inspect `af tasks list --all` for tasks still bound to former parent identity %s (%s) before assuming there are none", repo.IdentityPath(), repo.ID, err, legacyID, legacyRoot)
		return
	}
	var stranded []task.Task
	for _, t := range all {
		if t.RepoID != "" {
			if t.RepoID == legacyID {
				stranded = append(stranded, t)
			}
			continue
		}
		if t.ProjectPath != "" && filepath.Clean(t.ProjectPath) == filepath.Clean(legacyRoot) {
			stranded = append(stranded, t)
		}
	}
	if len(stranded) == 0 {
		return
	}
	enabled := 0
	for _, t := range stranded {
		if t.Enabled {
			enabled++
		}
	}
	log.WarningLog.Printf("bare repository %s now uses repo identity %s; %d pre-#3358 task(s) (%d enabled) remain bound to former parent identity %s (%s) and are NOT listed by this project: %s; enabled cron/watch deliveries keep creating sessions under that identity — inspect them with `af tasks list --all`, then disable or explicitly rebind each one to this project", repo.IdentityPath(), repo.ID, len(stranded), enabled, legacyID, legacyRoot, describeLegacyTasks(stranded))
}

// describeLegacyTasks names the stranded tasks so the warning is actionable
// without a second lookup: an id is what `af tasks` acts on, a name is what the
// user recognizes.
func describeLegacyTasks(tasks []task.Task) string {
	parts := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if t.Name != "" {
			parts = append(parts, fmt.Sprintf("%s (%q)", t.ID, t.Name))
			continue
		}
		parts = append(parts, t.ID)
	}
	return strings.Join(parts, ", ")
}

// loadTasksForLegacyScan is a package var so the warning above can be tested
// without a task file on disk. Production never reassigns it.
var loadTasksForLegacyScan = task.LoadTasks

func (m *Manager) reserveCreate(req CreateSessionRequest) (*config.RepoContext, string, func(), *session.InstanceData, error) {
	if req.RepoPath == "" {
		return nil, "", nil, nil, fmt.Errorf("repo path is required")
	}

	// Sample the fence-transition counter BEFORE resolving the repo, because
	// resolving the repo is itself part of the window (#2947).
	//
	// config.RepoFromPath shells out to `git rev-parse` with no context and no
	// deadline — the same unbounded call #2931 moved the manager lock off, and it
	// sits outside that lock here. A DeleteProject can install its fence, run to
	// completion, and remove it while this is stalled on an unreachable mount.
	//
	// Nothing repo-keyed can be sampled at this point: the key is what the call
	// below returns. That is why the counter is global and only the STAMP is per
	// repo — the sample needs no identity, while the later comparison still has
	// one, so another project's delete never refuses this create.
	deleteSeq := m.projectDeleteSeqNow()

	repo, err := repoFromPathForCreate(req.RepoPath)
	if err != nil {
		return nil, "", nil, nil, err
	}
	warnLegacyBareCloneSessions(repo)
	warnLegacyBareCloneTasks(repo)
	workspace := repo.WorkspacePath()
	identityRoot := repo.IdentityPath()
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
	// Now that the identity exists, ask the two questions the sample enables, and
	// refuse HERE — ahead of the resolvers below, not after them.
	//
	// Deferring the refusal past the resolution would make a create that is
	// already known to be impossible wait on more unbounded git, which is exactly
	// the stall this hoist exists to bound. Before the hoist the fence check ran
	// ahead of backend resolution and such a create returned promptly; refusing
	// here preserves that. It is still behind everything a refusal needs — repo
	// identity and the task-binding check — and ahead of every mutation, so
	// admission order is unchanged.
	//
	// Together with the post-lock check this covers EVERY delete overlapping the
	// create. Writing the delete's fence interval as [install, remove] and the
	// create's unlocked span as [sample, check]:
	//
	//   - install before sample, remove after sample -> the stamp moved (remove)
	//   - install within the span                    -> the stamp moved (install)
	//   - fence still up when asked                  -> active, either check
	//   - install after check                        -> the create has reserved by
	//     then, so DeleteProject's own fail-closed preflight refuses instead
	//
	// The first arm is why the REMOVE is stamped and not only the install: such a
	// delete began before the sample, so its install stamp cannot exceed it, and
	// its fence may be gone by the time either check runs. A delete that finished
	// entirely before the sample leaves a stamp at or below it and is admitted —
	// an ordinary create after a completed delete, not a race.
	deleteActive, deleteMoved := m.projectDeleteStateFor(repo.ID, deleteSeq)
	if deleteActive || deleteMoved {
		return nil, "", nil, nil, projectDeleteRefusal(req, repo.ID, deleteActive)
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
	if kind, kerr := backendKindForCreate(backendOpts, workspace); kerr == nil {
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
	inPlaceConflict := session.InPlaceBackendConflict(backendOpts, workspace)

	m.mu.Lock()
	defer m.mu.Unlock()
	// The re-check, against the SAME sample. It catches a delete that began, or
	// began and finished, during the backend resolution above — the span the
	// pre-resolver check could not see.
	_, deleting := m.projectDeletes[repo.ID]
	if deleting || m.projectDeleteMovedLocked(repo.ID, deleteSeq) {
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
		title, err = m.nextAvailableTitleLocked(repo.ID, identityRoot, base, req.Program, nameNamespace, diskData)
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
		if err := m.refuseHeldBranchReuseLocked(repo.ID, identityRoot, title, nameNamespace, req.InPlace, diskData); err != nil {
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
		if err := m.refuseUnclaimableTitleReuseLocked(repo.ID, identityRoot, title, req.Program, nameNamespace, req.allowReserved, diskData); err != nil {
			return nil, "", nil, nil, err
		}
		renamedArchived, err = m.renameArchivedForReuseLocked(repo.ID, identityRoot, title, req.Program, nameNamespace, &diskData)
		if err != nil {
			return nil, "", nil, nil, err
		}
		if err := m.validateTitleAvailableLocked(repo.ID, identityRoot, title, req.Program, nameNamespace, req.allowReserved, diskData); err != nil {
			return nil, "", nil, nil, err
		}
	}

	key := daemonInstanceKey(repo.ID, title)
	tmuxReservationKey := ""
	if nameNamespace == runtimeNamespaceLocalTmux {
		tmuxReservationKey = daemonInstanceKey(repo.ID, tmux.SanitizedNameForRepo(title, identityRoot))
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
// per-repo instances.json that is corrupted, or that could not be read at all
// (#3476), and might be hiding a hook session using the name.
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
		m.warn().Printf("could not list worktree branch holds for %s; resolving the session title without them (a name an archived worktree holds will fail at worktree add instead of being skipped): %v", repoPath, err)
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
	instance.PinStorageRepoID(repoID)
	return nil
}
