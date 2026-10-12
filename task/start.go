package task

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// MaxTrustPromptAttempts and trustPromptRetryDelay are THE trust-prompt
// dismissal budget: ~20s, enough for a sequence of dialogs (folder trust, then
// MCP trust) plus the readiness re-wait each one costs.
//
// The budget is exported and the loop below is shared because copying the pair
// is how it broke: the daemon's config agent carried its own 5 × 500ms
// constants under a comment claiming they mirrored these, giving a config-agent
// spawn an eighth of a session's budget and failing it with "trust prompt did
// not dismiss" where a session would have succeeded (#2097). A second copy of
// the numbers cannot drift if there is no second copy — every caller goes
// through DismissTrustPrompt.
const MaxTrustPromptAttempts = 20

var trustPromptRetryDelay = time.Second

// ErrAgentReadiness marks failures that happen before the incoming agent has
// reached a usable composer. ErrPromptDelivery marks the narrower failure after
// readiness was established. Callers that have already crossed an irreversible
// runtime-swap boundary need this distinction: the former must become an inert
// startup-unknown record, while the latter can retain a delivery retry against
// the runtime whose readiness was positively established.
var (
	ErrAgentReadiness = errors.New("agent did not become ready")
	ErrPromptDelivery = errors.New("prompt delivery failed after readiness")
)

// SetTrustPromptTimingForTest compresses the trust-prompt dismissal loop's
// timing — both the backoff between attempts and the readiness poll each retry
// drives — so a test outside this package can exercise the full attempt budget
// without sleeping through ~20s of it. Returns a restore func, matching the
// session.SetBackendFactoryForTest seam. Test-only.
func SetTrustPromptTimingForTest(retryDelay time.Duration) func() {
	oldDelay, oldPoll := trustPromptRetryDelay, waitForReadyPollInterval
	poll := retryDelay
	if poll <= 0 {
		// time.NewTicker panics on a non-positive duration, so the poll floor is
		// the smallest tick rather than the caller's zero.
		poll = time.Nanosecond
	}
	trustPromptRetryDelay, waitForReadyPollInterval = retryDelay, poll
	return func() {
		trustPromptRetryDelay, waitForReadyPollInterval = oldDelay, oldPoll
	}
}

// TrustPromptTarget is a ReadinessTarget that can also dismiss its own first-run
// trust dialog — the narrow contract DismissTrustPrompt drives, implemented both
// by a full session.Instance and by the daemon's bare tmux config agent.
type TrustPromptTarget interface {
	ReadinessTarget
	// CheckAndHandleTrustPrompt dismisses a visible trust dialog and reports
	// whether one was in the way — i.e. whether another check is owed before the
	// pane may be typed into. False is the loop's permission to let the caller
	// deliver the user's prompt, so it must mean an OBSERVED dialog-free pane: a
	// dialog whose dismissal keystroke failed and a pane that could not be read
	// both report true (#3302).
	CheckAndHandleTrustPrompt() bool
}

// DismissTrustPrompt clears an agent's first-run trust dialogs, bounded by the
// canonical budget above.
//
// Dialogs arrive one at a time (folder trust, then MCP trust), so each dismissal
// is followed by a fresh readiness wait before the next check; the loop ends
// when the pane shows no dialog. A nil ctx is treated as context.Background().
func DismissTrustPrompt(ctx context.Context, target TrustPromptTarget) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for attempts := 0; target.CheckAndHandleTrustPrompt(); attempts++ {
		if attempts+1 >= MaxTrustPromptAttempts {
			return fmt.Errorf("trust prompt did not dismiss after %d attempts", MaxTrustPromptAttempts)
		}
		// Honor cancellation while backing off between trust-prompt retries so an
		// abandoned create doesn't sit here sleeping and re-waiting.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(trustPromptRetryDelay):
		}
		if err := WaitForReadyOn(ctx, target); err != nil {
			return err
		}
	}
	return nil
}

// instanceTrustTarget adapts a full session.Instance to TrustPromptTarget,
// reusing the readiness adapter so the trust loop polls the same way
// WaitForReady does.
type instanceTrustTarget struct{ instanceReadinessTarget }

func (t instanceTrustTarget) CheckAndHandleTrustPrompt() bool {
	return t.inst.CheckAndHandleTrustPrompt()
}

