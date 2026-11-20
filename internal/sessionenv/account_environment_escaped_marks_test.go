package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// account_environment_escaped_marks_test.go pins #7627's fix: a denied or
// hidden mark under one of master's modelled launchers is computed on the
// operand's exact spelling OR its escape- and quote-normalized spelling,
// never on the normalized one alone. Every refuse row below ADMITS on
// master — the escape or quote byte hid the protected variable, the hidden
// option, or the agent name from the raw-text cut — so this test fails on
// master. The admit rows pin the neighbours: the same mark must not fire
// on a different agent's variable or on text the normalized reading does
// not expose.

// TestAccountEnvironmentEscapedMarkRefuses covers the command lines whose
// protected mark a downstream decode restores: strace's own issue shape
// (a prefix path the decoded-literal arm never reached), env's operand
// scan (the final review's R5), the sudo/docker/bwrap/podman/nerdctl
// lexical cuts, bwrap's --setenv/--unsetenv and --clearenv marks, the
// env-file/arg-file hidden-input spellings, and the reset reach's agent
// name. refuseClaude marks the rows the claude account's gate refuses too
// — a name protected only for one agent still refuses under the other
// gate only when that gate's own denied set names it.
func TestAccountEnvironmentEscapedMarkRefuses(t *testing.T) {
	claude := corpusClaudeAccount
	for _, test := range []struct {
		command      string
		refuseCodex  bool
		refuseClaude bool
	}{
		// #7627's own strace shape: under a command word the strace
		// prefix path does not run, and the tail's escaped -E operand
		// admitted on master.
		{"echo strace -ECODEX\\_HOME=/other codex", true, false},
		{"x strace -ECODEX\\_HOME=/o prog", true, false},

		// The independent review's R1: an escaped dash spells the option
		// strace's raw-byte reader could not see — `\-…` was taken as the
		// traced command. An option word is recognized on the exact OR
		// normalized spelling, and a word whose normalized text begins
		// with `-` is never the traced command.
		{"echo strace \\-ECODEX_HOME=/other codex", true, false},
		{"echo strace \\-E CODEX_HOME=/other codex", true, false},
		{"echo strace \\--env CODEX_HOME=/other codex", true, false},
		{"echo strace \\-fECODEX_HOME=/o codex", true, false},
		{"echo strace -f \\-ECODEX_HOME=/o codex", true, false},
		{"echo strace '-f' \\-ECODEX_HOME=/o codex", true, false},
		{"firejail strace \\-ECODEX_HOME=/o codex", true, false},
		{"prog strace \\-ECODEX_HOME=/o codex", true, false},
		{"make strace \\-ECODEX_HOME=/o codex", true, false},
		{"ssh host strace \\-ECODEX_HOME=/o codex", true, false},
		{"watch strace \\-ECODEX_HOME=/o codex", true, false},

		// A literal quote byte inside the option word is the same hidden
		// mark: the normalized spelling shows the env option the raw one
		// cannot.
		{"echo strace \\'-E CODEX_HOME=/o codex", true, false},
		{"echo strace \\'--env CODEX_HOME=/o codex", true, false},

		// The final review's R5: env's operands never got the escaped
		// reading at all.
		{"env 'CODEX\\_HOME=/o' prog", true, false},
		{"env -u 'CODEX\\_HOME' prog", true, false},
		{"env --unset 'CODEX\\_HOME' prog", true, false},
		{"nohup env 'CODEX\\_HOME=/o' prog", true, false},
		{"doas env 'CODEX\\_HOME=/o' prog", true, false},
		{"env 'CO\\DEX_HOME=/o' prog", true, false},
		{"env 'C\\ODEX_HOME=/o' prog", true, false},
		{"env 'CODEX_HOM\\E=/o' prog", true, false},
		{"env 'CODEX_HOME\\==/o' prog", true, false},
		{"env \"CODEX_H'OME=/o\" prog", true, false},
		{"env 'CODEX\\\\\\_HOME=/o' prog", true, false},

		// The env-injection launchers' lexical cut.
		{"sudo 'CODEX\\_HOME=/o' prog", true, false},
		{"sudo -E 'CODEX\\_HOME=/o' prog", true, false},
		{"sudo --env 'CODEX\\_HOME=/o' prog", true, false},
		{"sudo --env='CODEX\\_HOME=/o' prog", true, false},
		{"sudo -u root 'CODEX\\_HOME=/o' prog", true, false},
		{"sudo 'CODEX\\\\_HOME=/o' prog", true, false},
		{"docker run -e 'CODEX\\_HOME=/o' img prog", true, false},
		{"docker run --env 'CODEX\\_HOME=/o' img prog", true, false},
		{"docker run --env='CODEX\\_HOME=/o' img prog", true, false},
		{"docker run --env 'OPENAI\\_API_KEY=x' img prog", true, false},
		{"podman run -e 'CODEX\\_HOME=/o' img prog", true, false},
		{"nerdctl run -e 'CODEX\\_HOME=/o' img prog", true, false},
		{"bwrap --setenv 'CODEX\\_HOME' /o prog", true, false},
		{"bwrap --unsetenv 'CODEX\\_HOME' prog", true, false},
		{"env 'OPENAI\\_API_KEY=x' prog", true, false},

		// Shell-startup names sit in the same denied set, under both
		// agents' gates.
		{"sudo 'BASH\\_ENV=/o' prog", true, true},
		{"env 'BASH\\_ENV=/x' prog", true, true},
		{"sudo 'PS\\1=x' prog", true, true},
		{"sudo 'ZDOTDI\\R=/x' prog", true, true},
		{"sudo 'EN\\V=/x' prog", true, true},
		{"env 'PROMPT_COMMAN\\D=x' prog", true, true},

		// The hidden-input spellings are hidden marks too: an escape or
		// quote inside the option text must not spell it unread.
		{"docker run '--env-\\file=x' img prog", true, true},
		{"bwrap '--ar\\gs' 3 prog", true, true},
		{`bwrap "--clea'renv" codex`, true, false},

		// The reset reach's agent name is a mark the same way — an
		// escaped or quoted agent basename still reaches the agent.
		{`sudo "co'dex'"`, true, false},
		{"sudo 'co\\dex'", true, false},
		{"sudo -u root 'co\\dex'", true, false},
		{"sudo 'x/co\\dex'", true, false},
		{`su - "co'dex'"`, true, false},
		{"sudo 'cla\\ude'", false, true},
		{"sudo 'ANTHROPIC\\_API_KEY=/o' prog", false, true},
		{"sudo 'ANTHROPIC\\_AUTH_TOKEN=x' prog", false, true},

		// The neighbours stay admitted: a normalized word that does not
		// open with a dash is still the traced command, and an escaped
		// dash that spells a recognized non-env option (or the option
		// terminator, or an env option whose operand names nothing) is
		// the same word the shell was always going to pass.
		{"echo strace x\\-E codex", false, false},
		{"echo strace \\-o /tmp/log codex", false, false},
		{"echo strace \\-p 1234 codex", false, false},
		{"echo strace \\-- codex", false, false},
		{"echo strace \\'-E FOO=1 codex", false, false},
	} {
		codexErr := ValidateAccountEnvironmentCommand(test.command, scopedProcessTabAccount())
		require.Equal(t, test.refuseCodex, codexErr != nil,
			"command %q codex-gate verdict differs from its pinned verdict", test.command)
		claudeErr := ValidateAccountEnvironmentCommand(test.command, claude)
		require.Equal(t, test.refuseClaude, claudeErr != nil,
			"command %q claude-gate verdict differs from its pinned verdict", test.command)
	}
}

