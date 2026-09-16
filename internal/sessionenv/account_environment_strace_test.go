package sessionenv

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// strace is modeled without an option grammar: the child boundary is never
// resolved, every suffix of the argv is judged as a command line, and a small
// hazard record judges the operands of the two option shapes that mutate or
// exec outside that argv. These cases pin the contract.
func TestCommandMutatesAccountEnvironment_StraceSuffixModel(t *testing.T) {
	codex := accountScopedNames("codex", "CODEX_HOME")
	for name := range accountShellStartupNames {
		codex[name] = struct{}{}
	}
	cases := []struct {
		command string
		want    bool
	}{
		// -o/--output operands beginning with | or ! are spawned as commands
		// by strace itself — a mutation channel the suffix scan cannot see
		// because the operand is one shell word.
		{"strace -o '|env CODEX_HOME=/other codex' true", true},
		{"strace -o'|env CODEX_HOME=/other codex' true", true},
		{"strace --output='|env CODEX_HOME=/other codex' true", true},
		{"strace --output '|env CODEX_HOME=/other codex' true", true},
		{"strace -o '!env CODEX_HOME=/other codex' true", true},
		{"strace -o '|sh -c \"touch /tmp/pwn\"' true", true},
		{"strace -fo '|env CODEX_HOME=/other codex' true", true},
		// getopt_long resolves unambiguous prefixes of --output the same way;
		// ambiguous spellings exit in strace and refuse here as well.
		{"strace --out='|env CODEX_HOME=/other codex' true", true},
		// Ordinary file operands are benign: writing a trace is not a
		// mutation of the sibling environment.
		{"strace -o /tmp/trace.out codex", false},
		{"strace -o/tmp/trace.out codex", false},
		{"strace --output=/tmp/trace.out codex", false},
		{"strace -A -o /tmp/trace.out codex", false},
		{"strace --output-append-mode -o /tmp/trace.out codex", false},
		// -E/--env injects or removes a variable in the traced child; the
		// operand is judged like the env builtin's.
		{"strace -E CODEX_HOME=/other codex", true},
		{"strace -E CODEX_HOME codex", true},
		{"strace -ECODEX_HOME=/other codex", true},
		{"strace --env=CODEX_HOME=/other codex", true},
		{"strace --env CODEX_HOME=/other codex", true},
		{"strace --e CODEX_HOME=/other codex", true},
		{"strace -E PORT=3000 codex", false},
		{"strace --env=PORT=3000 codex", false},
		// -Y is argument-free in strace 6.8. The grammar-free model needs no
		// arity table to get this right: env is judged as a suffix, so the
		// option cannot smuggle the assignment past it.
		{"strace -Y env CODEX_HOME=/other codex", true},
		{"strace -Y -f env CODEX_HOME=/other codex", true},
		// A child hidden behind any option spelling is caught by its own
		// suffix, not by knowing where the options end.
		{"strace -e trace=%file env CODEX_HOME=/other codex", true},
		{"strace -s 128 env CODEX_HOME=/other codex", true},
		{"strace -p 1 -o /tmp/t env CODEX_HOME=/other codex", true},
		{"strace env CODEX_HOME=/other codex", true},
		{"strace sh -c 'export CODEX_HOME=/x; codex'", true},
		// A nested strace argv is itself a suffix judged by the same rules.
		{"strace strace -o '|env CODEX_HOME=/other codex' true", true},
		{"strace strace -E CODEX_HOME=/x env true", true},
		// A strace inside another unrecognized wrapper's tail keeps its own
		// hazard record — the pipe-output exec is still refused.
		{"perf strace -o '|env CODEX_HOME=/other codex' true", true},
		{"perf strace -E CODEX_HOME=/x true", true},
		{"perf strace -o /tmp/t true", false},
		// A non-literal word is unreducible: its expansion is a suffix the
		// scan cannot see, so the whole invocation fails closed.
		{"strace -o $f codex", true},
		{"strace $W codex", true},
		{"strace ${W:-env} CODEX_HOME=/other codex", true},
		// Unquoted glob metacharacters are not literals either: /bin/sh -c
		// expands them into different argv before strace runs, so a filename
		// such as CODEX_HOME=x or |cmd could silently become the operand.
		{"strace -E * codex", true},
		{"strace -o * codex", true},
		{"strace -o /tmp/*.out codex", true},
		{"strace -e 'trace=*' codex", false},
		{"strace -o '/tmp/*.out' codex", false},
		// An escaped metacharacter resolves to its literal value: \* is a
		// literal asterisk operand (benign), but \| resolves to | and still
		// trips the output-command hazard the raw string would have missed.
		{"strace -E \\* codex", false},
		{"strace -o \\|env CODEX_HOME=/other codex true", true},
		// A nested strace behind an unrecognized argv-passthrough wrapper runs
		// the full suffix model, not just the hazard record: its xargs child
		// is judged as a command line exactly as at top level.
		{"valgrind strace xargs -I{} env {} codex", true},
		{"perf strace xargs -I{} env {} codex", true},
		{"valgrind strace -o /tmp/t true", false},
		// Class-B over-refusals, priced by the design: an option operand that
		// spells a denied assignment mutates nothing as a filename or filter,
		// but the same words as a command-line suffix would — so they refuse.
		{"strace -o CODEX_HOME=x codex", true},
		{"strace -e CODEX_HOME=x codex", true},
		{"strace -e trace=env,CODEX_HOME=x codex", false},
		// Ordinary invocations stay allowed.
		{"strace codex", false},
		{"strace -e trace=%file codex", false},
		{"strace -p 1234", false},
		{"strace -f -s 128 codex --flag", false},
		{"strace -o /tmp/t nice -n 5 codex", false},
	}
	for _, test := range cases {
		got := commandMutatesAccountEnvironment(test.command, codex)
		require.Equal(t, test.want, got, "command %q", test.command)
	}
}

