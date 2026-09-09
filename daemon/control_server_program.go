package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// validateCreateProgram returns an error when program runs no recognized agent,
// so controlServer.createSession (the net/rpc and HTTP /v1/CreateSession entry
// point) rejects it BEFORE the create proceeds. It is the daemon-side gate for the
// one program-setting surface a raw RPC caller (token-holding automation over the
// listener, any local process over the unix socket) can reach WITHOUT the enum
// validation every shipped client applies upstream (CLI, TUI, task runner, config
// loader). An empty program is the no-explicit-program path and is allowed here
// (Manager.CreateSession defaults it downstream to the repo's resolved program).
//
// The check is tmux.DetectAgentFromCommand — the codebase's canonical "which
// known agent does this program run" resolution (the one Instance.ResolvedAgent
// keys readiness, trust handling, and flag injection off). A program that
// resolves to no known agent would otherwise flow into provisioning where
// sandboxWorkspace.agentName() returns "" (its Claude fallback fires only for an
// EMPTY program) and the empty agent strips every per-agent credential from the
// launch environment (the deliberate fail-closed enshrined by
// TestSandboxCredentialSelectionRejectsAgentNameUsedAsData), so the agent fails
// to become ready and the request blocks for the full 60s readiness timeout with
// an embedded pane snippet instead of failing fast here.
//
// DetectAgentFromCommand, not the bare-enum check config.ValidateProgramEnum
// every CLIENT surface uses, because a program may be a fully-resolved command
// string that runs a known agent (e.g. "/opt/claude --model opus" or
// "CLAUDE_CONFIG_DIR=… claude") rather than a bare enum name; the bare-enum check
// would reject those, and so would the stricter sessionenv.AgentForCommand
// credential-grant check (which handles only the `env` keyword, not bare VAR=val
// prefixes). DetectAgentFromCommand accepts any program that runs a supported
// agent — bare name, full path, or assignment-prefixed invocation — while
// rejecting genuinely unrecognized ones. (The stricter AgentForCommand check
// still runs downstream on the sandbox provisioning path, so the fail-closed
// security property holds for any program this accepts.)
//
// This gate is on the RPC boundary (controlServer.createSession), NOT in
// Manager.CreateSession, on purpose: the daemon's own root-agent ensure loop
// (rootagent_create.go) and DeliverPrompt auto-create (delivery.go) call
// Manager.CreateSession DIRECTLY with programs a raw RPC caller cannot —
// including root_agents.program entries that are custom commands running NO
// known agent (e.g. "/opt/bare-root", exercised by
// TestEnsureRootAgentsCreatesRootAtBareCloneWorktree). Validating in
// Manager.CreateSession would reject those and break the root-agent ensure
// flow; validating at the RPC boundary rejects only external input and leaves
// the in-process callers' contract untouched. A bare unsupported program from a
// raw RPC caller (the bug: "my-custom-agent" -> 60s timeout) is rejected here;
// the same string handed to Manager.CreateSession by in-process code is not.
func validateCreateProgram(program string) error {
	if program != "" && tmux.DetectAgentFromCommand(program) == "" {
		return fmt.Errorf(
			"program %q does not run a recognized agent and must resolve to one of the known agents [%s]; "+
				"set program to one of these agent names, or — to run a custom path or flags — set it to a known agent name and move the full command into program_overrides",
			program, tmux.SupportedProgramsString())
	}
	return nil
}
