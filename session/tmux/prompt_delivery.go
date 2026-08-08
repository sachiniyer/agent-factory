package tmux

// PromptDeliveryStatus is the closed set of outcomes a caller may learn from
// the pane observation performed during prompt submission. It reports only
// what the daemon observed; PromptCouldNotConfirm deliberately does not guess
// whether an unobservable paste ultimately landed.
type PromptDeliveryStatus string

const (
	PromptDelivered       PromptDeliveryStatus = "delivered"
	PromptNotDelivered    PromptDeliveryStatus = "not-delivered"
	PromptCouldNotConfirm PromptDeliveryStatus = "could-not-confirm"
)

// Valid reports whether the status is one of the three wire-supported
// outcomes. An empty or future value from an older/newer peer is not evidence
// of delivery and must be normalized to PromptCouldNotConfirm by callers.
func (s PromptDeliveryStatus) Valid() bool {
	return s == PromptDelivered || s == PromptNotDelivered || s == PromptCouldNotConfirm
}

func (o deliveryOutcome) promptDeliveryStatus() PromptDeliveryStatus {
	switch o {
	case deliveryObservedLanded:
		return PromptDelivered
	case deliveryObservedAbsent:
		return PromptNotDelivered
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
// It does not add another capture or wait: callers receive the status that was
// previously used only for logging.
func (t *TmuxSession) SendKeysCommandObserved(text string) (PromptDeliveryStatus, error) {
	t.inputMu.Lock()
	defer t.inputMu.Unlock()
	return t.sendKeysPasteBuffer(text)
}
