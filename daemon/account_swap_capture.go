package daemon

import (
	"errors"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// captureAccountSwapConversation binds Codex discovery to the replacement
// runtime while the limit-resume operation still owns its fence. When Codex has
// already minted a rollout, account swaps cannot use the ordinary asynchronous
// capture: that goroutine serializes its write through the same per-session
// operation lock held by the caller, so the pending recovery marker could
// otherwise be cleared and checkpointed before the conversation id became
// durable.
//
// The capture window also owns the trap fixed in #4393: a fresh account home may
// still paint its directory-trust dialog after the readiness check (#4392), the
// status poll skips a pending-swap row, and an automatic swap's notice is sent
// by a path that dismisses nothing (deliverAutomaticAccountNotice). Pump the
// existing guarded recognizer so the replacement can reach its composer. The
// pump is an obligation of its own, bounded by accountSwapTrustWindow and
// separate from the capture's work: a capture that answers at once — a carried
// confirmation, a failed baseline, no evidence to judge from — does no store
// walk or attribution wait, yet the pump still runs to the end of its window
// before the swap moves on to its notice (#7140). Reaching the composer does not itself mint a rollout; the
// no-rollout branch below handles that separate #4712 ordering constraint. The
// boolean result asks the caller to capture again after delivery creates the
// rollout and retires the pending recovery marker.
//
// The replacement has already launched when this runs, so an error here fails
// a handoff after its runtime boundary was crossed. A fresh capture therefore
// errors only on what the evidence said — two threads the runtime holds, a
// rollout header that never became classifiable. A channel it merely cannot
// read — a pre-launch baseline that failed, a store scan or open-file walk
// that fails inside the window (session.ErrCodexCaptureChannelUnavailable) —
// defers to the post-delivery capture like the no-rollout case, which asks the
// runtime what it holds and reports why when it cannot (#7126). A carried
// confirmation still refuses an unreadable store: confirming the carried
// thread past it would hide the breakage.
func captureAccountSwapConversation(instance *session.Instance, snap session.ConversationCaptureSnapshot) (bool, error) {
	if instance.AgentRuntimeToken().Agent() != tmux.ProgramCodex {
		return false, nil
	}
	pump := startReplacementTrustPump(instance, swapTrustWindow())
	defer pump.stop()
	captureAfterDelivery, err := captureReplacementCodexConversation(instance, snap, pump)
	if err == nil {
		// The swap goes on to its notice next.
		pump.throughWindow()
	}
	return captureAfterDelivery, err
}

func captureReplacementCodexConversation(instance *session.Instance, snap session.ConversationCaptureSnapshot, pump *replacementTrustPump) (bool, error) {
	token := instance.AgentRuntimeToken()
	carried := snap.CarriesConversation()
	if !carried && snap.CodexBaselineErr() != nil {
		// The diff can never answer from a failed before-image, so nothing
		// re-reads that failure; the caller still pumps the window.
		return true, nil
	}
	// Rollout adoption needs the replacement's own runtime as evidence, not
	// just the store and directory its launch was composed against (#6847).
	// Re-verify an identity kept for capture first (#7123); with none, the
	// fresh capture returns at once and the swap defers (#7124 review F2).
	_ = instance.CheckConversationCaptureEvidence()
	snap = instance.BindConversationCaptureRuntime(snap)
	resultCh := make(chan swapCaptureResult, 1)
	captureTimeout := conversationCaptureTimeout // read at spawn: the pump below may abandon the worker
	go func() {
		conversation, err := session.CaptureAgentConversation(token.Agent(), snap, captureTimeout)
		resultCh <- swapCaptureResult{conversation, err}
	}()
	res := pump.until(resultCh)
	if res.err != nil {
		if !carried && errors.Is(res.err, session.ErrCodexCaptureChannelUnavailable) {
			return true, nil
		}
		return false, fmt.Errorf("capture replacement Codex conversation: %w", res.err)
	}
	if !res.conversation.HasID() {
		// A fresh Codex runtime does not create a rollout merely by reaching its
		// composer. The first submitted message creates it, and this capture runs
		// before mission delivery so a capture failure can never turn a delivered
		// mission into an ambiguous retry. No rollout is therefore the expected
		// fresh-conversation fallback, not a failed replacement (#4712). Tell the
		// caller to capture again only after the first mission has landed and the
		// pending recovery marker has been retired.
		return true, nil
	}
	if !instance.RecordAccountSwapConversationForRuntime(token, res.conversation) {
		return false, errors.New("replacement Codex runtime changed before its conversation id could be recorded")
	}
	return false, nil
}

type swapCaptureResult struct {
	conversation session.AgentConversationData
	err          error
}

// replacementTrustPump runs the guarded trust-dialog dismissal on a
// replacement's pane every accountSwapTrustDismissInterval, from readiness
// until at least window has passed.
type replacementTrustPump struct {
	instance  *session.Instance
	windowEnd time.Time
	ticker    *time.Ticker
}

func startReplacementTrustPump(instance *session.Instance, window time.Duration) *replacementTrustPump {
	return &replacementTrustPump{
		instance:  instance,
		windowEnd: time.Now().Add(window),
		ticker:    time.NewTicker(accountSwapTrustDismissInterval),
	}
}

func (p *replacementTrustPump) stop() { p.ticker.Stop() }

// until pumps until the capture answers, however long that takes.
func (p *replacementTrustPump) until(result <-chan swapCaptureResult) swapCaptureResult {
	for {
		select {
		case res := <-result:
			return res
		case <-p.ticker.C:
			p.instance.CheckAndHandleTrustPrompt()
		}
	}
}

// throughWindow pumps until the window ends; past it, it returns at once.
func (p *replacementTrustPump) throughWindow() {
	end := time.NewTimer(time.Until(p.windowEnd))
	defer end.Stop()
	for {
		select {
		case <-end.C:
			return
		case <-p.ticker.C:
			p.instance.CheckAndHandleTrustPrompt()
		}
	}
}
