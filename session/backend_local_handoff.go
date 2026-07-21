package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/preflight"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// PrepareAgentSwap freezes and validates the exact first-launch command before
// handoff tears down the outgoing pane. Configuration is resolved once here;
// SwapAgent consumes the returned plan and cannot drift to a different override
// in the destructive close/start gap.
func (b *LocalBackend) PrepareAgentSwap(i *Instance, target string) (AgentSwapPlan, error) {
	resolved := resolveProgramForAgent(i, target)
	program := injectSystemPrompt(resolved)
	program, conversation := planLaunchConversation(i.ID, program)
	workDir := i.GetWorktreePath()
	if workDir == "" {
		return AgentSwapPlan{}, fmt.Errorf("handoff target %s has no worktree path for launch preflight", target)
	}
	if _, err := preflight.CheckCommandAt(program, workDir); err != nil {
		return AgentSwapPlan{}, fmt.Errorf("handoff target %s failed launch preflight: %w", target,
			preflight.ProgramError(target, resolved, err))
	}
	var capture ConversationCaptureSnapshot
	if tmux.DetectAgentFromCommand(program) == tmux.ProgramCodex {
		codexHome, err := tmux.CodexHomeFromCommand(program, workDir)
		if err != nil {
			return AgentSwapPlan{}, fmt.Errorf("handoff target %s has an unresolvable Codex conversation store: %w", target, err)
		}
		capture = BeginConversationCaptureAtCodexHome(codexHome)
	}
	return AgentSwapPlan{
		target: target, program: program, conversation: conversation, conversationCapture: capture,
	}, nil
}
