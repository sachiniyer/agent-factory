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
	status, err := t.deliverPromptOnce(text)
	if err != nil || status != PromptNotDelivered {
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
	// so the gate above is exact.
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
	// inputMu is deliberately NOT held across the wait. Each attempt is
	// self-contained — the clear makes it idempotent — and holding the input
	// lock for seconds would starve the handlers that serialize on it (trust
	// prompt, codex safety). A delivery that interleaves during the wait is
	// harmless for the same reason the retry is: its own pre-paste clear removes
	// this attempt's strand before pasting.
	log.WarningLog.Printf("submit: redelivering prompt to session %q once in %s; delivery was observed absent and the pre-paste clear makes redelivery safe (#3293)",
		t.sanitizedName, redeliverAfterAbsentDelay)
	time.Sleep(redeliverAfterAbsentDelay)
	return t.deliverPromptOnce(text)
}

// deliverPromptOnce is one full submit attempt under the input lock. Named
// without a *Locked suffix on purpose: it TAKES inputMu rather than expecting
// the caller to hold it.
func (t *TmuxSession) deliverPromptOnce(text string) (PromptDeliveryStatus, error) {
	t.inputMu.Lock()
	defer t.inputMu.Unlock()
	return t.sendKeysPasteBuffer(text)
}
