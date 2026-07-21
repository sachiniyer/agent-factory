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
	if err := s.requireStateMutationAdmission(); err != nil {
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
//  1. Resolve and preflight the exact launch command BEFORE touching anything.
//     An unsupported backend, unknown/same target, missing executable, or env
//     wrapper af cannot model must fail without having killed a working agent.
//  2. Capture the branch tip. This is the attribution boundary, and it has to be
//     read while the outgoing agent's work is the only work on the branch.
//  3. Build the mission from the OUTGOING agent's perspective — it names who
//     stopped and why — so it must be built before Program is rewritten.
//  4. Raise OpReplacing, then rewrite Program + append the ledger entry.
//  5. Swap the runtime using the prepared plan: stop the old agent, confirm it
//     stopped, and launch the new one fresh while KEEPING the fence raised.
//  6. Wait for readiness, dismiss trust UI, and deliver the mission. A usage
//     limit here atomically parks the incoming runtime with that mission pending.
//  7. Settle the fence, then persist and begin generation-scoped capture.
//
// Rollback: if the runtime swap fails, step 4's state change is reverted, because
// a record claiming the session runs claude while the pane still runs codex is
// worse than no handoff at all — every subsequent decision keyed off
// Instance.Program would be wrong. If the swap SUCCEEDS but readiness cannot be
// established, the record becomes startup-unknown rather than claiming a
// healthy incoming runtime. If readiness succeeds but the mission paste fails,
// the exact rendered brief and OpReplacing fence remain durable for the daemon's
// bounded retry path; neither failure is flattened into a false Running state.
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

	// Capability answers whether this backend IMPLEMENTS handoff. The shared
	// runtime-action guard answers the prior question: whether this row currently
	// has a live runtime that may be replaced. Keeping state first prevents an
	// Archived/Lost/Dead row from being reanimated through a capable backend
	// instead of its restore path (#2231).
	if err := instance.ValidateRuntimeAction(session.RuntimeActionHandoff); err != nil {
		return HandoffSessionResponse{}, err
	}
	if session.IsReservedTitle(instance.Title) {
		return HandoffSessionResponse{}, fmt.Errorf("session %q cannot be handed off", req.Title)
	}
	target := strings.TrimSpace(req.To)
	if err := instance.ValidateHandoffTarget(target); err != nil {
		return HandoffSessionResponse{}, err
	}
	if !instance.Capabilities().Handoff {
		return HandoffSessionResponse{}, session.ErrHandoffUnsupported
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
	if killing || current != instance {
		return HandoffSessionResponse{}, fmt.Errorf("session %q is no longer available to hand off", req.Title)
	}
	if err := instance.ValidateRuntimeAction(session.RuntimeActionHandoff); err != nil {
		return HandoffSessionResponse{}, err
	}
	if err := instance.ValidateHandoffTarget(target); err != nil {
		return HandoffSessionResponse{}, err
	}
	plan, err := instance.PrepareAgentSwap(target)
	if err != nil {
		return HandoffSessionResponse{}, fmt.Errorf("cannot hand %q off to %s without stopping its current agent: %w", req.Title, target, err)
	}

	outgoing := instance.CurrentAgentName()

	// The mission describes the outgoing agent's work, so build it before the
	// swap rewrites who the outgoing agent is.
	reason := session.HandoffReasonManual
	if instance.LimitReached() {
		reason = session.HandoffReasonUsageLimit
	}
	brief := instance.BuildMissionBrief(target, req.Brief, reason)
	mission := brief.Render()
	headSHA := brief.Work.HeadSHA
	conversationCapture := plan.ConversationCapture()

	// Fence the close/start window before either the record or runtime changes.
	// New polls skip OpReplacing; a poll already observing the outgoing pane sees
	// the bumped state epoch and cannot apply that stale result to the incoming one.
	if err := instance.Transition(session.BeginHandoff()); err != nil {
		return HandoffSessionResponse{}, err
	}
	entry, err := instance.RecordHandoffSwap(target, reason, headSHA, false)
	if err != nil {
		_ = instance.Transition(session.AbortHandoff())
		return HandoffSessionResponse{}, err
	}

	checkpoint, swapErr := instance.SwapAgent(plan)
	if swapErr != nil {
		// Put the record back: SwapAgent did not confirm an incoming runtime, so
		// recording a completed handoff would make later resume/readiness decisions
		// target an agent af never established. Expected target/config failures were
		// already rejected by PrepareAgentSwap before teardown; this is the narrow
		// runtime/infrastructure failure path.
		if rbErr := instance.RevertHandoff(entry); rbErr != nil {
			log.ErrorLog.Printf("handoff %q: swap failed (%v) AND the record could not be reverted (%v); "+
				"the session's recorded agent may not match its running one", req.Title, swapErr, rbErr)
		}
		_ = instance.Transition(session.AbortHandoff())
		return HandoffSessionResponse{}, fmt.Errorf("failed to hand %q off to %s: %w", req.Title, target, swapErr)
	}
	delivery := prepareHandoffDelivery(handoffDelivery{
		repoID: repoID, key: key, title: req.Title, target: target, mission: mission,
		instance: instance, conversationCapture: conversationCapture,
	})

	// The runtime this session's failure history was about is gone (#1794).
	m.noteRuntimeReplaced(repoID, instance)

	// A successful backend swap does not clear the replacement fence yet. That is
	// intentional: a fresh idle prompt before delivery is not a completed task
	// run, and the status poll must not observe it as one. Settling below clears
	// any OUTGOING usage-limit block as part of the incoming agent's outcome. That
	// is the opposite of what the #1146 respawn arm does — that path deliberately
	// re-applies the block, because there the same agent is coming back and stays
	// parked until its pending prompt lands.
	//
	// Here the pane is running a DIFFERENT agent. The old block described the
	// outgoing agent's plan; re-applying it would badge a healthy incoming session
	// [limit]. A NEW limit observed while waiting for that incoming agent is a
	// different fact and parks through ParkHandoff below.
	//
	// An operator-supplied brief becomes the durable goal for later limit resumes
	// and handoffs; replaying the create-time prompt would send the new agent back
	// to an obsolete mission.
	if override := strings.TrimSpace(req.Brief); override != "" {
		instance.SetPrompt(override)
		checkpoint.Prompt = override
	}
	// The incoming runtime now exists, but its takeover context does not. Record
	// that exact rendered mission in memory and in the post-swap checkpoint BEFORE
	// waiting for readiness: if the daemon exits anywhere in that potentially
	// minute-long window, the restored row has an explicit delivery obligation
	// instead of silently claiming an instruction-less agent is complete.
	instance.SetPendingHandoffMission(mission)
	checkpoint.PendingHandoffMission = mission
	// The process swap is already irreversible, so checkpoint its durable facts
	// before the readiness wait. Memory stays fenced, but a daemon crash in this
	// window must restore the incoming Program rather than lie that the outgoing
	// agent still owns the pane. The projection also clears any outgoing-provider
	// limit state, matching CommitHandoff's recovery posture.
	if err := m.persistInstanceSnapshotErr(repoID, checkpoint); err != nil {
		// Preserve the existing best-effort persist contract: delivery can still
		// make the live agent useful, and the final settled write retries below.
		log.WarningLog.Printf("handoff %q: failed to persist the post-swap checkpoint before mission delivery: %v", req.Title, err)
	}

	if err := m.deliverHandoffMission(delivery); err != nil {
		return HandoffSessionResponse{}, err
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
