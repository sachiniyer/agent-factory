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

// resolveIdleLiveness settles a session whose pane went idle this tick (#1146):
// it sets LimitReached when the captured content shows a usage-limit banner for
// the resolved agent (only claude/codex ever match), else the plain Ready
// liveness. A limit session must never render Ready, which is why the detector
// runs before the Ready fallback. Self-recovery (the banner scrolls away) or
// resumed work (the `updated` branch → Running) clears the limit liveness on its
// own later tick, so no explicit clear is needed here. Split from
// refreshInstanceStatus so control.go stays under its length ceiling (#1145).
func (m *Manager) resolveIdleLiveness(instance *session.Instance, content string) {
	if hit, resetAt, _ := m.limitDetector.Check(content, instance.ResolvedAgent(), time.Now()); hit {
		instance.SetLimitReached(resetAt)
	} else {
		// Plain idle: settle to Ready. On the two-axis model (#1195) SetLiveness
		// writes only the liveness axis and never clobbers an in-flight op, so it
		// needs no "if not deleting" guard — this is exactly what the poll's Ready
		// fallback does inline in refreshInstanceStatus.
		_ = instance.Transition(session.ObserveLiveness(session.LiveReady))
	}
}

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
	repoStartLock.Unlock()
	if err != nil {
		log.WarningLog.Printf("daemon failed to persist status for %q: %v", instance.Title, err)
	}
	// Push the change onto the events plane (#1592 PR5): this is the single choke
	// point every liveness/limit transition already flows through, so one publish
	// here covers session.updated without threading it through each caller.
	m.publishEvent(agentproto.EventSessionUpdated, data)
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

	// Re-spawn only when the agent's tmux session actually exited while blocked
	// (the edge case where the agent dropped to a shell / the pane vanished). A
	// live stall — the common claude/codex case — just needs the un-stall prompt.
	//
	// Respawn, NOT Recover: the session is LiveLimitReached, and Recover's !Lost
	// guard rejects any non-Lost liveness, so routing a limit retry through Recover
	// always failed the guard (#1204 P1). Respawn is the guard-free re-spawn core
	// (same resumeProgram path: claude --continue, codex resume --last); the
	// LimitReached/no-tombstone precondition is enforced above under the target
	// lock.
	as := instance.AgentServer()
	if !as.Alive() {
		if rerr := instance.Respawn(); rerr != nil {
			return fmt.Errorf("failed to re-spawn agent for %q: %w", requestedTitle, rerr)
		}
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
	instance.ClearLimitReached()

	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	perr := persistInstanceData(repoID, instance.ToInstanceData())
	repoStartLock.Unlock()
	if perr != nil {
		log.WarningLog.Printf("daemon failed to persist resume for %q: %v", instance.Title, perr)
	}
	return nil
}
