package daemon

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
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
	if target := strings.TrimSpace(req.To); target != "" {
		effective, err := session.HandoffEffectiveAgentForPath(instance.Path, target)
		if err != nil {
			return HandoffSessionResponse{}, fmt.Errorf("cannot hand %q off to %s: %w", instance.Title, target, err)
		}
		if !session.HandoffTargetIsCurrent(outgoing, target, effective, instance.AgentProgram()) {
			return HandoffSessionResponse{}, fmt.Errorf("session %q runs on a host: moving it to %s and to account %q at once is not supported; hand it off to the agent first, then change its account, or use a local session: %w",
				instance.Title, target, to, session.ErrSwapNeedsAccountHandoff)
		}
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
	// The in-flight record is durable before the request crosses (#7793):
	// settlement-class, so it writes under this verb's own op lock — the
	// ordering fence persistSettlementUnderAdmission names — never under the
	// repo start lock the project config file lock forbids spanning (#6874).
	instance.SetOffBoxSwapPersist(func(i *session.Instance) error {
		return m.persistSettlementUnderAdmission(repoID, key, i)
	})
	// The takeover brief is the durable delivery obligation (#7793 review):
	// the commit stamps it as the pending mission in the same checkpoint that
	// retires the in-flight record, so a daemon exit before delivery reloads
	// a mission recovery replays — never a completable run with nothing owed.
	prompt := instance.GetPrompt()
	if override := strings.TrimSpace(req.Brief); override != "" {
		prompt = override
	}
	notice := offBoxAccountNotice(outgoing, from, to, offBoxAccountReason(session.HandoffReasonManual), prompt)
	res, err := instance.SwapOffBoxAccount(to, false, notice)
	if err != nil {
		if errors.Is(err, session.ErrAccountSwapIndeterminate) {
			// The host's account could not be established: the row stays fenced as
			// startup-unknown and nothing here asserts either account.
			_ = m.persistSettlement(repoID, key, instance)
			_ = instance.Transition(session.AbortHandoff())
			return HandoffSessionResponse{}, &mutationCommittedError{err: err}
		}
		_ = instance.Transition(session.AbortHandoff())
		return HandoffSessionResponse{}, err
	}
	// No ClearLimitReached here: the swap's commit already retired the
	// outgoing account's wall inside the checkpoint that names the new one
	// (#7799). A clear at this point would be a no-op that only suggests the
	// wall outlives the commit.
	m.noteRuntimeReplaced(repoID, instance)
	response := HandoffSessionResponse{OK: true, From: outgoing, To: outgoing, FromAccount: res.From, ToAccount: res.To,
		Conversation: session.HandoffConversation{CarryFailure: offBoxAccountNoConversation}}
	if override := strings.TrimSpace(req.Brief); override != "" {
		instance.SetPrompt(override)
	}
	// The replacement fence stays raised through the notice's submission: until
	// the new agent has its task, a status poll that sees its idle composer must
	// not read the run as finished. Whatever the delivery's outcome, the fence is
	// lowered after it.
	noticeErr := m.finishOffBoxNotice(instance, res, repoID, key)
	if err := instance.Transition(session.CommitHandoff()); err != nil {
		return response, &mutationCommittedError{err: errors.Join(noticeErr, err)}
	}
	if err := m.persistSettlement(repoID, key, instance); err != nil {
		return response, &mutationCommittedError{err: errors.Join(noticeErr, err)}
	}
	if noticeErr != nil {
		return response, &mutationCommittedError{err: noticeErr}
	}
	m.info().Printf("handoff: session %q moved from %s to %s", instance.Title, accountSwapIdentity(outgoing, res.From), accountSwapIdentity(outgoing, res.To))
	return response, nil
}