// TestAccountEnvironmentExactMarkStillRefuses pins the union's exact half:
// these rows refuse on master BECAUSE the exact spelling already shows
// the boundary or the name — a normalized-only computation would lose the
// mark (a backslash that formed the boundary is deleted, so x\CODEX_HOME
// reads xCODEX_HOME and stops matching). They are the re-review's six R1
// rows plus the strace shapes master's decoded arms already caught; the
// fix must keep refusing every one.
func TestAccountEnvironmentExactMarkStillRefuses(t *testing.T) {
	for _, command := range []string{
		// The re-review's R1 rows — the quote or backslash supplies the
		// identifier boundary the normalized text no longer has.
		"sudo 'x\\CODEX_HOME=/o' prog",
		`sudo "x'CODEX_HOME=/o" prog`,
		"sudo 'CODEX_HOME\\x=/o' prog",
		"x sudo 'x\\CODEX_HOME=/o' prog",
		"docker run -e 'a\\CODEX_HOME=/o' img prog",
		"bwrap --setenv 'x\\CODEX_HOME' /o prog",

		// Strace forms master's decoded arms already refused.
		"strace -ECODEX\\_HOME=/other codex",
		"strace --env=CODEX\\_HOME=/other codex",
		"strace -E 'CODEX\\_HOME=/other' codex",
		"x strace '--e\\nv' CODEX_HOME=/o codex",
		"x strace '-\\E' CODEX_HOME=/o codex",
		"x strace -E CODEX\\_HOME=/o codex",

		// Exact boundary or exact reach on master, kept.
		"sudo /o/co\\dex",
		"sudo 'ENV=/x' prog",
		"systemd-run '--setenv=CODEX\\_HOME=/o' prog",
		"systemd-run --setenv 'CODEX\\_HOME' /o prog",
		"run0 --setenv 'CODEX\\_HOME' /o prog",
		"systemctl --user set-environment 'CODEX\\_HOME=/o'",
		"systemctl --user unset-environment 'CODEX\\_HOME'",
		"dbus-update-activation-environment 'CODEX\\_HOME=/o'",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q must stay refused — the mark is exact OR normalized, never normalized alone", command)
	}
}

// TestAccountEnvironmentPositiveDecisionsStayExact pins the property's
// other half: every positive decision — dispatch identity, the command
// word, credential and grant classification — is computed on the exact
// spelling only. A quoted escape does not dispatch the agent (master's
// own behaviour: credentialAgentForCommand sees no codex in 'co\dex'),
// while the unquoted spelling the shell itself decodes still does.
func TestAccountEnvironmentPositiveDecisionsStayExact(t *testing.T) {
	for _, test := range []struct {
		command string
		cred    string
		grant   string
	}{
		{"'co\\dex' -m x", "", ""},
		{"env 'co\\dex'", "", ""},
		{"sudo 'co\\dex'", "", ""},
		{"xargs 'co\\dex'", "", ""},
		{"'ge\\mini'", "", ""},
		{"co\\dex", "codex", "codex"},
		{"claud\\e", "claude", "claude"},
		{"codex resume 'co\\dex'", "codex", "codex"},
	} {
		require.Equal(t, test.cred, credentialAgentForCommand(test.command),
			"command %q credential classification differs", test.command)
		require.Equal(t, test.grant, GrantAgentForRemoteCommand(test.command, "/w"),
			"command %q grant classification differs", test.command)
	}
}
