package daemon

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// finishCreateStart settles a freshly created instance after StartAndSendPrompt
// returns (#1146 PR4). It is the single place CreateSession translates that
// outcome into liveness:
//   - startErr nil: the agent came up — mark it live.
//   - startErr is a usage-limit park (task.LimitReachedError): the agent hit a
//     usage-limit wall during startup before the prompt could be delivered. KEEP
//     the session — mark it limit-blocked with its parsed reset time and stash
//     the prompt so the resume machinery (the daemon auto-resume scheduler when
//     limit_auto_resume is on, or the manual `c` retry) re-delivers it once the
//     window resets. Return nil so CreateSession registers+persists it as a
//     parked row, not a failed one, firing no failure side-effects. instance.Prompt
//     is the only input resumeFromLimit re-delivers and is never otherwise set on
//     a daemon-created instance (the initial prompt goes straight to
//     StartAndSendPrompt), so it is set here so a parked task run resumes its OWN
//     work rather than a bare "continue".
//   - any other error: return it for CreateSession to surface after tearing the
//     half-started session down.
func finishCreateStart(instance *session.Instance, prompt string, startErr error) error {
	if startErr == nil {
		// Create succeeded: mark live through the chokepoint (#1195 Phase 2d).
		_ = instance.Transition(session.ConfirmLive())
		return nil
	}
	var limitErr *task.LimitReachedError
	if errors.As(startErr, &limitErr) {
		instance.SetLimitReached(limitErr.ResetAt)
		instance.Prompt = prompt
		return nil
	}
	return startErr
}

// createdTaskStatus maps a freshly created session's data to the task-run status
// DeliverPrompt records (#1146 PR4): the parked status when the create hit a
// usage-limit wall at startup (a NON-failure outcome the resume machinery owns),
// else the historical "started".
func createdTaskStatus(data session.InstanceData) string {
	if data.Liveness == session.LiveLimitReached {
		return TaskStatusLimitParked
	}
	return "started"
}

// resolveIdleLiveness settles a session whose pane DID NOT CHANGE this tick
// (#1146): it sets LimitReached when the captured content shows a usage-limit
// banner for the resolved agent (only claude/codex ever match), else Running when
// the agent is visibly still mid-turn, else the plain Ready liveness. A limit
// session must never render Ready, which is why the detector runs before the
// Ready fallback. Self-recovery (the banner scrolls away) or resumed work (the
// `updated` branch → Running) clears the limit liveness on its own later tick, so
// no explicit clear is needed here. Split from refreshInstanceStatus so control.go
// stays under its length ceiling (#1145).
//
// "Did not change" is NOT the same as "is idle", which is the whole reason the
// working check sits here. The caller reaches this branch on pane STILLNESS, and
// stillness is only evidence of idleness for agents that repaint while they work
// (claude/codex animate a spinner + elapsed timer; amp does not — it holds a
// static frame through every quiet gap in a turn). So before settling Ready, ask
// the agent: a pane that says it is working IS working, and stays Running. See
// task.IsWorkingContent for why a debounce cannot stand in for this.
func (m *Manager) resolveIdleLiveness(instance *session.Instance, content string) {
	agent := instance.ResolvedAgent()
	if hit, resetAt, _ := m.limitDetector.Check(content, agent, time.Now()); hit {
		instance.SetLimitReached(resetAt)
		return
	}
	if task.IsWorkingContent(content, agent) {
		// Still mid-turn behind a still pane: hold Running so the #1766 status dot
		// stays dark. Settling Ready here is the green flash (#1766 says green ==
		// waiting for you), and it is not merely cosmetic — a Ready amp is what
		// `af sessions watch` unblocks on and what tells a user their turn is done.
		_ = instance.Transition(session.ObserveLiveness(session.LiveRunning))
		return
	}
	// Plain idle: settle to Ready. On the two-axis model (#1195) SetLiveness
	// writes only the liveness axis and never clobbers an in-flight op, so it
	// needs no "if not deleting" guard — this is exactly what the poll's Ready
	// fallback does inline in refreshInstanceStatus.
	_ = instance.Transition(session.ObserveLiveness(session.LiveReady))
}

// testHookPollBeforePublish runs immediately before persistPollChange announces
// its session.updated, inside the repo start lock it persisted under. Tests
// substitute it to prove the publish really is in that critical section — the
// property that keeps an older whole-session payload from landing after a newer
// tab roster. No-op in production.
var testHookPollBeforePublish = func() {}