// finishOffBoxNotice delivers the replacement's notice unless the swap was only
// reconciled (its response was lost, so the new agent's start is unconfirmed), and
// folds a credential-cleanup failure and any skipped agent-config files into the
// result.
func (m *Manager) finishOffBoxNotice(instance *session.Instance, res session.OffBoxAccountSwap, repoID, key string) error {
	var errs []error
	if res.Reconciled {
		// The commit already stamped the takeover brief as the row's pending
		// mission with a not-delivered verdict (#7793 review), so the brief is
		// owed, durably, and handoff recovery replays it once the new agent
		// proves ready. Sending the operator to relaunch or resend by hand
		// would race that replay and could hand the agent the brief twice
		// (#7799).
		errs = append(errs, fmt.Errorf("the swap of %q to account %q completed on the host but its response was lost, so the new agent's start is unconfirmed and its takeover brief was not sent; the brief stays the session's pending mission, and handoff recovery delivers it once the new agent is confirmed ready — it needs no resend", instance.Title, res.To))
	} else if err := m.deliverOffBoxAccountNotice(instance, res, repoID, key); err != nil {
		errs = append(errs, err)
	}
	if res.CleanupErr != nil {
		errs = append(errs, fmt.Errorf("account handoff of %q completed, but %w; the credentials are removed with the session at the latest", instance.Title, res.CleanupErr))
	}
	// The staged scope is live even though some declared personalization never
	// landed in it — name the skip under the cross-surface prefix so the CLI's
	// handoff warning and the session row tell the same story (#6337).
	if len(res.AgentConfigSkipped) > 0 {
		errs = append(errs, fmt.Errorf("account handoff of %q completed, but %s%s", instance.Title, session.AgentConfigSkippedPrefix, strings.Join(res.AgentConfigSkipped, "; ")))
	}
	return errors.Join(errs...)
}

// commitOffBoxAccountSwap is commitNewAccountSwapIdentity for a sandbox session: the
// scheduler's opportunity is re-admitted from fresh facts (the same sources, the
// same fences the caller holds), each candidate is proven by the off-box admission
// instead of the local launch proof, and the first admitted one replaces the
// limited account in place. Nothing is stopped for a candidate that was not
// admitted, and the row names the new account only once the host holds it.
func (m *Manager) commitOffBoxAccountSwap(instance *session.Instance, repoID, key string, scheduled *autoAccountSwap, global *config.Config) (session.OffBoxAccountSwap, error) {
	admitted, err := m.admitAccountSwapWith(instance, global, instance.ValidateOffBoxAccountSwap)
	if err != nil {
		return session.OffBoxAccountSwap{}, err
	}
	// The in-flight record's durable write is settlement-class (#7793): this
	// path's per-session op lock is the ordering fence
	// persistSettlementUnderAdmission names, and the repo start lock is
	// deliberately never taken — the project config file lock this swap runs
	// under must not span the fence <-> start-lock order (#6874).
	instance.SetOffBoxSwapPersist(func(i *session.Instance) error {
		return m.persistSettlementUnderAdmission(repoID, key, i)
	})
	// Same obligation as the manual path: the commit's checkpoint records the
	// exact takeover brief recovery owes the replacement (#7793 review).
	notice := offBoxAccountNotice(admitted.agent, admitted.from, admitted.to,
		offBoxAccountReason(session.HandoffReasonUsageLimit), instance.GetPrompt())
	res, err := instance.SwapOffBoxAccount(admitted.to, true, notice)
	if err != nil {
		return session.OffBoxAccountSwap{}, err
	}
	*scheduled = *admitted
	return res, nil
}

// tryOffBoxAccountSwap runs the in-place swap when the session is a listed-host
// sandbox and reports nil, nil when it is not (the caller takes the local path).
func (m *Manager) tryOffBoxAccountSwap(instance *session.Instance, repoID, key string, scheduled *autoAccountSwap, global *config.Config, fallbackEligible *bool) (*session.OffBoxAccountSwap, error) {
	if scheduled.manual || !instance.OffBoxAccountHandoffApplies() {
		return nil, nil
	}
	res, err := m.commitOffBoxAccountSwap(instance, repoID, key, scheduled, global)
	if err != nil {
		if errors.Is(err, session.ErrAccountSwapIndeterminate) {
			// An ordinary same-account retry would relaunch into a scope whose
			// account is unknown.
			*fallbackEligible = false
		}
		return nil, err
	}
	return &res, nil
}

