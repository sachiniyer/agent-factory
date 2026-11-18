package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// scopedProcessTabEnvNames is the denied-name set the production walk uses for a
// codex sibling process tab: the agent's credential names plus the config dir,
// with the shell-startup names overlaid exactly as the wrapper tests prepare it
// (TestCommandMutatesAccountEnvironment_UnmodeledWrapperAssignment /
// scopedProcessTabAppendNames).
func scopedProcessTabEnvNames() map[string]struct{} {
	codex := accountScopedNames("codex", "CODEX_HOME")
	for name := range accountShellStartupNames {
		codex[name] = struct{}{}
	}
	return codex
}

// sudo, docker and systemd-run run a child command AND apply a NAME=value
// operand to its environment: sudo's positional `NAME=value` (and CODEX_HOME+=
// /other), docker's `-e`/`--env` (separate word or attached, with `run`/`exec`/
// `create`) and systemd-run's `--setenv`. The wrapper tail scan judging
// `account_environment_command.go` judged a dash-prefixed option value whose
// left side names a denied variable (e.g. `--env=CODEX_HOME=/other`), but a
// plain-word positional `NAME=value` ahead of the child command fell to the
// early non-dash return and was treated as inert data, so `sudo
// CODEX_HOME=/other codex`, `docker run -e CODEX_HOME=/other busybox codex`
// and `systemd-run --setenv CODEX_HOME=/other codex` (the separate-word value
// spellings) were ADMITTED while the `--env=CODEX_HOME=/other` attached form and
// `env CODEX_HOME=/other codex` were REFUSED. The wrapper name is the only
// discriminator that does not also refuse documented noun-uses such as
// `rg OPENAI_API_KEY= .`, so sudo/docker/systemd-run are listed in
// isUnmodeledArgvPassthroughWrapper (and in accountChildTailSuffixVerdictNames
// so a buried form under a modeled wrapper's peeled tail is judged too).
func TestValidateAccountEnvironmentCommand_RefusesPositionalEnvInjectionAssignment(t *testing.T) {
	for _, command := range []string{
		// sudo: positional `NAME=value` ahead of the child (and every option the
		// bare-tail NAME= path missed).
		"sudo CODEX_HOME=/other codex",
		"sudo -n CODEX_HOME=/other codex",
		"sudo -u root CODEX_HOME=/other codex",
		"sudo -- CODEX_HOME=/other codex",
		// The `+=` append form mutates the same variable and routes through the
		// same wholesale refusal at the sudo head.
		"sudo CODEX_HOME+=/other codex",
		// sudo with a credential name (the same as the existing `env`
		// spelling the guard already refused).
		"sudo OPENAI_API_KEY=sk-stolen codex",
		// A shell-startup name runs before the agent reaches its exec shim.
		"sudo BASH_ENV=/tmp/evil codex",
		// Absolute-path wrapper is still a listed wrapper (matched by basename).
		"/usr/bin/sudo CODEX_HOME=/other codex",

		// docker: the value carries `-e`/`--env` as a separate word, the exact
		// shape the bare-tail NAME= path treated as inert. `run`, `exec`,
		// `create` and any subcommand that execs a child all land under the
		// wholesale refusal at the docker head.
		"docker run -e CODEX_HOME=/other busybox codex",
		"docker run --env CODEX_HOME=/other busybox codex",
		"docker run --env=CODEX_HOME=/other busybox codex",
		"docker exec -e CODEX_HOME=/other c1 codex",
		"docker create -e CODEX_HOME=/other busybox codex",

		// systemd-run: `--setenv` value as a separate word, the same separate-
		// word gap the bare-tail NAME= path did not see.
		"systemd-run --setenv CODEX_HOME=/other codex",
		"systemd-run --setenv=CODEX_HOME=/other codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q injects a denied NAME= assignment through an env-injection wrapper", command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable",
			"command %q must be refused by the account-environment guard", command)
	}
}

// The fix must close the same gap across every supported agent's credential
// names, config variable, and cloud-mode selectors — not just codex. Each
// command below injects an agent's identity variable through sudo/docker/
// systemd-run; each must be refused at the production gate so an agent-scoped
// sibling pane cannot have its selected account re-rooted.
func TestValidateAccountEnvironmentCommand_RefusesPositionalEnvInjectionAcrossAgents(t *testing.T) {
	cases := []struct {
		agent   string
		command string
	}{
		{"codex", "sudo CODEX_HOME=/other codex"},
		{"codex", "sudo OPENAI_API_KEY=sk codex"},
		{"codex", "docker run -e CODEX_HOME=/other busybox codex"},
		{"codex", "systemd-run --setenv CODEX_HOME=/other codex"},
		{"claude", "sudo CLAUDE_CONFIG_DIR=/other claude"},
		{"claude", "sudo ANTHROPIC_API_KEY=sk claude"},
		{"claude", "sudo CLAUDE_CODE_USE_BEDROCK=1 claude"},
		{"claude", "docker run -e ANTHROPIC_API_KEY=sk busybox claude"},
		{"claude", "systemd-run --setenv CLAUDE_CONFIG_DIR=/other claude"},
		{"gemini", "sudo GEMINI_CLI_HOME=/other gemini"},
		{"gemini", "sudo GEMINI_API_KEY=key gemini"},
		{"gemini", "sudo GOOGLE_GENAI_USE_VERTEXAI=1 gemini"},
		{"gemini", "docker run -e GEMINI_API_KEY=key busybox gemini"},
		{"gemini", "systemd-run --setenv GEMINI_CLI_HOME=/other gemini"},
	}
	for _, test := range cases {
		account := Account{Agent: test.agent, Name: "work", Dir: "/afhome/accounts/" + test.agent + "/work"}
		err := ValidateAccountEnvironmentCommand(test.command, account)
		require.Error(t, err, "agent %q: command %q injects an identity variable through an env-injection wrapper",
			test.agent, test.command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable",
			"agent %q: command %q must be refused by the account-environment guard", test.agent, test.command)
	}
}

