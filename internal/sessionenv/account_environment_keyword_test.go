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
		// `o` embedded in a cluster has the same conditional arity as standalone
		// `-o`: the following word is consumed as the mode name when it does not
		// start with `-` or `+`. `set +e -eo keyword` enables `-e` and then
		// `-o keyword`, which enables keyword mode — the guard must catch this.
		"set +e -eo keyword; codex CODEX_HOME=/other",
		// When a cluster contains both `o` and `k`, bash applies all cluster
		// characters left to right; `o` consumes the following word as a mode
		// name AND `k` still enables keyword mode. The guard must check for `k`
		// in the cluster even after consuming the `o` mode name.
		"set -ok pipefail; codex CODEX_HOME=/other",
		"set -ko pipefail; codex CODEX_HOME=/other",
		"set -ekxo pipefail; codex CODEX_HOME=/other",
		// The lone form (no compound) is refused for the same contract reason
		// as a lone `set -k`: keyword mode outlives the call that set it.
		"set +e -k",
		// bash aborts its option scan at an UNRECOGNIZED -o/+o long name (it
		// prints an error and returns non-zero): an earlier `-k` stays ON and
		// any later `+k` is never applied. The scanner must model that abort
		// and fail closed on an invalid name, instead of consuming it and
		// letting the trailing `+k` flip keywordMode back off across a flag
		// bash ignores. `|| true` keeps the script alive past the abort on
		// POSIX bash-as-`/bin/sh`. Verified against bash 5.2.21.
		"set -k -o nonsense +k 2>/dev/null || true; codex CODEX_HOME=/other",
		// `+o` with an invalid name aborts bash's scan the same way `-o` does.
		"set -k +o nonsense +k 2>/dev/null || true; codex CODEX_HOME=/other",
		// `o` embedded in a minus-prefixed cluster has the same conditional
		// arity and the same abort-on-invalid-name semantics as standalone
		// `-o`. With `k` before `o` in the cluster, bash applies the `-k`
		// before aborting at the invalid name, leaving keyword mode ON.
		"set -ko nonsense +k 2>/dev/null || true; codex CODEX_HOME=/other",
		// With `o` before `k` in the cluster, bash aborts at the invalid name
		// before it reaches `k`, but the scan is frozen either way — fail
		// closed rather than model which cluster characters the abort skipped.
		"set -ok nonsense +k 2>/dev/null || true; codex CODEX_HOME=/other",
		// An invented `no`-prefixed "negation" is not a bash option name (bash
		// rejects `nopipefail` with `set: nopipefail: invalid option name`); it
		// must NOT be admitted as a mode name, or the trailing `+k` would flip
		// keywordMode back off across a flag bash never applies.
		"set -k -o nopipefail +k 2>/dev/null || true; codex CODEX_HOME=/other",
		// Long names use dashes, not underscores (`interactive-comments`); the
		// underscore spelling is an invalid name that aborts bash's scan.
		"set -k -o interactive_comments +k 2>/dev/null || true; codex CODEX_HOME=/other",
		// The recognizer is case-sensitive: `Nonsense` is not `keyword`-cased
		// and aborts bash's scan the same as any other invalid name.
		"set -k -o Nonsense +k 2>/dev/null || true; codex CODEX_HOME=/other",
		// Fail closed on an invalid -o name even when no `-k` precedes it: bash
		// aborts its scan there, so the scanner cannot prove the final
		// keyword-mode state of any later word. A `set -o nonsense` is itself a
		// bash error, so refusing it costs no legitimate command.
		"set -o nonsense; npm run dev",
		// The embedded-`o` cluster form must fail closed on an invalid name
		// even without a trailing `+k` to cancel — the abort freezes whatever
		// keyword-mode state the cluster established.
		"set -eo nonsense; npm run dev",
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
		// Bash processes options left to right and the last setting wins: `+k`
		// turns keyword mode back off after `-k` turned it on, so this sequence
		// leaves keyword mode disabled and the command is valid.
		"set +e -k +k; npm run dev",
		// Every real bash `set -o` long name must stay allowed — the recognizer
		// exists to fail closed on INVALID names, not to refuse ordinary
		// prologues. `pipefail`, `errexit`, `vi`, and `posix` are all valid.
		"set -o pipefail; npm run dev",
		"set -o errexit; npm run dev",
		"set -o vi; npm run dev",
		"set -o posix -o errexit; npm run dev",
		// `o` embedded in a cluster with a VALID name consumes that name and is
		// allowed; the fail-closed rule only fires on an unrecognized name.
		"set -eo pipefail; npm run dev",
		"set -eo pipefail -o errexit; npm run dev",
		// `+o` with a valid name turns the mode OFF and is allowed when no `-k`
		// turns anything on.
		"set +o keyword; npm run dev",
		"hash -r; npm run dev",
		"hash npm; npm run dev",
		"npm run dev",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"command %q is ordinary shell and must stay allowed", command)
	}
}

