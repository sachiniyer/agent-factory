package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// handoffDelivery is the post-launch half of a replacement transaction. Keeping
// these values together makes every terminal branch settle the same instance,
// mission, persistence key, and command-specific conversation capture.
type handoffDelivery struct {
	repoID              string
	key                 string
	title               string
	target              string
	mission             string
	instance            *session.Instance
	conversationCapture session.ConversationCaptureSnapshot
}

// prepareHandoffDelivery crosses the runtime-identity boundary exactly once.
// A limit observation belongs to the outgoing provider, so constructing the
// incoming delivery obligation clears it while leaving OpReplacing raised. A
// newly observed incoming limit is classified independently by the readiness
// result in deliverHandoffMission.
func prepareHandoffDelivery(delivery handoffDelivery) handoffDelivery {
	delivery.instance.ClearLimitReached()
	return delivery
}

func (m *Manager) deliverHandoffMission(delivery handoffDelivery) error {
	settle := func(event session.TransitionEvent, clearPending bool) error {
		if err := delivery.instance.Transition(event); err != nil {
			return err
		}
		if clearPending && !delivery.instance.ClearPendingHandoffMission(delivery.mission) {
			return fmt.Errorf("pending handoff mission changed before settlement")
		}
		// Persist only settled outcomes. Disk persistence strips transient ops, so
		// writing while OpReplacing is raised would manufacture an unfenced state
		// no in-memory reader was ever allowed to observe.
		m.persistInstance(delivery.repoID, delivery.instance)
		m.captureAgentConversationAsync(delivery.repoID, delivery.key, delivery.instance, delivery.conversationCapture)
		return nil
	}

	serr := task.WaitForReadyAndSendPrompt(context.Background(), delivery.instance, delivery.mission)
	if serr == nil {
		if err := settle(session.CommitHandoff(), true); err != nil {
			return fmt.Errorf(
				"handed %q off to %s and delivered its mission, but could not settle the replacement fence: %w",
				delivery.title, delivery.target, err)
		}
		return nil
	}

	var limitErr *task.LimitReachedError
	if errors.As(serr, &limitErr) {
		// The mission never reached the composer. Store the rendered takeover
		// context — not the old create prompt or bare override — because the
		// normal limit-resume path replays exactly Instance.Prompt.
		delivery.instance.SetPrompt(delivery.mission)
		if terr := settle(session.ParkHandoff(limitErr.ResetAt), true); terr != nil {
			return fmt.Errorf(
				"handed %q off to %s, which hit a usage limit before its mission could be delivered, and failed to park the handoff: %w",
				delivery.title, delivery.target, errors.Join(serr, terr))
		}
		return fmt.Errorf(
			"handed %q off to %s, which hit a usage limit before its mission could be delivered; "+
				"the rendered mission is saved and will be sent by the normal limit-resume path: %w",
			delivery.title, delivery.target, serr)
	}
	if !errors.Is(serr, task.ErrPromptDelivery) {
		// Readiness/trust failures do not prove a usable incoming runtime. Retain
		// the record and pending mission, but make it inert: claiming Running here
		// turns a startup death into a fake delivery failure and invites commands
		// into an unconfirmed binding.
		delivery.instance.MarkStartupStateUnknown()
		m.persistInstance(delivery.repoID, delivery.instance)
		return fmt.Errorf(
			"handed %q off to %s, but the incoming agent never reached a confirmed ready state (%w); "+
				"the session was retained as startup-unknown with its mission pending for inspection",
			delivery.title, delivery.target, serr)
	}
	// Readiness proved this is the incoming runtime, so conversation capture is
	// safe even though its mission remains behind the durable retry fence. The
	// later recovery pass no longer has the command-specific capture plan. The
	// delivery constructor already cleared every predecessor-scoped limit, so the
	// retry cannot be diverted into the outgoing provider's reset schedule.
	m.captureAgentConversationAsync(delivery.repoID, delivery.key, delivery.instance, delivery.conversationCapture)
	return fmt.Errorf(
		"handed %q off to %s, but its mission brief could not be delivered (%w); "+
			"the exact mission remains pending behind the replacement fence for a readiness-based retry "+
			"(the outgoing provider's limit state was cleared at the runtime boundary)",
		delivery.title, delivery.target, serr)
}