// StartAndSendPromptWithConversationCapture is the canonical way to start an
// instance, wait for readiness, handle trust prompts, and optionally send a
// prompt. It is the daemon create path, and separates provisioning from launch
// so a local backend can snapshot the exact command-specific provider store
// after the final cwd exists in the model but before the agent process can
// create a transcript there. Everything after launch — the readiness wait, the
// trust-dialog loop, and prompt delivery — is WaitForReadyAndSendPrompt's
// contract, documented there.
//
// It does NOT mark the instance live; callers must settle liveness themselves
// when appropriate, and the two live callers settle at different moments. The
// daemon create path settles synchronously on return, in finishCreateStart
// (daemon/limit.go), which also translates a usage-limit wall at startup into a
// parked row rather than a live one. The TUI never calls this directly — the
// daemon has been the sole session writer since #960 — so its create settles
// asynchronously instead, in the instanceStartedMsg handler (app/home_update.go),
// once the started row is in the store. Both go through session.ConfirmLive(),
// the #1195 Phase 2d chokepoint. A caller that assumes this function already
// settled liveness leaves the session stuck in its creating state.
//
// ctx bounds the readiness wait it delegates; a nil ctx is treated as
// context.Background().
func StartAndSendPromptWithConversationCapture(
	ctx context.Context,
	instance *session.Instance,
	prompt string,
) (session.ConversationCaptureSnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	plan, err := instance.PrepareCreateLaunch()
	if err != nil {
		return session.ConversationCaptureSnapshot{}, err
	}
	capture := plan.ConversationCapture()
	if err := instance.LaunchPreparedCreate(plan); err != nil {
		return capture, err
	}
	return capture, WaitForReadyAndSendPrompt(ctx, instance, prompt)
}

// WaitForReadyAndSendPrompt is the post-launch half of
// StartAndSendPromptWithConversationCapture. A handoff has already launched its
// incoming pane, but it owes the same readiness and trust-dialog contract as a
// fresh create before any mission text is typed. Keeping that contract here
// prevents the two launch paths from drifting.
//
// It always waits for the program to become ready. If prompt is non-empty, it
// sends the prompt after readiness (and after the trust dialog is cleared),
// never before — a prompt typed into a pane that is not yet at its composer is
// lost or mangled.
//
// ctx bounds the readiness wait: an abandoned or cancelled create tears down the
// pane-poll instead of spinning to the timeout (see WaitForReady). A nil ctx is
// treated as context.Background().
func WaitForReadyAndSendPrompt(ctx context.Context, instance *session.Instance, prompt string) error {
	_, err := WaitForReadyAndSendPromptWithStatus(ctx, instance, prompt)
	return err
}

// WaitForReadyAndSendPromptWithStatus is the status-bearing form of
// WaitForReadyAndSendPrompt. Callers with a durable delivery obligation must
// inspect the returned verdict: a nil error says the submission call completed,
// while only PromptDelivered proves the prompt landed.
func WaitForReadyAndSendPromptWithStatus(
	ctx context.Context,
	instance *session.Instance,
	prompt string,
) (session.PromptDeliveryStatus, error) {
	return WaitForReadyAndSubmitPrompt(ctx, instance, prompt, nil)
}

