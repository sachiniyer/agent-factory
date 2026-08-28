package session

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestSendPromptCallBudgetCoversTheInSandboxRetry pins the remote send-prompt
// route budget to the submit path's own worst case (#3293). The headless
// handler does not observe the request context, so an outer call that expires
// while the in-sandbox submit is mid-retry leaves the sandbox delivering
// anyway — and the caller, holding a transport error, likely to re-send an
// already-delivered instruction. The budget must therefore outlive the whole
// two-attempt submit with transport slack to spare; deriving it from
// tmux.SendPromptWorstCaseBound keeps a retimed delivery path from silently
// outgrowing it, and this test keeps anyone from decoupling the two again.
func TestSendPromptCallBudgetCoversTheInSandboxRetry(t *testing.T) {
	bound := tmux.SendPromptWorstCaseBound()
	if slack := agentSendPromptCallTimeout - bound; slack < 10*time.Second {
		t.Fatalf("agentSendPromptCallTimeout (%s) must exceed the in-sandbox submit worst case (%s) with transport slack; got %s slack",
			agentSendPromptCallTimeout, bound, slack)
	}
}
