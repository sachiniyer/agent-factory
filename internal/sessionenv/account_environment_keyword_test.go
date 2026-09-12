package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func scopedProcessTabAccount() Account {
	return Account{Agent: "codex", Name: "work", Dir: "/afhome/accounts/codex/work"}
}

// A sibling command may switch the shell into a mode that changes how the words
// AFTER it are interpreted. The validator walks the command list once and reads
// each call under default parsing rules, so a mode switch earlier in the same
// list silently invalidates every later verdict.
//
// bash's `set -k` places "all assignment arguments ... in the environment for a
// command", INCLUDING the ones written after the command name. Under it,
// `codex CODEX_HOME=/other` is not the two-word call the walk sees; bash removes
// that word from codex's arguments and launches it with the replacement root.
// Verified against the installed bash before this test was written.
func TestValidateAccountEnvironmentCommand_RefusesKeywordMode(t *testing.T) {
	for _, command := range []string{
		"set -k; codex CODEX_HOME=/other",
		"set -o keyword; codex CODEX_HOME=/other",
		// Combined short options are the same switch (#3402's lesson): a guard
		// that only matches a lone "-k" walks straight past "-ek".
		"set -ek; codex CODEX_HOME=/other",
		"set -e -k; codex CODEX_HOME=/other",
		// The mode outlives the call that set it, so ordering does not save us.
		"npm run build; set -k; codex CODEX_HOME=/other",
		// An unprovable operand could expand to -k.
		"set $AF_FLAGS; codex CODEX_HOME=/other",
		// A `+`-prefixed flag is a turn-OFF option word, not a non-option
		// operand, so it does not end option parsing: a trailing `-k` still
		// switches keyword mode on. `set +e -k` and `set -k` are equivalent in
		// effect on keyword mode under bash.
		"set +e -k; codex CODEX_HOME=/other",
		"set +u -k; codex CODEX_HOME=/other",
		"set +e -ek; codex CODEX_HOME=/other",
		"set +e -o keyword; codex CODEX_HOME=/other",
		// `-o` has conditional arity: it only consumes the next word as a mode
		// name when that word does not start with `-` or `+`. When the next
		// word is another option (like `-k`), bash processes `-o` as bare (it
		// prints settings) and then processes `-k` normally — enabling keyword
		// mode. The scanner must NOT swallow `-k` as the mode name here.
		"set +e -o -k; codex CODEX_HOME=/other",
		// The lone form (no compound) is refused for the same contract reason
		// as a lone `set -k`: keyword mode outlives the call that set it.
		"set +e -k",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q must not silently enable keyword mode", command)
	}
}

// `hash -p pathname name` makes `name` resolve to `pathname`, so every later
// executable-name check in the walk is answering about a different binary than
// the one that will run. Here `runner` is really env, which applies the
// replacement root. Verified against the installed bash.
func TestValidateAccountEnvironmentCommand_RefusesExecutableRemapping(t *testing.T) {
	for _, command := range []string{
		"hash -p /usr/bin/env runner; runner CODEX_HOME=/other codex",
		"hash -rp /usr/bin/env runner; runner CODEX_HOME=/other codex",
		"hash -p /usr/bin/env env; env CODEX_HOME=/other codex",
		"hash $AF_HASH_ARGS; codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q must not silently remap an executable name", command)
	}
}

// The refusals above must be narrow. A process tab is an arbitrary user command,
// and `set -e` prologues and cache maintenance are ordinary shell, not identity
// mutations — refusing them would make account-scoped process tabs useless.
func TestValidateAccountEnvironmentCommand_AllowsOrdinaryShellOptions(t *testing.T) {
	for _, command := range []string{
		"set -e; npm run dev",
		"set -eu -o pipefail; npm run dev",
		"set +k; npm run dev",
		// After `--`, and after any non-option operand, the words are positional
		// parameters rather than options: this does NOT enable keyword mode.
		"set -- -k; npm run dev",
		// A `+`-prefixed flag does not end option parsing, but `--` still does:
		// the `-k` after `--` is the positional parameter $1, not an option.
		"set +e -- -k; npm run dev",
		// A lone `-` is also a bash option terminator: bash documents it as
		// "assign any remaining arguments to the positional parameters", so
		// `-k` after `-` is $1, not an option that enables keyword mode.
		"set +e - -k; npm run dev",
		"hash -r; npm run dev",
		"hash npm; npm run dev",
		"npm run dev",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"command %q is ordinary shell and must stay allowed", command)
	}
}
