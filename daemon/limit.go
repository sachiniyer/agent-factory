package daemon

import (
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

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
		instance.SetLiveness(session.LiveReady)
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
	err := persistInstanceData(repoID, instance.ToInstanceData())
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

// ResumeFromLimit asks the daemon to resume a usage-limit-blocked session
// (#1146): re-spawn if the agent exited, re-deliver the pending prompt, and
// clear the limit state. Surfaces the daemon's error (e.g. the session is not
// limit-blocked) verbatim so the TUI can show it.
func ResumeFromLimit(req ResumeFromLimitRequest) error {
	var resp ResumeFromLimitResponse
	if err := callDaemon("ResumeFromLimit", req, &resp); err != nil {
		return err
	}
	return nil
}

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

	unlock := m.lockTarget(repoID, instance.Title)
	defer unlock()

	// Re-verify under the lock: a self-recovery or the poll may have cleared the
	// limit between the check above and the lock.
	if !instance.LimitReached() {
		return nil
	}
	if instance.UserKilled() || session.IsReservedTitle(instance.Title) {
		return fmt.Errorf("session %q cannot be resumed", req.Title)
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
	if !instance.TmuxAlive() {
		if rerr := instance.Respawn(); rerr != nil {
			return fmt.Errorf("failed to re-spawn agent for %q: %w", req.Title, rerr)
		}
	}

	prompt := strings.TrimSpace(instance.Prompt)
	if prompt == "" {
		// Interactive session with no stored prompt: the best we can do is
		// un-stall it. Loses the agent's prior context (documented caveat).
		prompt = "continue"
	}
	if serr := instance.SendPromptCommand(prompt); serr != nil {
		return fmt.Errorf("failed to resume %q: %w", req.Title, serr)
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
