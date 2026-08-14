package tmux

import (
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// PromptDeliveryStatus is the closed set of outcomes a caller may learn from
// the pane observation performed during prompt submission. It reports only
// what the daemon observed; neither unverified status guesses whether the
// agent ultimately received the prompt.
type PromptDeliveryStatus string

const (
	PromptDelivered    PromptDeliveryStatus = "delivered"
	PromptNotDelivered PromptDeliveryStatus = "not-delivered"
	// PromptSentUnverified means tmux accepted both the paste and Enter while a
	// readable pane never rendered prompt-specific proof. Real Claude and Codex
	// panes collapse long pastes into placeholders, so this is deliberately not
	// promoted to PromptDelivered.
	PromptSentUnverified  PromptDeliveryStatus = "sent-unverified"
	PromptCouldNotConfirm PromptDeliveryStatus = "could-not-confirm"
)

// Valid reports whether the status is one of the four wire-supported
// outcomes. An empty or future value from an older/newer peer is not evidence
// of delivery and must be normalized to PromptCouldNotConfirm by callers.
func (s PromptDeliveryStatus) Valid() bool {
	return s == PromptDelivered || s == PromptNotDelivered ||
		s == PromptSentUnverified || s == PromptCouldNotConfirm
}

func (o deliveryOutcome) promptDeliveryStatus() PromptDeliveryStatus {
	switch o {
	case deliveryObservedLanded:
		return PromptDelivered
	case deliveryObservedAbsent:
		return PromptNotDelivered
	case deliveryObservedUnverified:
		return PromptSentUnverified
	default:
		return PromptCouldNotConfirm
	}
}

// SendKeysCommand sends text to the tmux pane using the reliable command path.
// Legacy callers keep the error-only contract while status-aware callers use
// SendKeysCommandObserved.
func (t *TmuxSession) SendKeysCommand(text string) error {
	_, err := t.SendKeysCommandObserved(text)
	return err
}

// SendKeysCommandObserved sends a prompt exactly like SendKeysCommand and also
// returns the terminal observation already made by the bounded submit path.
// It does not add another capture or wait to a delivery that confirmed or ended
// ambiguous: callers receive the status the submit path observed. The one
// exception is the observed-ABSENT outcome, which is redelivered once (#3293)
// before the final observation is reported.
func (t *TmuxSession) SendKeysCommandObserved(text string) (PromptDeliveryStatus, error) {
	t.inputMu.Lock()
	defer t.inputMu.Unlock()
	status, retryAuthorized, err := t.sendKeysPasteBuffer(text)
	if err != nil || status != PromptNotDelivered || !retryAuthorized {
		return status, err
	}

	// One automatic redelivery, only for the observed-ABSENT outcome (#3293).
	//
	// PromptNotDelivered is the single status this path may retry, because it is
	// the only outcome the observation machinery can honestly prove: the final
	// capture rendered THIS payload's prefix and never its completion tail, so
	// the paste did not land whole (#1982). Both unconfirmed outcomes stay final.
	// Sent-unverified means tmux accepted the paste and Enter while a readable
	// pane rendered no payload-specific proof either way (#3170), and
	// could-not-confirm means observation itself was unavailable — in both, the
	// first prompt may have SUBMITTED, and a redelivery would hand the agent the
	// same instruction twice. sendKeysPasteBuffer pairs PromptNotDelivered
	// exclusively with a nil error (every error path reports could-not-confirm),
	// and retryAuthorized additionally requires the post-Enter boundary frame to
	// still lack this payload's completion tail — see the authorization comment
	// in sendKeysPasteBuffer for why absence must hold at the submit itself, not
	// only at the observation deadline before it.
	//
	// The redelivery is the SAME full clear-observe submit, never a bare
	// re-paste: sendKeysPasteBuffer's unconditional pre-paste clear removes
	// whatever the failed attempt stranded in the composer, so the retry
	// replaces the strand instead of fusing with it into
	// STRANDED-DRAFTNEW-PROMPT (#2070). That clear is the entire reason an
	// automatic retry is safe by construction here.
	//
	// Exactly one retry, after a seconds-scale wait. Observed-absent clusters on
	// a pane that was busy mid-render, and an immediate retry re-enters the very
	// render that stranded the first paste. A pane that strands twice is a
	// persistent condition the caller must hear about: the second attempt's
	// observation is reported as-is and nothing loops.
	//
	// inputMu IS held across the wait, on purpose: the strand and its
	// replacement must stay adjacent. Released, a concurrent same-session
	// submission could run inside the gap — its unconditional clear consumes
	// this attempt's strand, and if that delivery then strands too, THIS retry's
	// clear would destroy a newer prompt whose Enter may still be pending, and
	// the older instruction would submit after the newer one. Both violate the
	// serialization inputMu exists to provide (#2178/#2181): the two attempts
	// are one submission transaction, not two. The cost is that the handlers
	// sharing this lock (trust prompt, codex safety) wait out the seconds-scale
	// delay — only on the rare stranded path, and strictly shorter than the
	// stall a wedged tmux server can already impose here.
	log.WarningLog.Printf("submit: redelivering prompt to session %q once in %s; delivery was observed absent through the submit boundary and the pre-paste clear makes redelivery safe (#3293)",
		t.sanitizedName, redeliverAfterAbsentDelay)
	time.Sleep(redeliverAfterAbsentDelay)
	status, _, err = t.sendKeysPasteBuffer(text)
	return status, err
}

// SendPromptWorstCaseBound returns an upper bound on one SendKeysCommandObserved
// call: two full submit attempts plus the redelivery wait (#3293). It exists for
// transport callers whose own deadline must OUTLIVE the submit — the remote
// send-prompt route budget — because a transport that gives up mid-retry leaves
// the in-sandbox submit running and possibly delivering, inviting the caller to
// re-send an already-delivered instruction (the AgentArchiveCallTimeout lesson,
// #2923).
//
// The bound is computed here, next to the submit path it describes, from the
// same knobs that bound the path itself. An attempt is counted as its
// individually bounded tmux commands plus the delivery observation window; the
// command count is deliberately GENEROUS (the longest success path issues 8:
// load, pre-clear capture, two cursor reads, clear, post-clear capture, paste,
// Enter+boundary) so a future added capture does not silently outgrow the bound.
func SendPromptWorstCaseBound() time.Duration {
	const boundedCommandsPerAttempt = 10
	attempt := boundedCommandsPerAttempt*tmuxCommandTimeout + pasteDeliveryMaxWait
	return 2*attempt + redeliverAfterAbsentDelay
}
