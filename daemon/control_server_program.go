package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// validateCreateProgram returns an error when program's executable does not run
// a recognized agent, so controlServer.createSession (the net/rpc and HTTP
// /v1/CreateSession entry point) and DeliverPrompt's absent-session auto-create
// branch reject it BEFORE the create proceeds. It is the daemon-side gate for
// the program-setting surfaces a raw RPC caller (token-holding automation over
// the listener, any local process over the unix socket) can reach WITHOUT the
// enum validation every shipped client applies upstream (CLI, TUI, task runner,
// config loader). An empty program is the no-explicit-program path and is
// allowed here (Manager.CreateSession defaults it downstream to the repo's
// resolved program).
//
// The check is tmux.DetectAgentExecutable — which resolves the executable token
// (after leading VAR=val assignments and "env …" invocations), not any token in
// the command. A program that runs no known agent would otherwise flow into
// provisioning where sandboxWorkspace.agentName() returns "" (its Claude
// fallback fires only for an EMPTY program) and the empty agent strips every
// per-agent credential from the launch environment (the deliberate fail-closed
// enshrined by TestSandboxCredentialSelectionRejectsAgentNameUsedAsData), so the
// agent fails to become ready and the request blocks for the full 60s readiness
// timeout with an embedded pane snippet instead of failing fast here.
//
// DetectAgentExecutable, not the bare-enum check config.ValidateProgramEnum
// every CLIENT surface uses, because a program may be a fully-resolved command
// string that runs a known agent (e.g. "/opt/claude --model opus" or
// "CLAUDE_CONFIG_DIR=… claude") rather than a bare enum name; the bare-enum
// check would reject those. Using the executable-only check (not
// DetectAgentFromCommand's any-token scan) ensures that a compound command like
// "./collect codex" — whose executable runs no known agent — is also rejected,
// preventing the same timeout bug via an appended agent-name argument.
// (The stricter sessionenv.AgentForCommand check still runs downstream on the
// sandbox provisioning path, so the fail-closed security property holds for any
// program this accepts.)
//
// This gate is NOT in Manager.CreateSession, on purpose: the daemon's own
// root-agent ensure loop (rootagent_create.go) calls Manager.CreateSession
// DIRECTLY with programs a raw RPC caller cannot — including root_agents.program
// entries that are custom commands running NO known agent (e.g. "/opt/bare-root",
// exercised by TestEnsureRootAgentsCreatesRootAtBareCloneWorktree). Validating in
// Manager.CreateSession would reject those and break the root-agent ensure flow.
// For DeliverPrompt, this gate is applied only on the absent-session auto-create
// branch (delivery.go), not unconditionally — so delivering to an existing
// session whose stored program is not a recognized agent (e.g. a root session
// running "/opt/bare-root") is not blocked.
func validateCreateProgram(program string) error {
	if program != "" && tmux.DetectAgentExecutable(program) == "" {
		return fmt.Errorf(
			"program %q does not run a recognized agent and must resolve to one of the known agents [%s]; "+
				"set program to one of these agent names, or — to run a custom path or flags — set it to a known agent name and move the full command into program_overrides",
			program, tmux.SupportedProgramsString())
	}
	return nil
}