// TestCommandMutatesAccountEnvironment_PositionalEnvInjection pins the boolean
// predicate directly so the signal — not only the end-to-end error — is locked
// against future drift, on the parts of the fix the table-driven wrapper-guard
// tests do not already cover: the buried-under-modeled-wrapper cases (which
// exercise the accountChildTailSuffixVerdictNames map update) and the
// "wholesale refusal is set by the wrapper HEAD" cases (which pin the
// discrimination rule — benign tails still refuse). Narrowness controls
// confirm the noun-use carve-out (only the wrapper name discriminates) against
// `rg OPENAI_API_KEY= .`, which keeps further words after the assignment-shaped
// word and must stay admitted.
func TestCommandMutatesAccountEnvironment_PositionalEnvInjection(t *testing.T) {
	names := scopedProcessTabEnvNames()
	cases := []struct {
		command string
		want    bool
	}{
		// Wholesale refusal is set by the wrapper HEAD, not by what follows:
		// a benign tail with no denied NAME= word still refuses, the same
		// rule the existing strace/perf/... parity test pins.
		{"sudo codex", true},
		{"sudo npm run dev", true},
		{"sudo --preserve-env=PATH codex", true},
		{"sudo -i", true},
		{"docker ps", true},
		{"docker build .", true},
		{"docker run busybox codex", true},
		{"systemd-run codex", true},
		{"systemd-run --unit=x codex", true},

		// Buried under a modeled wrapper: the buried-suffix scan judges the
		// buried sudo/docker/systemd-run at its own suffix (via the
		// accountChildTailSuffixVerdictNames map) and fails closed the same
		// way the head reading does. `nohup sudo …` is the report's explicit
		// buried case.
		{"nohup sudo CODEX_HOME=/other codex", true},
		{"nice sudo CODEX_HOME=/other codex", true},
		{"exec sudo CODEX_HOME=/other codex", true},
		{"command sudo CODEX_HOME=/other codex", true},
		// One level deeper: the buried verdict-name list catches sudo even
		// when an opaque leaf (echo) sits between the modelled wrapper and
		// the wrapper, because the operator's surrounding `echo` is inert
		// data and the shadowed sibling can still shift to sudo as the
		// command head.
		{"nohup echo sudo CODEX_HOME=/other codex", true},
		{"nohup echo docker run -e CODEX_HOME=/other busybox codex", true},
		{"nohup echo systemd-run --setenv CODEX_HOME=/other codex", true},

		// Narrowness — must stay ADMITTED. The fix lists sudo/docker/
		// systemd-run wholesale exactly so the noun-use carve-out does not
		// change: a NAME=value token under echo/rg/cat/grep is inert data,
		// NOT an exec-operand assignment, and a noun-use of the wrapper name
		// stays inert the same way.
		{"echo CODEX_HOME=/tmp", false},
		{"rg OPENAI_API_KEY= .", false},
		{"echo sudo CODEX_HOME=/other", false},
		{"echo docker run -e CODEX_HOME=/other busybox codex", false},
		{"man sudo", false},
		// An unlisted wrapper (firejail) running an ordinary command sets
		// nothing af can see — it is not in the wholesale refuse list.
		{"firejail npm run dev", false},
		{"firejail env PORT=3000 codex", false},
	}
	for _, test := range cases {
		got := commandMutatesAccountEnvironment(test.command, names)
		require.Equal(t, test.want, got, "command %q", test.command)
	}
}

// TestApplyAccountEnvironment_RefusesPositionalEnvInjection is the production
// exec-shim gate. Confirm a positional NAME= assignment through sudo/docker/
// systemd-run is refused before any scoped environment is produced, mirroring
// the existing TestApplyAccountEnvironment_RefusesUnmodeledWrapperHiddenAssignment
// that exercises the same entry point for the wrapper-hidden shape.
func TestApplyAccountEnvironment_RefusesPositionalEnvInjection(t *testing.T) {
	account := Account{Agent: "codex", Name: "work", Dir: "/afhome/accounts/codex/work"}
	for _, command := range []string{
		"sudo CODEX_HOME=/other codex",
		"sudo CODEX_HOME+=/other codex",
		"nohup sudo CODEX_HOME=/other codex",
		"docker run -e CODEX_HOME=/other busybox codex",
		"systemd-run --setenv CODEX_HOME=/other codex",
	} {
		_, err := ApplyAccountEnvironment(nil, command, account)
		require.Error(t, err, "command %q must not replace the sibling account environment", command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable",
			"command %q must be refused by the account-environment guard", command)
	}
}
