package daemon

import (
	"context"
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// Account handoff and limit failover for a session on a listed host (#5571 W7.6).
//
// The local transaction (a pending-swap record, a stopped pane, a respawn) has no
// counterpart on a host: the agent-server owns the pane and rotates its own account
// scope between the confirmed stop and the start (session.SwapOffBoxAccount). The
// daemon's part is the fence around that call, the durable row, and the notice that
// tells the fresh conversation what it is continuing.
//
// The two entry points share one rule: the session is on its old account until the
// swap reports the new agent started. Every refusal before that costs nothing, and
// nothing is recorded for an account the host does not hold.

// offBoxAccountNoConversation is the stated reason the replacement is a fresh
// conversation: a transcript lives in the previous account's store on the host, and
// the new account's scope holds none of it.
const offBoxAccountNoConversation = "a sandbox session's conversation stays in the previous account's store on the host and is not carried to another account"

// offBoxAccountNotice renders what the replacement agent is told. It states the
// conversation outcome, like the local automatic notice does, and carries the
// session's task so a fresh conversation can continue it.
func offBoxAccountNotice(agent, from, to, reason, prompt string) string {
	notice := fmt.Sprintf("[Agent Factory] This session switched from %s to %s%s. Continue the same task under the new identity. "+
		"This is a fresh conversation: the earlier messages are not available to you — only the working tree and its git history are, "+
		"because %s.",
		accountSwapIdentity(agent, from), accountSwapIdentity(agent, to), reason, offBoxAccountNoConversation)
	if strings.TrimSpace(prompt) == "" {
		return notice + "\n\ncontinue"
	}
	return notice + "\n\n" + strings.TrimSpace(prompt)
}

// handoffAccountOffBox is `handoff --account B` on a sandbox session carrying an
// account. The caller holds the target and operation locks and has re-read the slot.
func (m *Manager) handoffAccountOffBox(req HandoffSessionRequest, instance *session.Instance, repoID, key string) (HandoffSessionResponse, error) {
	to := strings.TrimSpace(req.Account)
	from, _ := instance.AccountSelection()
	outgoing := instance.CurrentAgentName()
	// The scope variable is part of the environment fixed when the agent-server
	// launched, so the account changes and the agent does not.
	if target := strings.TrimSpace(req.To); target != "" &&
		!session.HandoffTargetIsCurrent(outgoing, target, session.HandoffEffectiveAgentForPath(instance.Path, target), instance.AgentProgram()) {
		return HandoffSessionResponse{}, fmt.Errorf("session %q runs on a host: moving it to %s and to account %q at once is not supported; hand it off to the agent first, then change its account, or use a local session: %w",
			instance.Title, target, to, session.ErrSwapNeedsAccountHandoff)
	}
	if from == to {
		return HandoffSessionResponse{}, fmt.Errorf("session %q already uses %s account %q", instance.Title, outgoing, from)
	}
	// Admission, before anything is stopped.
	if err := instance.ValidateOffBoxAccountSwap(to); err != nil {
		return HandoffSessionResponse{}, err
	}
	if err := instance.Transition(session.BeginHandoff()); err != nil {
		return HandoffSessionResponse{}, err
	}
	res, err := instance.SwapOffBoxAccount(to, false)
	if err != nil {
		_ = instance.Transition(session.AbortHandoff())
		return HandoffSessionResponse{}, err
	}
	m.noteRuntimeReplaced(repoID, instance)
	instance.ClearLimitReached()
	response := HandoffSessionResponse{OK: true, From: outgoing, To: outgoing, FromAccount: res.From, ToAccount: res.To}
	if override := strings.TrimSpace(req.Brief); override != "" {
		instance.SetPrompt(override)
	}
	if err := instance.Transition(session.CommitHandoff()); err != nil {
		return response, &mutationCommittedError{err: err}
	}
	if err := m.persistSettlement(repoID, key, instance); err != nil {
		return response, &mutationCommittedError{err: err}
	}
	if err := m.deliverOffBoxAccountNotice(instance, outgoing, res, session.HandoffReasonManual); err != nil {
		return response, &mutationCommittedError{err: err}
	}
	if res.CleanupErr != nil {
		return response, &mutationCommittedError{err: fmt.Errorf("account handoff of %q completed, but %w; the credentials are removed with the session at the latest", instance.Title, res.CleanupErr)}
	}
	m.info().Printf("handoff: session %q moved from %s to %s", instance.Title, accountSwapIdentity(outgoing, res.From), accountSwapIdentity(outgoing, res.To))
	return response, nil
}

// commitOffBoxAccountSwap is commitNewAccountSwapIdentity for a sandbox session: the
// scheduler's opportunity is re-admitted from fresh facts (the same sources, the
// same fences the caller holds), each candidate is proven by the off-box admission
// instead of the local launch proof, and the first admitted one replaces the
// limited account in place. Nothing is stopped for a candidate that was not
// admitted, and the row names the new account only once the host holds it.
func (m *Manager) commitOffBoxAccountSwap(instance *session.Instance, scheduled *autoAccountSwap, global *config.Config) (session.OffBoxAccountSwap, error) {
	admitted, err := m.admitAccountSwapWith(instance, global, instance.ValidateOffBoxAccountSwap)
	if err != nil {
		return session.OffBoxAccountSwap{}, err
	}
	res, err := instance.SwapOffBoxAccount(admitted.to, true)
	if err != nil {
		return session.OffBoxAccountSwap{}, err
	}
	*scheduled = *admitted
	return res, nil
}

// tryOffBoxAccountSwap runs the in-place swap when the session is a listed-host
// sandbox and reports nil, nil when it is not (the caller takes the local path).
func (m *Manager) tryOffBoxAccountSwap(instance *session.Instance, scheduled *autoAccountSwap, global *config.Config) (*session.OffBoxAccountSwap, error) {
	if scheduled.manual || !instance.OffBoxAccountHandoffApplies() {
		return nil, nil
	}
	res, err := m.commitOffBoxAccountSwap(instance, scheduled, global)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// finishOffBoxFailover settles a completed failover: the agent is live on the new
// account (which also lifts the limit block that belonged to the old one), the row
// is durable, and the replacement is told what it continues.
func (m *Manager) finishOffBoxFailover(repoID, key string, instance *session.Instance, swap *autoAccountSwap, res session.OffBoxAccountSwap) error {
	m.noteRuntimeReplaced(repoID, instance)
	instance.ClearLimitReached()
	if err := instance.Transition(session.ConfirmLive()); err != nil {
		return &mutationCommittedError{err: err}
	}
	if err := m.persistSettlement(repoID, key, instance); err != nil {
		return &mutationCommittedError{err: err}
	}
	if err := m.deliverOffBoxAccountNotice(instance, swap.agent, res, session.HandoffReasonUsageLimit); err != nil {
		return &mutationCommittedError{err: err}
	}
	if res.CleanupErr != nil {
		return &mutationCommittedError{err: fmt.Errorf("limit failover of %q completed, but %w; the credentials are removed with the session at the latest", instance.Title, res.CleanupErr)}
	}
	m.info().Printf("limit failover: session %q moved from %s to %s", instance.Title, accountSwapIdentity(swap.agent, res.From), accountSwapIdentity(swap.agent, res.To))
	return nil
}

// deliverOffBoxAccountNotice tells the replacement agent what it is continuing. A
// failed delivery is reported, not retried: the task prompt stays on the row, so
// `sessions send-prompt` can repeat it.
func (m *Manager) deliverOffBoxAccountNotice(instance *session.Instance, agent string, res session.OffBoxAccountSwap, reason string) error {
	notice := offBoxAccountNotice(agent, res.From, res.To, offBoxAccountReason(reason), instance.GetPrompt())
	status, err := task.WaitForReadyAndSubmitPrompt(context.Background(), instance, notice, nil)
	if err = handoffDeliveryResultError(status, err); err != nil {
		return fmt.Errorf("account handoff of %q completed, but the notice for the new account was not delivered (send the task again with `af sessions send-prompt`): %w", instance.Title, err)
	}
	return nil
}

func offBoxAccountReason(reason string) string {
	if reason == session.HandoffReasonUsageLimit {
		return " after the previous identity reached its usage limit"
	}
	return ""
}
