package daemon

import (
	"time"

	"github.com/sachiniyer/agent-factory/log"
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
		// The same chokepoint as the daemon's loop: settle the evidence first
		// (re-verifying a kept identity, #7123), and say so when there is
		// none rather than waiting out the window in silence (#7124 review
		// F5) — a server restarted over a surviving tmux session rebuilds its
		// instance with no launch record at all.
		if err := hs.inst.CheckConversationCaptureEvidence(); err != nil {
			log.WarningLog.Printf("agent-server: conversation capture for %q recorded nothing: %v",
				hs.inst.Title, unrecordedCaptureError(hs.inst.Title, err))
			return
		}
		// Adoption is attributed to the launched runtime's own open files,
		// not merely to a new rollout naming the right directory (#6847).
		conv, err := session.CaptureAgentConversation(token.Agent(), hs.inst.BindConversationCaptureRuntime(snap), agentServerCaptureTimeout)
		if err != nil {
			log.WarningLog.Printf("agent-server: conversation capture for %q failed: %v", hs.inst.Title, err)
			return
		}
		if !conv.HasID() {
			return
		}
		hs.inst.SetAgentConversationForRuntime(token, conv)
	}()
}
