package daemon

import (
	"errors"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// finishCreateStart settles a freshly created instance after
// StartAndSendPromptWithConversationCapture returns (#1146 PR4). It is the
// single place CreateSession translates that outcome into liveness:
//   - startErr nil: the agent came up — mark it live.
//   - startErr is a usage-limit park (task.LimitReachedError): the agent hit a
//     usage-limit wall during startup before the prompt could be delivered. KEEP
//     the session — mark it limit-blocked with its parsed reset time and stash
//     the prompt so the resume machinery (the daemon auto-resume scheduler when
//     limit_auto_resume is on, or the manual `c` retry) re-delivers it once the
//     window resets. Return nil so CreateSession registers+persists it as a
//     parked row, not a failed one, firing no failure side-effects. The stored
//     prompt is the only input resumeFromLimitOutcome re-delivers, so it is set here so
//     a parked task run resumes its OWN work rather than a bare "continue". A
//     later handoff brief may replace this durable goal; both paths use Instance's
//     prompt accessors so the resume scheduler cannot race the handoff writer.
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
		// The create path always sights the wall on a pane it just spawned —
		// freshRuntime keeps the sighting from ever proving the residue of a
		// wall a prior runtime recorded (#6972).
		instance.SetLimitReachedAtEvidence(limitErr.ResetAt, limitErr.Evidence, limitBannerOf(limitErr), true)
		instance.SetPrompt(prompt)
		return nil
	}
	return startErr
}

// limitBannerOf adapts a send-path park error's detection anchor (#6823) to the
// session's recorded-sighting form.
func limitBannerOf(limitErr *task.LimitReachedError) session.LimitBanner {
	return session.LimitBanner{Prefix: limitErr.BannerPrefix, Count: limitErr.BannerCount}
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
