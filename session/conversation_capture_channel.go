package session

import "errors"

// ErrCodexCaptureChannelUnavailable marks a capture failure that says nothing
// about which thread the runtime is on, only that a channel capture needs
// could not be read: the rollout store (a before-image that failed, a scan
// that failed inside the window) or the runtime's open files. A fresh
// account swap's synchronous capture defers to its post-delivery capture on
// it rather than failing a handoff whose replacement already launched
// (#7126); every other capture error — two held threads, a header that never
// became classifiable — is an answer that still fails it. A carried swap's
// confirmation keeps refusing an unreadable store regardless.
var ErrCodexCaptureChannelUnavailable = errors.New("a Codex conversation capture channel is unavailable")

// captureChannelError tags err as ErrCodexCaptureChannelUnavailable without
// changing its text, so a caller can tell the class apart while every log
// line reads exactly as before.
type captureChannelError struct{ err error }

func (e captureChannelError) Error() string { return e.err.Error() }

func (e captureChannelError) Unwrap() []error {
	return []error{ErrCodexCaptureChannelUnavailable, e.err}
}

func captureChannelUnavailable(err error) error {
	if err == nil {
		return nil
	}
	return captureChannelError{err: err}
}

// CarriesConversation reports whether this snapshot confirms a conversation an
// account swap carried to its replacement, rather than discovering a new one.
// A carried confirmation needs no attribution and refuses a store it cannot
// read; a fresh capture is what may defer (#7126).
func (s *ConversationCaptureSnapshot) CarriesConversation() bool {
	return s != nil && s.carried.HasID()
}

// ExpectCarriedCodexRolloutForTest marks snap as a carried swap's capture, the
// way an account swap's launch preparation does when it carries conv's rollout
// at path into the incoming store.
func (s *ConversationCaptureSnapshot) ExpectCarriedCodexRolloutForTest(conv AgentConversationData, path string) {
	s.expectCarriedCodexRollout(conv, path)
}