// persistPollChange writes an instance's state to disk when the poll changed
// something durable this tick (the #960 targeted writer): its LIVENESS
// transitioned, OR — evaluated INDEPENDENTLY — its usage-limit reset time changed
// (#1146). The liveness is the compared axis (#1195), so a Ready→LimitReached idle
// transition (invisible to the old composed-Status compare, since LimitReached
// composes to Ready) is caught as a genuine change.
//
// The reset-time check MUST be independent of the liveness compare: a row can
// enter LiveLimitReached on one tick with no parsed reset time (the banner
// matched but the time was not yet captured/parseable) and only parse it on a
// LATER tick. That later tick leaves the liveness unchanged, so gating
// persistence on the liveness alone would silently drop the reset time — the
// [limit] resets <t> badge would never show it, and PR3's auto-resume scheduler
// would have no time to schedule against once the daemon restarts and reloads
// from disk. beforeReset is the reset time captured before this tick's poll.
//
// A concurrent client op (create/kill/archive) means that op's executor owns the
// durable state, so the poll never persists over it. Split from
// refreshInstanceStatus so control.go stays under its length ceiling (#1145).
func (m *Manager) persistPollChange(repoID string, instance *session.Instance, before session.Liveness, beforeReset time.Time) {
	if instance.GetInFlightOp() != session.OpNone {
		return
	}
	afterReset, _ := instance.LimitResetAt()
	livenessChanged := instance.GetLiveness() != before
	resetChanged := !afterReset.Equal(beforeReset)
	if !livenessChanged && !resetChanged {
		return
	}
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	data := instance.ToInstanceData()
	err := persistInstanceData(repoID, data)
	// Push the change onto the events plane (#1592 PR5): this is the single choke
	// point every liveness/limit transition already flows through, so one publish
	// here covers session.updated without threading it through each caller.
	//
	// Published while STILL HOLDING the repo start lock, in the same critical section
	// as the persist that produced `data` — matching CreateTab/CloseTab. session.updated
	// carries a WHOLE InstanceData and every client re-projects the session wholesale
	// from it, so publish order is not cosmetic: it decides which snapshot wins. Publishing
	// after the unlock let this poll capture a roster, release the lock, and be preempted
	// by a tab create/delete that persisted AND announced the grown roster first — then
	// this older payload landed last and clients re-projected the tab right back out of
	// existence, until some later update happened to repair it (post-merge Codex finding
	// on #1815). Serializing publish with persist makes the last event the newest state.
	// publishEvent is non-blocking (drop-slow), so a wedged subscriber can't stall the poll.
	testHookPollBeforePublish()
	m.publishEvent(agentproto.EventSessionUpdated, data)
	repoStartLock.Unlock()
	if err != nil {
		log.WarningLog.Printf("daemon failed to persist status for %q: %v", instance.Title, err)
	}
}

// This file is the daemon side of the usage-limit manual-retry action (#1146
// PR2): the ResumeFromLimit RPC and the reusable resumeFromLimit Manager method
// behind it. Detection itself lives in refreshInstanceStatus (control.go), which
// runs the PR1 detector over captured pane content and sets the LiveLimitReached
// liveness. Split out of control.go to keep that (grandfathered, #1145) file
// from growing.

// ResumeFromLimitRequest asks the daemon to resume a session blocked at a usage-
// limit wall (#1146): re-spawn its agent if the tmux session exited, re-deliver
// the pending prompt, and clear the LimitReached liveness. It is the manual-
// retry action behind the TUI's `c` key; PR3's auto-resume scheduler reuses the
// same Manager method.
type ResumeFromLimitRequest struct {
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
}

type ResumeFromLimitResponse struct {
	OK bool `json:"ok"`
}

// The net/rpc ResumeFromLimit client wrapper moved onto the HTTP apiclient in
// #1592 Phase 2 PR3 (apiclient.Client.ResumeFromLimit, an internal non-cataloged
// route) — the TUI's `c` key was its only caller. The controlServer handler
// below still serves the verb over both transports.

func (s *controlServer) ResumeFromLimit(req ResumeFromLimitRequest, resp *ResumeFromLimitResponse) error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	if err := s.manager.resumeFromLimit(req); err != nil {
		return err
	}
	resp.OK = true
	return nil
}

