package daemon

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// HandoffSessionRequest asks the daemon to continue a session under a different
// agent, in place (#2013).
type HandoffSessionRequest struct {
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	// ID is the session's stable id; see KillSessionRequest.ID. When non-empty
	// the daemon resolves by id first, so a web/CLI handoff cannot land on the
	// wrong session under a cross-repo title collision.
	ID string `json:"id"`
	// To is the incoming agent (a supported agent enum name).
	To string `json:"to"`
	// Brief optionally replaces the session's stored prompt as the mission handed
	// to the incoming agent. It is what a user typed at handoff time, so it wins:
	// more specific and more current than a prompt stored at create time.
	Brief string `json:"brief"`
}

type HandoffSessionResponse struct {
	OK bool `json:"ok"`
	// From and To are the outgoing and incoming agents, echoed so a client can
	// report the swap without re-reading the snapshot.
	From string `json:"from"`
	To   string `json:"to"`
	// HeadSHA is the branch tip at swap time — the attribution boundary.
	HeadSHA string `json:"head_sha,omitempty"`
}

func (s *controlServer) HandoffSession(req HandoffSessionRequest, resp *HandoffSessionResponse) error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	result, err := s.manager.HandoffSession(req)
	if err != nil {
		return err
	}
	*resp = result
	return nil
}

