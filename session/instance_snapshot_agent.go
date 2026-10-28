package session

import (
	"strings"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// ReconcileAgentRuntimeSnapshot mirrors the daemon-owned current agent onto an
// existing client projection. A handoff keeps the Instance pointer and tmux
// address stable while replacing the process behind them, so Program, the
// tmux-side command metadata, and the live conversation must move together.
// Otherwise CurrentAgentName can keep returning the outgoing agent and the next
// account picker queries the wrong registry until the client restarts.
func (i *Instance) ReconcileAgentRuntimeSnapshot(data InstanceData) bool {
	var conversation AgentConversationData
	if len(data.Tabs) > 0 && data.Tabs[0].Conversation != nil {
		conversation = *data.Tabs[0].Conversation
	} else if data.AgentConversation != nil {
		conversation = *data.AgentConversation
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	changed := false
	runtimeChanged := false
	programChanged := i.Program != data.Program
	observedAgent := i.currentAgentNameLocked()
	if programChanged {
		i.Program = data.Program
		changed = true
		runtimeChanged = true
	}
	currentAgent := strings.TrimSpace(data.CurrentAgent)
	identityChanged := tmux.IsSupportedProgram(currentAgent) && observedAgent != currentAgent
	if programChanged || identityChanged {
		paneProgram := data.Program
		// CurrentAgent is the daemon's resolved runtime identity. Preserve the
		// exact configured command when it identifies that agent; otherwise use
		// the enum as projection metadata so an opaque wrapper cannot leave this
		// client believing the outgoing agent is still running.
		if identityChanged && tmux.DetectAgentFromCommand(paneProgram) != currentAgent {
			paneProgram = currentAgent
		}
		if agentTmux := i.tmuxLocked(); agentTmux != nil && agentTmux.Program() != paneProgram {
			agentTmux.SetProgram(paneProgram)
			changed = true
			runtimeChanged = true
		}
	}
	if len(i.Tabs) > 0 && i.Tabs[0].Conversation != conversation {
		i.Tabs[0].Conversation = conversation
		changed = true
	}
	if !changed {
		return false
	}
	if runtimeChanged {
		// Any asynchronous conversation capture belongs to the process described
		// by the prior snapshot and must not write into this replacement's slot.
		i.agentRuntimeGeneration++
	}
	i.touchLocked()
	return true
}
