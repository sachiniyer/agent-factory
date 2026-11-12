package session

import (
	"errors"
	"fmt"
)

// RespawnForAccountSwap starts the replacement identity in a fresh provider
// conversation. Local recovery normally resumes the prior conversation, which
// belongs to the previous account's separate home; Docker already provisions a
// fresh sandbox and launch through its ordinary Respawn implementation.
func (i *Instance) RespawnForAccountSwap() error {
	_, err := i.RespawnForAccountSwapWithLiveBoundary(nil)
	return err
}

// RespawnForAccountSwapWithLiveBoundary is the fresh-conversation account
// replacement with the same pre-ConfirmLive callback used by ordinary respawn.
// The idle-evidence mechanism owns that boundary; account swapping only routes
// its distinct launch through it.
//
// It returns the provider-store before-image of the plan that actually
// launched, which is the snapshot conversation discovery must judge the
// replacement against. That can differ from the plan admission froze: the
// local respawn re-runs a committed carry's copy, and a copy that can no
// longer land demotes the plan to a fresh start with a before-image of its own
// (#7140). Discovering against the admitted carried snapshot would confirm a
// carried id onto a fresh runtime, or refuse a store as a carried swap would.
func (i *Instance) RespawnForAccountSwapWithLiveBoundary(beforeLive func()) (ConversationCaptureSnapshot, error) {
	if op := i.GetInFlightOp(); op != OpRespawning {
		return ConversationCaptureSnapshot{}, fmt.Errorf("account respawn of %q requires the limit-resume fence (in-flight op is %s)", i.Title, opLabel(op))
	}
	var launched ConversationCaptureSnapshot
	if err := i.withLiveBoundary(beforeLive, func() error {
		backend := i.currentBackend()
		if local, ok := backend.(*LocalBackend); ok {
			var err error
			launched, err = local.respawnFresh(i)
			return err
		}
		// Other backends launch the plan admission froze, unchanged.
		var err error
		if launched, err = i.AccountSwapConversationCapture(); err != nil {
			return err
		}
		return backend.Respawn(i)
	}); err != nil {
		return ConversationCaptureSnapshot{}, err
	}
	return launched, i.markAccountSwapReplacementPanesStarted()
}

// respawnFresh launches the replacement from its committed plan and returns
// the before-image of the plan it launched (see
// RespawnForAccountSwapWithLiveBoundary).
func (b *LocalBackend) respawnFresh(i *Instance) (ConversationCaptureSnapshot, error) {
	plan, err := i.accountSwapLaunchForRespawn()
	if err != nil {
		return ConversationCaptureSnapshot{}, err
	}
	// The outgoing panes are already stopped: every environment refresh below —
	// worktree hooks, the agent pane, each sibling tab — applies the list
	// admission resolved instead of re-reading config, which could now fail only
	// after refusing would have kept an agent running (#7077).
	if plan.passthroughFrozen {
		defer i.freezeEnvPassthrough(plan.passthrough)()
	}
	// Idempotent, and the only copy a restarted daemon performs. A carry that
	// can no longer complete — at this copy or already at validation — leaves
	// this launch and the pending record as a stated fresh start.
	if plan, err = i.ensureAccountSwapConversationCarried(plan); err != nil {
		return ConversationCaptureSnapshot{}, err
	}
	if plan.carry != nil {
		i.markCarriedLaunchStarted(plan.account)
	}
	if err := b.respawnWithConversation(i, false, plan); err != nil {
		stopErr := b.stopForAccountSwap(i, false)
		return ConversationCaptureSnapshot{}, fmt.Errorf("account swap: replacement pane set for %q is incomplete: %w",
			i.Title, errors.Join(err, stopErr))
	}
	return cloneConversationCaptureSnapshot(plan.conversationCapture), nil
}
