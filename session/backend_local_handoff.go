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
	resolved, err := resolveProgramForAgent(i, target)
	if err != nil {
		return AgentSwapPlan{}, fmt.Errorf("handoff target %s: %w", target, err)
	}
	return prepareAgentSwapFrom(i, target, resolved)
}

// prepareAgentSwapFrom is PrepareAgentSwap for an already-resolved command: an
// account swap restarts the command that was proven and is running, not whatever
// its agent name resolves to under today's overrides.
func prepareAgentSwapFrom(i *Instance, target, resolved string) (AgentSwapPlan, error) {
	// A plain handoff never carries the outgoing scope: a scoped session can
	// only reach this preflight when the target cannot hold an account —
	// scoped-to-scopable without --account is refused at admission — so the
	// record will clear i.Account before this command launches. Resolving the
	// skill against the still-recorded outgoing account would mark a
	// non-scopable target unresolved and drop the ambient af skill the
	// replacement pane should read (#4430 review round 7).
	workDir := i.GetWorktreePath()
	env, err := launchEnvFor(i, workDir)
	if err != nil {
		return AgentSwapPlan{}, fmt.Errorf("handoff target %s: %w", target, err)
	}
	program := injectSystemPromptWithEnv(resolved, resolveSkillTargetForAccount(resolved, "", "", workDir), i.guidanceCaps(), env)
	// A swap is never the instance's first launch. The instance id may already
	// name a transcript: a claude → codex → claude handoff's first Claude
	// conversation was started on it, and Claude refuses to start a new
	// conversation on an id already in use (#7211). Mint one, as the account
	// swap's fresh start does; SwapAgent records it before the spawn.
	program, conversation := planLaunchConversation(newSessionID(), program)
	if workDir == "" {
		return AgentSwapPlan{}, fmt.Errorf("handoff target %s has no worktree path for launch preflight", target)
	}
	if _, err := preflight.CheckCommandAt(program, workDir); err != nil {
		return AgentSwapPlan{}, fmt.Errorf("handoff target %s failed launch preflight: %w", target,
			swapPreflightCause(i.Title, workDir, target, resolved, err))
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
		if err := capture.ScopeErr(); err != nil {
			return AgentSwapPlan{}, worktreeLookupError(i.Title, workDir, err)
		}
	}
	return AgentSwapPlan{
		target: target, baseProgram: resolved, program: program, conversation: conversation, conversationCapture: capture,
		passthrough: env.passthrough, passthroughFrozen: true,
	}, nil
}