// HandoffSession swaps a session's agent in place, keeping its workspace,
// branch, tabs, task binding, and identity (#2013, design decision D3).
//
// The sequence and why it is ordered this way:
//
//  1. Resolve and validate BEFORE touching anything. An unsupported backend, an
//     unknown target, or a target that is already the running agent must fail
//     without having killed a working agent.
//  2. Capture the branch tip. This is the attribution boundary, and it has to be
//     read while the outgoing agent's work is the only work on the branch.
//  3. Build the mission from the OUTGOING agent's perspective — it names who
//     stopped and why — so it must be built before Program is rewritten.
//  4. Rewrite Program + append the ledger entry (SwapAgentProgram).
//  5. Swap the runtime (SwapAgent): stop the old agent, confirm it stopped,
//     launch the new one fresh.
//  6. Deliver the mission.
//
// Rollback: if the runtime swap fails, step 4's state change is reverted, because
// a record claiming the session runs claude while the pane still runs codex is
// worse than no handoff at all — every subsequent decision keyed off
// Instance.Program would be wrong. If the swap SUCCEEDS but the mission delivery
// fails, the swap stands: the new agent is genuinely the one running now, and
// lying about that to make the error tidier would strand the record. The error
// says the agent changed but the brief did not land, which is the truth and is
// recoverable with a send-prompt.
//
// Locking mirrors resumeFromLimit exactly: per-(repo,title) target lock FIRST,
// then the per-session op lock (#2006's canonical target-before-op order), with
// a re-verification under both. The target lock is what serializes this swap's
// prompt delivery against a concurrent DeliverPrompt to the same pane.
func (m *Manager) HandoffSession(req HandoffSessionRequest) (HandoffSessionResponse, error) {
	instance, repoID, title, _, _, err := m.resolveActionSession(req.ID, req.Title, req.RepoID)
	if err != nil {
		return HandoffSessionResponse{}, err
	}
	if instance == nil {
		return HandoffSessionResponse{}, fmt.Errorf("session %q not found", req.Title)
	}
	// Key everything off the RESOLVED title, never the request's: an id-resolved
	// handoff may carry a stale or empty title, and the locks below must name the
	// session actually being swapped.
	req.Title = title

	target := strings.TrimSpace(req.To)
	if err := instance.ValidateHandoffTarget(target); err != nil {
		return HandoffSessionResponse{}, err
	}
	if !instance.Capabilities().Handoff {
		return HandoffSessionResponse{}, session.ErrHandoffUnsupported
	}
	if instance.UserKilled() || session.IsReservedTitle(instance.Title) {
		return HandoffSessionResponse{}, fmt.Errorf("session %q cannot be handed off", req.Title)
	}

	key := daemonInstanceKey(repoID, instance.Title)
	m.mu.Lock()
	if _, killing := m.killsInFlight[key]; killing {
		m.mu.Unlock()
		return HandoffSessionResponse{}, fmt.Errorf("session %q is being killed", req.Title)
	}
	m.mu.Unlock()

	unlock := m.lockTarget(repoID, instance.Title)
	defer unlock()

	opLock := m.opLockFor(key)
	if !opLock.TryLock() {
		return HandoffSessionResponse{}, fmt.Errorf("session %q is busy with another operation", req.Title)
	}
	defer opLock.Unlock()

	// Re-verify under the locks: a kill or archive may have started, or another
	// handoff may have already moved this session to the requested agent.
	m.mu.Lock()
	current := m.instances[key]
	_, killing := m.killsInFlight[key]
	m.mu.Unlock()
	if killing || current != instance || instance.IsTearingDown() {
		return HandoffSessionResponse{}, fmt.Errorf("session %q is no longer available to hand off", req.Title)
	}
	if err := instance.ValidateHandoffTarget(target); err != nil {
		return HandoffSessionResponse{}, err
	}

	outgoing := instance.ResolvedAgent()

	// The mission describes the outgoing agent's work, so build it before the
	// swap rewrites who the outgoing agent is.
	reason := session.HandoffReasonManual
	if instance.LimitReached() {
		reason = session.HandoffReasonUsageLimit
	}
	brief := instance.BuildMissionBrief(target, req.Brief, reason)
	headSHA := brief.Work.HeadSHA

	entry, err := instance.SwapAgentProgram(target, reason, headSHA, false)
	if err != nil {
		return HandoffSessionResponse{}, err
	}

	if swapErr := instance.SwapAgent(); swapErr != nil {
		// Put the record back: the pane still runs the outgoing agent, so a
		// Program that says otherwise would mis-resolve every later respawn.
		if rbErr := instance.RevertHandoff(entry); rbErr != nil {
			log.ErrorLog.Printf("handoff %q: swap failed (%v) AND the record could not be reverted (%v); "+
				"the session's recorded agent may not match its running one", req.Title, swapErr, rbErr)
		}
		return HandoffSessionResponse{}, fmt.Errorf("failed to hand %q off to %s: %w", req.Title, target, swapErr)
	}

	// The runtime this session's failure history was about is gone (#1794).
	m.noteRuntimeReplaced(repoID, instance)

	// Any usage-limit block is already cleared at this point: SwapAgent ends in
	// ConfirmLive, which drops LiveLimitReached and its reset time. That is the
	// CORRECT outcome here, and it is the opposite of what the #1146 respawn arm
	// does — that path deliberately re-applies the block, because there the same
	// agent is coming back and stays parked until its pending prompt lands.
	//
	// Here the pane is running a DIFFERENT agent, which is not at anyone's limit.
	// The block described the outgoing agent's plan; re-applying it would badge a
	// healthy session [limit] and hand it to the auto-resume scheduler, which
	// would then "resume" an agent that was never blocked. This holds even if the
	// delivery below fails — the session is then a running agent with no
	// instructions, which is a different problem than a usage limit and should
	// not be reported as one.
	//
	// Persist before delivering: the swap is the durable fact, and a delivery
	// failure must not lose it (the #1854 lesson from the resume path).
	m.persistInstance(repoID, instance)

	mission := brief.Render()
	if serr := instance.AgentServer().SendPrompt(mission); serr != nil {
		return HandoffSessionResponse{}, fmt.Errorf(
			"handed %q off to %s, but its mission brief could not be delivered (%w); "+
				"the new agent is running with no instructions — re-send them with `af sessions send-prompt`",
			req.Title, target, serr)
	}
	log.InfoLog.Printf("handoff: session %q swapped %s → %s at %s", instance.Title, outgoing, target, shortSHA(headSHA))

	return HandoffSessionResponse{OK: true, From: outgoing, To: target, HeadSHA: headSHA}, nil
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "(no commits)"
	}
	return sha
}

// IsHandoffUnsupported reports whether err is the backend-restriction sentinel,
// so a client can render the restriction rather than match on prose.
func IsHandoffUnsupported(err error) bool {
	return errors.Is(err, session.ErrHandoffUnsupported)
}
