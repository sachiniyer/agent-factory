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
	// Use resolveSkillTargetForAccount with an empty account name rather than
	// resolveSkillTarget(i, resolved): at preflight time i.Account still holds
	// the auto-selected account (ClearAutoSelectedAccount runs later, after
	// PrepareAgentSwap succeeds). SwapAgent unconditionally rejects any non-empty
	// i.Account, so every launch built from an AgentSwapPlan runs with no account.
	// Stating that identity here — ambient, unscoped — means the af skill lands at
	// the unscoped root the incoming pane will actually read, rather than in the
	// outgoing auto-account's directory, which the incoming pane never opens.
	program := injectSystemPrompt(resolved, resolveSkillTargetForAccount(resolved, target, ""))
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
		launch, err := tmux.CommandEnvironmentFromCommand(program, workDir)
		if err != nil {
			return AgentSwapPlan{}, fmt.Errorf("handoff target %s has an unresolvable Codex launch environment: %w", target, err)
		}
		codexHome, err := tmux.CodexHomeFromCommand(program, workDir)
		if err != nil {
			return AgentSwapPlan{}, fmt.Errorf("handoff target %s has an unresolvable Codex conversation store: %w", target, err)
		}
		captureWorkingDir := ""
		if launch.WorkingDirKnown() {
			captureWorkingDir = launch.WorkingDir
		}
		capture = beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, captureWorkingDir)
	}
	return AgentSwapPlan{
		target: target, baseProgram: resolved, program: program, conversation: conversation, conversationCapture: capture,
	}, nil
}