// An unrecognized `set -o`/`set +o` long option name fails closed, but the
// refusal must name that option and say af cannot prove what the shell did with
// the rest of the `set` line: the generic "sets an identity or shell-startup
// variable itself" refusal is false for this class, which sets no variable at
// all (e.g. `set -o extendedglob; npm run dev`). A real keyword-mode switch
// (`set -k`) keeps the generic refusal, accurate for it by contract.
func TestValidateAccountEnvironmentCommand_UnrecognizedSetOptionNameRefusal(t *testing.T) {
	for _, tc := range []struct {
		command string
		option  string
	}{
		{"set -o extendedglob; npm run dev", "extendedglob"},
		{"set -o nonsense; npm run dev", "nonsense"},
		{"set -eo nonsense; npm run dev", "nonsense"},
		{"set -k -o nonsense +k 2>/dev/null || true; codex CODEX_HOME=/other", "nonsense"},
		{"set -k +o nonsense +k 2>/dev/null || true; codex CODEX_HOME=/other", "nonsense"},
		{"set -ko nonsense +k 2>/dev/null || true; codex CODEX_HOME=/other", "nonsense"},
		{"set -ok nonsense +k 2>/dev/null || true; codex CODEX_HOME=/other", "nonsense"},
		{"set -k -o nopipefail +k 2>/dev/null || true; codex CODEX_HOME=/other", "nopipefail"},
		{"set -k -o interactive_comments +k 2>/dev/null || true; codex CODEX_HOME=/other", "interactive_comments"},
		{"set -k -o Nonsense +k 2>/dev/null || true; codex CODEX_HOME=/other", "Nonsense"},
	} {
		err := ValidateAccountEnvironmentCommand(tc.command, scopedProcessTabAccount())
		require.Error(t, err, "command %q must be refused", tc.command)
		require.Contains(t, err.Error(), tc.option,
			"command %q: refusal must name the unrecognized option", tc.command)
		require.Contains(t, err.Error(), "cannot prove what the shell did",
			"command %q: refusal must say af cannot prove what the shell did with the rest of the set line", tc.command)
		require.NotContains(t, err.Error(), "sets an identity or shell-startup variable itself",
			"command %q: refusal must not reuse the generic identity-variable message", tc.command)
	}
	// A real keyword-mode switch keeps the generic refusal, accurate for it.
	keywordErr := ValidateAccountEnvironmentCommand("set -k; codex CODEX_HOME=/other", scopedProcessTabAccount())
	require.Error(t, keywordErr)
	require.Contains(t, keywordErr.Error(), "sets an identity or shell-startup variable itself")
	// A recognized long name stays allowed (no refusal at all).
	require.NoError(t, ValidateAccountEnvironmentCommand("set -o pipefail; npm run dev", scopedProcessTabAccount()))
}