// WaitForReadyAndSubmitPrompt is WaitForReadyAndSendPromptWithStatus with a
// pre-submission boundary, for callers that owe a durable record of the attempt
// (#4429). beforeSubmit runs once readiness and the trust dialog have proved
// the runtime, and before the composer is touched. That is the one point where
// a could-not-confirm attempt marker is both needed (a crash after it may have
// submitted) and true about the runtime; a crash inside the readiness wait
// then reloads the verdict that admitted the attempt.
//
// If beforeSubmit fails, nothing is sent: the result is PromptNotDelivered with
// beforeSubmit's own error, wrapped by neither ErrAgentReadiness nor
// ErrPromptDelivery. A nil beforeSubmit, or an empty prompt, skips it.
func WaitForReadyAndSubmitPrompt(
	ctx context.Context,
	instance *session.Instance,
	prompt string,
	beforeSubmit func() error,
) (session.PromptDeliveryStatus, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	boundary := func() error {
		if prompt == "" || beforeSubmit == nil {
			return nil
		}
		return beforeSubmit()
	}

	// Readiness polling, trust-prompt dismissal and prompt delivery below all
	// drive the agent's PTY locally; a backend without interactive input (remote
	// hook) handles readiness and prompts on its own host, so skip them.
	if !instance.Capabilities().InteractiveInput {
		if err := boundary(); err != nil {
			return session.PromptNotDelivered, err
		}
		return session.PromptDelivered, nil
	}

	// The readiness wait below proves the composer idle — but the observation
	// is a text capture that carries no locked instant, so the proof is bound
	// by BRACKETING instead: the prompt-send generation is sampled before the
	// wait, then re-checked inside the send's own record lock. Any prompt
	// recording inside the (sample, record] window — a manual send racing
	// the wait's polls or the trust-dismiss/boundary gap after it — has
	// already moved the generation past the sample, so the record owes the
	// stillness base rather than seeding it from a proof that predates that
	// send (#5221 review). Sampling inside or after the wait would capture a
	// racing send's NEWER generation and wrongly report the stale proof
	// current.
	readyEpoch := instance.PromptRecordEpoch()

	if err := WaitForReady(ctx, instance); err != nil {
		return session.PromptCouldNotConfirm, fmt.Errorf("%w: %w", ErrAgentReadiness, err)
	}

	if err := DismissTrustPrompt(ctx, instanceTrustTarget{instanceReadinessTarget{inst: instance}}); err != nil {
		return session.PromptCouldNotConfirm, fmt.Errorf("%w: %w", ErrAgentReadiness, err)
	}

	if err := boundary(); err != nil {
		return session.PromptNotDelivered, err
	}

	if prompt != "" {
		// The stillness base seeds only from an ACTUAL idle observation — a
		// strict prompt frame at send time. The readiness wait tolerates
		// signals that are not that: a line-leading glyph can be scrollback,
		// a trust dialog is a modal not a composer, and a program override
		// calls any non-blank pane ready — a wait that passed on any of
		// those proves nothing about what the pane was doing, so the send
		// must not carry the bracket it brackets (#5221 review).
		readyArg := session.NoReadinessProof
		if content, err := capturePreview(ctx, instance); err == nil &&
			provenIdleContent(content, instance.ResolvedAgent()) {
			readyArg = readyEpoch
		}
		status, sendEpoch, err := instance.SendPromptWithEvidence(prompt, time.Now, readyArg)
		if err != nil {
			return status, fmt.Errorf("%w: %w", ErrPromptDelivery, err)
		}
		// The agent's own mid-turn chrome serves two verdicts here (#4429,
		// #5219): for sent-unverified it is the one positive delivery proof
		// left, and for delivered on a TASK RUN it is the run's turn boundary
		// when the scoped indicator can appear before the daemon's first poll
		// — a daemon watcher's first sight of it already present can never be
		// attributed, so the turn would wait for the quiet fallback. An
		// ordinary prompted create has no run boundary to record, so a
		// delivered send there skips the watch rather than burn its budget
		// (#5221 review). Readiness proved the composer idle before the send,
		// so chrome in this window is this submission's work starting. A miss
		// claims nothing; signatureless agents have no chrome to find, so
		// they skip the watch as well.
		//
		// The stamp is fenced to the epoch the send itself returned — sampled
		// inside the record's own lock, so a manual send landing in the gap
		// before this read (it takes no lock this path holds) cannot hand the
		// watch the newer boundary's generation while the pane still ticks
		// the earlier prompt's turn. Stamping then would retire the NEW
		// window with the old turn's evidence, and the earlier turn's idle
		// edge could apply on_complete before the queued prompt starts
		// (#5221 review). It also requires the send's OWN strict-idle proof:
		// a redelivery that passed only lenient readiness may have landed
		// while an EARLIER turn still ticks, and the chrome in this window
		// would then be that turn's indicator attributed to this boundary —
		// the same pre-boundary evidence the epoch fence exists to reject
		// (#5221 review).
		if TurnSignaturedAgent(instance.ResolvedAgent()) &&
			readyArg != session.NoReadinessProof &&
			(status == session.PromptSentUnverified ||
				(status == session.PromptDelivered && instance.TaskRunActive())) &&
			submittedTurnVisible(ctx, instanceReadinessTarget{inst: instance}) {
			instance.RecordTaskRunTurnAtEpoch(time.Now(), sendEpoch)
			if status == session.PromptSentUnverified {
				// The UPGRADE is fenced the same way the stamp is: a
				// concurrent send landing inside the watch window can tick
				// the indicator — only an unbroken epoch proves this chrome
				// is OUR send's turn. Upgrading on anything else retires a
				// delivery obligation on someone else's work (#5221 review).
				if _, epoch := instance.InFlightOpAndEpoch(); epoch == sendEpoch {
					return session.PromptDelivered, nil
				}
			}
		}
		return status, nil
	}

	return session.PromptDelivered, nil
}
