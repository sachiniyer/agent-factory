package session

import (
	"errors"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// keptCaptureEvidence is the launch identity a reattach probe could not answer
// about (#7123). Compile-only stub for the failing-first commit.
type keptCaptureEvidence struct {
	pid      int
	startID  uint64
	verified bool
}

// ErrNoConversationCaptureEvidence: compile-only stub for the failing-first
// commit.
var ErrNoConversationCaptureEvidence = errors.New("af cannot tell which open thread is the agent's own")

// CheckConversationCaptureEvidence: compile-only stub for the failing-first
// commit — master's behavior, with no re-verification.
func (i *Instance) CheckConversationCaptureEvidence() error {
	i.mu.RLock()
	override := i.codexRuntimeHeldOverride
	i.mu.RUnlock()
	if override != nil {
		return nil
	}
	_, err := i.codexVerifiedRuntimeRoot()
	return err
}

// KeepCaptureEvidenceForTest: compile-only stub for the failing-first commit.
func (i *Instance) KeepCaptureEvidenceForTest(p proctree.Process) {}
