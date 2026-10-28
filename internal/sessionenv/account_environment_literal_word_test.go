package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// #4523: the account-environment walk read an unquoted word by its written
// text, but the shell passes something else — backslashes are removed (POSIX,
// so dash does it too and the exec shim runs `/bin/sh -c`), and brace
// expansion, globs, a leading tilde and $'…' are resolved at run time. Each
// shape below was measured against the real shell in the issue; every one
// replaces an identity variable and was ACCEPTED on master.
func TestCommandMutatesAccountEnvironment_ReadsWordsAsTheShellPassesThem(t *testing.T) {
	names := map[string]struct{}{"CODEX_HOME": {}, "OPENAI_API_KEY": {}}
	for name := range accountShellStartupNames {
		names[name] = struct{}{}
	}
	for _, test := range []struct {
		name    string
		command string
		want    bool
	}{
		// Backslash removal: the command name.
		{"escaped env", `\env CODEX_HOME=/o codex`, true},
		{"escape inside env", `e\nv CODEX_HOME=/o codex`, true},
		{"escaped env behind ionice", `ionice -c 3 \env CODEX_HOME=/o codex`, true},
		{"escaped env behind nice", `nice -n 5 e\nv CODEX_HOME=/o codex`, true},
		{"escaped env behind timeout", `timeout 5 \env CODEX_HOME=/o codex`, true},
		{"escaped env behind strace", `strace \env CODEX_HOME=/o codex`, true},
		{"escaped env behind xargs", `xargs \env CODEX_HOME=/o codex`, true},
		{"escaped unset", `\unset CODEX_HOME`, true},
		{"escape inside unset", `un\set CODEX_HOME`, true},
		{"escaped shell -c", `\sh -c 'unset CODEX_HOME'`, true},
		// Backslash removal: an assignment or operand name.
		{"escaped name in env assignment", `env CODEX\_HOME=/o codex`, true},
		{"escaped name mid-word", `env CODEX_HO\ME=/o codex`, true},
		{"escaped equals", `env CODEX_HOME\=/o codex`, true},
		{"escaped name in env -u", `env -u CODEX\_HOME codex`, true},
		{"escaped name in unset", `unset CODEX\_HOME`, true},
		{"escaped name in export", `export CODEX\_HOME=/o`, true},
		// A backslash in a bare assignment's NAME makes it a command word, not
		// an assignment, so nothing is replaced (env above receives the
		// decoded argument, which is why it IS an assignment there).
		{"escaped name in bare assignment is a command", `CODEX\_HOME=/o codex`, false},
		// Brace expansion (bash running as sh).
		{"brace command", `{env,CODEX_HOME=/o} codex`, true},
		{"brace behind nice", `nice -n 10 {env,CODEX_HOME=/o} codex`, true},
		{"brace behind nohup", `nohup {env,CODEX_HOME=/o} codex`, true},
		{"brace operand", `env {CODEX_HOME=/o,x} codex`, true},
		{"brace unset", `{unset,CODEX_HOME}`, true},
		{"brace in env -u", `env -u CODEX_HOME{,} codex`, true},
		// Glob and tilde resolve against the launch directory / HOME.
		{"glob command", `en* CODEX_HOME=/o codex`, true},
		{"glob assignment value", `env CODEX_HOME=/o* codex`, true},
		{"glob class command", `e[n]v CODEX_HOME=/o codex`, true},
		{"glob question command", `en? CODEX_HOME=/o codex`, true},
		// Bash-only quoting forms.
		{"dollar-quote env", `$'env' CODEX_HOME=/o codex`, true},
		{"dollar-quote name", `env $'CODEX_HOME'=/o codex`, true},
		// Controls: the plain spellings, and the same characters quoted or
		// escaped into literal text where they decide nothing.
		{"plain env", `env CODEX_HOME=/o codex`, true},
		{"plain env quoted name", `e'n'v CODEX_HOME=/o codex`, true},
		{"quoted braces canary", `echo '{a,b}'`, false},
		{"quoted glob canary", `echo '*' '?'`, false},
		{"quoted tilde canary", `echo '~'`, false},
		{"brace in a literal command's argument", `nice -n 5 codex {a,b}`, false},
		{"glob in a literal command's argument", `nice -n 5 codex *.md`, false},
		{"escape in a literal command's argument", `nice -n 5 codex \-\-model`, false},
		{"escaped name that is not the account's", `env PORT\_X=1 codex`, false},
		{"test builtin as plain words", `[ -f x ] && codex`, false},
		{"brace without a separator", `echo {}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, commandMutatesAccountEnvironment(test.command, names), "command %q", test.command)
		})
	}
}
