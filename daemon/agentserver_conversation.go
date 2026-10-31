package daemon

import (
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Conversation identity for the in-sandbox agent-server (#5571 W7.2): the server
// runs the local runtime, so it is the process that injects a Claude session id
// or captures a Codex rollout, and the launch/snapshot responses report it.

// agentServerCaptureTimeout bounds how long the server waits for Codex to write
// the rollout that names its conversation.
const agentServerCaptureTimeout = 30 * time.Second

func codexHomeFor(program, workDir string) string {
	if home, err := tmux.CodexHomeFromCommand(program, workDir); err == nil && home != "" {
		return home
	}
	return ""
}

// captureCodexConversation discovers the Codex conversation id in the
// background — the capture the daemon Manager runs for a local session — and
// records it on the instance, from which Snapshot reports it. Bound to the
// process generation it was started for, so a replaced agent cannot inherit it.
func (hs *headlessServer) captureCodexConversation(snap session.ConversationCaptureSnapshot) {
	token := hs.inst.AgentRuntimeToken()
	if token.Agent() != tmux.ProgramCodex {
		return
	}
	go func() {
		conv, err := session.CaptureAgentConversation(token.Agent(), snap, agentServerCaptureTimeout)
		if err != nil || !conv.HasID() {
			return
		}
		hs.inst.SetAgentConversationForRuntime(token, conv)
	}()
}
