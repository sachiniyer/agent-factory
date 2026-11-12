package session

import "errors"

// ErrCodexCaptureChannelUnavailable: compile-only stub for the failing-first
// commit.
var ErrCodexCaptureChannelUnavailable = errors.New("a Codex conversation capture channel is unavailable")

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