// finishOffBoxFailover settles a completed failover: the agent is live on the new
// account, the row is durable, and the replacement is told what it continues. The
// limit block that belonged to the old account is already lifted — the swap's
// commit retires it in the same checkpoint that names the new account (#7799), so
// a daemon death before this tail can never reload the new account carrying the
// old one's wall. Nothing here re-parks: a wall the new account hits later is its
// own, observed fresh.
func (m *Manager) finishOffBoxFailover(repoID, key string, instance *session.Instance, swap *autoAccountSwap, res session.OffBoxAccountSwap) error {
	m.noteRuntimeReplaced(repoID, instance)
	// The limit-resume fence is still raised while the notice is submitted, so a
	// poll cannot read the fresh agent's idle composer as a finished run.
	noticeErr := m.finishOffBoxNotice(instance, res, repoID, key)
	if err := instance.Transition(session.ConfirmLive()); err != nil {
		return &mutationCommittedError{err: errors.Join(noticeErr, err)}
	}
	if err := m.persistSettlement(repoID, key, instance); err != nil {
		return &mutationCommittedError{err: errors.Join(noticeErr, err)}
	}
	if noticeErr != nil {
		return &mutationCommittedError{err: noticeErr}
	}
	m.info().Printf("limit failover: session %q moved from %s to %s", instance.Title, accountSwapIdentity(swap.agent, res.From), accountSwapIdentity(swap.agent, res.To))
	return nil
}

// deliverOffBoxAccountNotice submits the takeover brief the swap committed as
// the row's durable pending mission (#7793 review). It runs the same lifecycle
// every handoff mission runs: the ambiguous-attempt marker lands durably before
// the composer is touched, the runtime verdict is bound to THIS mission, and a
// delivered verdict retires the obligation. Anything else keeps it pending —
// positive non-delivery for automatic recovery, an ambiguous verdict for the
// operator's confirm-or-retry exits. A reconciled swap reaches finishOffBoxNotice
// with the mission still owed; recovery's replay is the notice the unconfirmed
// start could not take.
func (m *Manager) deliverOffBoxAccountNotice(instance *session.Instance, res session.OffBoxAccountSwap, repoID, key string) error {
	mission := instance.PendingHandoffMission()
	if mission == "" {
		return fmt.Errorf("account handoff of %q completed but its takeover mission was not recorded; refusing to deliver an unrecorded notice", instance.Title)
	}
	delivery := handoffDelivery{
		repoID: repoID, key: key, title: instance.Title,
		target: res.To, mission: mission, instance: instance,
	}
	status, serr, beginErr := m.waitAndSubmitHandoffMission(delivery, true)
	if beginErr != nil {
		return beginErr
	}
	if evidenceErr := instance.RecordPendingHandoffMissionDelivery(mission, status); evidenceErr != nil {
		return errors.Join(serr, evidenceErr)
	}
	if err := handoffDeliveryResultError(status, serr); err != nil {
		// The text names the obligation that actually remains (#7799). A
		// recorded not-delivered verdict is the one recovery replays on its
		// own, so a `send-prompt` here would race that replay and deliver the
		// brief twice; every other verdict is the operator's confirm-or-retry
		// decision, worded exactly as the local handoff path words it.
		if instance.PendingHandoffMissionAutoRetryable() {
			return fmt.Errorf("account handoff of %q completed, but the takeover brief for the new account was not delivered (%w); it stays the session's pending mission and handoff recovery replays it automatically — `af sessions retry-limit` resends it by hand", instance.Title, err)
		}
		return fmt.Errorf("account handoff of %q completed, but could not confirm whether the takeover brief for the new account landed (%w); the exact mission remains pending — inspect the pane, then `af sessions retry-limit` resends it or `af sessions retry-limit --delivered` retires it", instance.Title, err)
	}
	if !instance.ClearPendingHandoffMission(mission) {
		return fmt.Errorf("account handoff of %q delivered, but its pending mission changed before settlement", instance.Title)
	}
	return nil
}

func offBoxAccountReason(reason string) string {
	if reason == session.HandoffReasonUsageLimit {
		return " after the previous identity reached its usage limit"
	}
	return ""
}
