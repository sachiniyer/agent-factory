package agentaccount

import (
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// codexUpdateCheckOperand is the -c operand af itself writes into every codex
// launch (tmux.EnsureCodexUpdateCheckDisabled): the startup-update picker's
// config key forced off. It is the ONE config override af can prove is on the
// command line — every other -c operand is operator-authored and could carry
// sandbox_mode, a profile selector, or a profile table entry.
const codexUpdateCheckOperand = "check_for_update_on_startup=false"

// codexProgramAgentSubcommands are the subcommand names af itself can record
// in a launch program — resume from the restore/carry splices, exec and fork
// from supported program_overrides shapes. They open the runtime grammar the
// positional bound below models.
var codexProgramAgentSubcommands = map[string]bool{
	"resume": true, "fork": true, "exec": true, "e": true,
}

// codexProgramOtherSubcommands are recognized codex subcommands whose argv
// grammar af does not model — a flag after one is not provably a codex flag,
// so the program answers "yes, it could change the mode or profile" and the
// caller must compare rather than trust a config mode.
var codexProgramOtherSubcommands = map[string]bool{
	"review": true, "login": true, "logout": true, "mcp": true,
	"plugin": true, "app-server": true, "remote-control": true,
	"app": true, "completion": true, "update": true, "doctor": true,
	"sandbox": true, "debug": true, "execpolicy": true,
	"apply": true, "a": true, "cloud": true, "cloud-tasks": true,
	"agents": true, "queue": true, "archive": true, "delete": true,
	"unarchive": true, "migrate-rollouts": true, "migrate-nfs-db": true,
	"features": true, "responses-api-proxy": true, "stdio-to-uds": true,
	"exec-server": true, "tcp-tunnel": true,
}

// codexProgramSandboxNeutral classifies — never interprets — the session's
// recorded launch program for the drift compare's skip question (#6902;
// #6919 review). It answers the ONLY question the program can settle: could
// the launch command change the sandbox mode or the selected profile?
//
// true: the program reduces to a single literal codex argv (the sessionenv
// reduction — assignments, an exec prefix, one trusted env wrapper) whose
// every word is either an argument af itself writes or a positional codex
// never reads as an option. Nothing on the command line can affect the
// sandbox story, so the homes' configs alone decide it.
//
// false: anything else. Another -c operand in any spelling, a mode or
// profile flag, an option af does not model, a subcommand af does not model,
// a wrapper, an unparseable program. Every one could carry a mode or profile
// change, so the compare must not skip sandbox_workspace_write. A
// classification mistake can only over-compare a table — drift or equality
// on an inert key — never skip a live one: fail closed by construction,
// with no codex precedence semantics to reimplement.
func codexProgramSandboxNeutral(program string) bool {
	argv, ok := sessionenv.LiteralInvocationArgv(program)
	if !ok || len(argv) == 0 || !strings.EqualFold(filepath.Base(argv[0]), "codex") {
		return false
	}
	inSubcommand := false
	positionals := 0
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "--":
			// Option parsing ends; everything left is positional.
			positionals += len(argv) - i - 1
			i = len(argv)
		case arg == "-c":
			// af's update-check pair is the only -c af writes; its operand
			// must be the exact word af generates — byte-for-byte, so any
			// other override (sandbox_mode, a profile selector, whitespace
			// variants) answers "yes, it could change the mode".
			if i+1 >= len(argv) || argv[i+1] != codexUpdateCheckOperand {
				return false
			}
			i++
		case arg == "--last":
			// af's resumeProgram splice word; it selects a conversation, not
			// a mode or profile.
		case arg == "-" || !strings.HasPrefix(arg, "-"):
			// A positional word. Only the first positional can be a
			// subcommand name; codex matches it there even when options
			// precede it.
			if !inSubcommand && positionals == 0 {
				if codexProgramOtherSubcommands[arg] {
					return false
				}
				if codexProgramAgentSubcommands[arg] {
					inSubcommand = true
					continue
				}
			}
			positionals++
		default:
			// Any other option-shaped token — -s, --sandbox, --full-auto,
			// -p/--profile, --dangerously-*, an attached -c form, or a flag
			// af does not model — could set a mode or a profile.
			return false
		}
		if positionals > codexProgramMaxPositionals(inSubcommand) {
			return false
		}
	}
	return true
}

// codexProgramMaxPositionals bounds positional words af can prove belong to
// the runtime grammar: a bare prompt at the root, or a session selector plus
// prompt inside a modeled subcommand. More than that and a word af passed as
// positional could be something the real parser rejected — unprovable.
func codexProgramMaxPositionals(inSubcommand bool) int {
	if inSubcommand {
		return 2
	}
	return 1
}