// resumeFromLimit clears a session's usage-limit block and nudges its agent back
// to work (#1146). It is the reusable resume action shared by the TUI manual-
// retry key (`c`) and PR3's auto-resume scheduler — factored as a Manager method
// rather than inlined so the scheduler can call it directly. If the agent's tmux
// session exited while blocked it is re-spawned (Recover → resumeProgram) before
// the prompt is sent; a live stall needs no respawn. The pending prompt is then
// re-delivered — the session's stored initial/task prompt when it carries one (a
// task-driven session resumes its work), else a bare "continue" that un-stalls an
// interactive session (which loses context per anthropics/claude-code#5977;
// documented). The LimitReached liveness is cleared so the poll re-resolves the
// real state on the next tick, and the transition is persisted.
//
// Delivering that prompt is the ONLY thing that lifts the block: a resume that
// fails anywhere before the send lands leaves the session parked at the wall,
// both in memory and on disk, so the manual retry and the auto-resume scheduler
// (which both gate on the limit still being set) can pick it up again. The
// respawn arm re-applies the block for that reason — Respawn ends in ConfirmLive,
// which would otherwise report a session as resumed before its prompt existed.
//
// Runs under the per-(repo, title) target lock (like DeliverPrompt) and re-
// verifies the limit state under it, so it never races a self-recovery, a kill,
// or a concurrent resume. Rejects a tombstoned / reserved-root session, mirroring
// the lostrestore guards.
func (m *Manager) resumeFromLimit(req ResumeFromLimitRequest) error {
	instance, repoID, _, err := m.findSession(req.Title, req.RepoID)
	if err != nil {
		return err
	}
	if instance == nil {
		return fmt.Errorf("session %q not found", req.Title)
	}
	if !instance.LimitReached() {
		return fmt.Errorf("session %q is not blocked on a usage limit", req.Title)
	}

	key := daemonInstanceKey(repoID, instance.Title)
	m.mu.Lock()
	if _, killing := m.killsInFlight[key]; killing {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	opLock := m.opLockFor(key)
	if !opLock.TryLock() {
		return nil
	}
	defer opLock.Unlock()

	m.mu.Lock()
	current := m.instances[key]
	_, killing := m.killsInFlight[key]
	m.mu.Unlock()
	if killing || current != instance || instance.IsTearingDown() {
		return nil
	}

	return m.resumeFromLimitLocked(repoID, key, instance, req.Title)
}

// resumeFromLimitLocked performs the shared limit-resume action. The caller
// must hold the per-session op lock for key, so a manual retry cannot interleave
// with kill teardown and auto-resume can reuse the body after its own op-lock
// guard.
func (m *Manager) resumeFromLimitLocked(repoID, key string, instance *session.Instance, requestedTitle string) error {
	unlock := m.lockTarget(repoID, instance.Title)
	defer unlock()

	// Re-verify under the lock: a self-recovery or the poll may have cleared the
	// limit between the check above and the lock.
	if !instance.LimitReached() {
		return nil
	}
	m.mu.Lock()
	current := m.instances[key]
	_, killing := m.killsInFlight[key]
	m.mu.Unlock()
	if killing || current != instance || instance.IsTearingDown() {
		return nil
	}
	if instance.UserKilled() || session.IsReservedTitle(instance.Title) {
		return fmt.Errorf("session %q cannot be resumed", requestedTitle)
	}

	// Re-spawn only when the agent's session actually exited while blocked (the
	// edge case where the agent dropped to a shell / the pane vanished).
	//
	// Respawn, NOT Recover: the session is LiveLimitReached, and Recover's !Lost
	// guard rejects any non-Lost liveness, so routing a limit retry through Recover
	// always failed the guard (#1204 P1). Respawn is the guard-free re-spawn core
	// (same resumeProgram path: claude --continue, codex resume --last); the
	// LimitReached/no-tombstone precondition is enforced above under the target
	// lock.
	//
	// The probe decides whether to re-spawn, so an UNANSWERED probe must never
	// reach that arm (#1794). A remote Respawn is recoverSandbox — provision a
	// fresh sandbox, clone the branch from origin — so acting on "we could not
	// tell" re-provisions on a transport blip and orphans a live sandbox with its
	// unpushed work. This is the poll goroutine and it runs later in the SAME tick
	// as RefreshStatuses, so it had to be closed here too and not just there: the
	// poll's debounce protects the Lost path, not this one.
	//
	// Note this needs no debounce of its own — it needs the DISTINCTION. A
	// blip yields probeUnknown and is refused outright; a durable outage is what
	// the poll's debounce settles to Lost, which drops this session out of
	// LimitReached and hands it to the Lost-restore loop (the one place remote
	// re-provision belongs, with its own recheck). What remains is probeDead: the
	// sandbox answered that its agent exited while blocked — the #1786 case — and
	// that is authoritative, so it re-spawns at once, exactly as before.
	as := instance.AgentServer()
	switch probe := probeLiveness(instance, as); probe {
	case probeAlive:
		// A live stall — the common claude/codex case. No re-spawn; the un-stall
		// prompt below is all it needs.
	case probeUnknown:
		return fmt.Errorf("cannot resume %q: its agent-server did not answer the liveness probe; not re-spawning, because re-provisioning a sandbox that may still be running would orphan it and discard its unpushed work", requestedTitle)
	case probeDead:
		// Capture the limit window BEFORE the re-spawn: Respawn ends in ConfirmLive,
		// which drops both the LiveLimitReached liveness and its reset time, and
		// LimitResetAt reports (zero, false) once that has happened. Re-applying the
		// block below has to restore THIS episode's window, not a zeroed one — the
		// auto-resume scheduler schedules off it (reset + grace).
		resetAt, _ := instance.LimitResetAt()
		if rerr := instance.Respawn(); rerr != nil {
			return fmt.Errorf("failed to re-spawn agent for %q: %w", requestedTitle, rerr)
		}
		// The runtime this session's failure history was about is gone; the fresh
		// sandbox must not inherit it (#1794).
		m.noteRuntimeReplaced(repoID, instance)
		// Re-fetch: a REMOTE respawn re-provisions a FRESH sandbox and rebinds the
		// instance to its endpoint (bindProvisionResult swaps remoteClient and clears
		// the cached agent-server), so the `as` captured above is a client pinned to
		// the sandbox Respawn just tore down — SendPrompt below would target a dead
		// endpoint and the resume could never clear the limit (#1786). Inert for local
		// sessions, whose localAgentServer resolves i.backend per call.
		as = instance.AgentServer()
		// Re-apply the limit block Respawn's ConfirmLive just cleared. A re-spawned
		// agent is NOT a resumed one: this session stays parked at the wall until the
		// SendPrompt below actually delivers its pending prompt, so LiveLimitReached
		// is the truthful liveness for the whole window between here and there. The
		// resume's single completion point is ClearLimitReached, after the send lands.
		//
		// Without this the arm strands the session on BOTH axes when SendPrompt fails:
		// in memory the scheduler's `GetLiveness() != LiveLimitReached` guard skips a
		// LiveRunning row forever, and — since the checkpoint below serializes the
		// whole instance — that unblocked row reaches disk, so even a restart reloads
		// it non-limit-blocked and neither the manual `c` retry (its !LimitReached
		// guard) nor auto-resume ever retries the prompt that never landed. Re-parking
		// leaves the session exactly where the failed resume found it, which is what
		// both retry paths already know how to pick up.
		//
		// This is also what makes the ClearLimitReached below load-bearing rather than
		// a no-op on this arm: ConfirmLive used to have already cleared the limit.
		//
		// No hot-loop risk from re-parking: the auto-resume scheduler sets its
		// backoff gate BEFORE firing (limitResumeAttempted), so a resume that keeps
		// failing here backs off exponentially instead of hammering.
		instance.SetLimitReached(resetAt)
		// Write the respawn's durable state NOW, not at the end of the happy path
		// (#1854). Respawn shares LocalBackend.respawn, so reaching this line can mean
		// it rebuilt a vanished worktree — recreating the branch, flipping
		// branchCreatedByUs and rewriting baseCommitSHA. The SendPrompt below can
		// fail, and its early return would drop all of that: a restart would reload a
		// record with no rebuilt branch recorded, and kill would orphan the branch af
		// itself created (#1841's outcome, same class). The poll does not cover it
		// either — persistPollChange writes only on a liveness/reset-time change, and
		// the re-parked row matches the one the poll already knows.
		//
		// AFTER noteRuntimeReplaced, never before: the #1794/#1804 rule keeps every
		// statement — a disk write above all — out of the window between the runtime
		// swap and the debounce reset, so a blip there is not judged against the dead
		// runtime. restore.go resolves the same ordering the same way.
		m.persistInstance(repoID, instance)
	}

	prompt := strings.TrimSpace(instance.Prompt)
	if prompt == "" {
		// Interactive session with no stored prompt: the best we can do is
		// un-stall it. Loses the agent's prior context (documented caveat).
		prompt = "continue"
	}
	if serr := as.SendPrompt(prompt); serr != nil {
		return fmt.Errorf("failed to resume %q: %w", requestedTitle, serr)
	}
	// The prompt landed: this is the resume's single completion point, and the only
	// place the limit block is lifted on either arm.
	instance.ClearLimitReached()

	// The cleared limit is itself durable state worth a checkpoint. On the respawn
	// arm this is the second write; that is deliberate — the first one records the
	// rebuilt worktree of a session still parked at the wall, this one records the
	// resume that actually landed.
	m.persistInstance(repoID, instance)
	return nil
}
