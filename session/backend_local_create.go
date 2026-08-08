package session

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func (b *LocalBackend) prepareCreateLaunch(i *Instance) (CreateLaunchPlan, error) {
	workDir := i.GetWorktreePath()
	if workDir == "" {
		return CreateLaunchPlan{}, fmt.Errorf("cannot prepare session %q: local provisioning produced no launch directory", i.Title)
	}

	resolution := resolveLaunchProgramForInstance(i)
	resolved := resolution.command
	program, conversation := planCreateConversation(i, resolved)
	program = injectSystemPrompt(program)
	proof := accountLaunchProof(resolved, program, resolution.trustBase)
	if account := strings.TrimSpace(i.Account); account != "" {
		accountAgent := sessionenv.AgentForCommand(i.AgentProgram())
		if err := refuseAccountAgentDrift(account, accountAgent, program); err != nil {
			return CreateLaunchPlan{}, fmt.Errorf("cannot create account-scoped session: %w", err)
		}
		err := sessionenv.ValidateAccountCommand(program, sessionenv.Account{
			Agent:             accountAgent,
			Name:              account,
			TrustedExecutable: proof.TrustedExecutable,
			GeneratedArgs:     proof.GeneratedArgs,
		})
		if err != nil {
			return CreateLaunchPlan{}, fmt.Errorf("cannot create account-scoped session: %w", err)
		}
	}
	plan := CreateLaunchPlan{
		program:      program,
		accountProof: proof,
		workDir:      workDir,
		conversation: conversation,
	}

	// Key capture off the executable the pane will actually run, never the
	// configured enum. An override may point "codex" at an opaque wrapper; its
	// private environment and storage contract are unknowable here, so polling a
	// daemon-inherited Codex home would manufacture a path we have no evidence the
	// child uses (#1116/#2290). Keep parser failure distinct from a positively
	// opaque command, though: DetectAgentFromCommand deliberately collapses both
	// to "", while a configured Codex command with unsupported env syntax must
	// still fail closed instead of silently losing capture.
	actualAgent := tmux.DetectAgentFromCommand(program)
	if actualAgent != tmux.ProgramCodex {
		if i.AgentProgram() != tmux.ProgramCodex {
			return plan, nil
		}
		launch, err := tmux.CommandEnvironmentFromCommand(program, workDir)
		if err != nil {
			return CreateLaunchPlan{}, fmt.Errorf(
				"cannot prepare Codex conversation capture for session %q: %w; use literal CODEX_HOME/HOME values and supported env options",
				i.Title, err)
		}
		if launch.Agent != tmux.ProgramCodex {
			return plan, nil
		}
	}
	codexHome, err := tmux.CodexHomeFromCommand(program, workDir)
	if err != nil {
		return CreateLaunchPlan{}, fmt.Errorf(
			"cannot prepare Codex conversation capture for session %q: %w; use literal CODEX_HOME/HOME values and supported env options",
			i.Title, err)
	}
	if strings.TrimSpace(codexHome) == "" {
		return CreateLaunchPlan{}, fmt.Errorf("cannot prepare Codex conversation capture for session %q: resolved store is empty", i.Title)
	}
	plan.conversationCapture = BeginConversationCaptureAtCodexHome(codexHome)
	return plan, nil
}

func (b *LocalBackend) launchPreparedCreate(i *Instance, plan CreateLaunchPlan) error {
	return b.launch(i, true, &plan)
}
