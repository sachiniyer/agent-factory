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
// status poll skips a pending-swap row, and delivery's own dismissal runs only
// after this returns. Pump the existing guarded recognizer so the replacement
// can reach its composer. Reaching it does not itself mint a rollout; the
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
	token := instance.AgentRuntimeToken()
	if token.Agent() != tmux.ProgramCodex {
		return false, nil
	}
	carried := snap.CarriesConversation()
	if !carried && snap.CodexBaselineErr() != nil {
		// The diff can never answer from a failed before-image; spending the
		// window re-reading that failure under the op-lock buys nothing.
		return true, nil
	}
	// Rollout adoption needs the replacement's own runtime as evidence, not
	// just the store and directory its launch was composed against (#6847).
	// Re-verify an identity kept for capture first (#7123); with none, the
	// fresh capture returns at once and the swap defers (#7124 review F2).
	_ = instance.CheckConversationCaptureEvidence()
	snap = instance.BindConversationCaptureRuntime(snap)
	type captureResult struct {
		conversation session.AgentConversationData
		err          error
	}
	resultCh := make(chan captureResult, 1)
	captureTimeout := conversationCaptureTimeout // read at spawn: the loop below may abandon the worker
	go func() {
		conversation, err := session.CaptureAgentConversation(token.Agent(), snap, captureTimeout)
		resultCh <- captureResult{conversation, err}
	}()
	var res captureResult
	ticker := time.NewTicker(accountSwapTrustDismissInterval)
	for done := false; !done; {
		select {
		case res = <-resultCh:
			done = true
		case <-ticker.C:
			instance.CheckAndHandleTrustPrompt()
		}
	}
	ticker.Stop()
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