// The suffix scan judges every suffix of a nested strace argv as its own
// command, so nested wrappers converge exponentially in the worst case. The
// shared evaluation budget turns an attacker-sized nesting from a hang into a
// refusal; this pins both that bound and that an ordinary nesting still
// returns a verdict.
func TestCommandMutatesAccountEnvironment_StraceBudgetRefusal(t *testing.T) {
	codex := accountScopedNames("codex", "CODEX_HOME")
	// ~20 nested strace words needs ~2^20 suffix walks — far over budget — so
	// the walk must refuse rather than hang.
	deep := "strace " + strings.Repeat("strace ", 20) + "codex"
	require.True(t, commandMutatesAccountEnvironment(deep, codex),
		"attacker-sized nesting must refuse inside the budget")
	// A few nested layers stay decidable.
	require.False(t, commandMutatesAccountEnvironment("strace strace strace codex", codex))
}

// The suffix scan meters one slot per suffix judgment, so an ordinary long
// flat argv (a compiler-style command with hundreds of file arguments) is not
// refused by cumulative charging — the shared budget only bounds the nested
// work each suffix descent actually does.
func TestCommandMutatesAccountEnvironment_StraceLongFlatArgv(t *testing.T) {
	codex := accountScopedNames("codex", "CODEX_HOME")
	flat := "strace codex " + strings.Repeat("src/file.o ", 400)
	require.False(t, commandMutatesAccountEnvironment(flat, codex),
		"ordinary long strace argv must not exhaust the shared budget")
}

// The refusal error names the exact word af could not prove literal so the
// user can pin a literal and self-correct — without it a fail-closed change is
// a support burden, not a fixable message.
func TestValidateAccountEnvironmentCommand_NamesUnprovableWord(t *testing.T) {
	err := ValidateAccountEnvironmentCommand(
		`strace -o "$AF_TRACE_FILE" codex`, scopedProcessTabAccount())
	require.Error(t, err)
	require.Contains(t, err.Error(), `$AF_TRACE_FILE`,
		"refusal must name the word that failed proof")
	require.Contains(t, err.Error(), "replace it with a literal",
		"refusal must tell the user how to fix it")
}
