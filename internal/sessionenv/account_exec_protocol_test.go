//go:build !windows

package sessionenv

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/shellquote"
)

// Tagged !windows because it drives execInvocation and processExec, which live in
// exec_unix.go. The windows stub has no shim protocol at all, so an untagged file
// here breaks that build — and CI cross-builds.

// The shim's argv protocol must carry the launcher's declaration intact, end to
// end: wrap a rewritten claude launch, split it the way /bin/sh would, run the
// shim's own parser over it, and assert the ENVIRONMENT the pane would have
// received. Without the declaration arriving word-for-word, ApplyAccount refuses
// and execInvocation returns an error instead — so the scoped environ IS the proof.
//
// Driven through WrapAccountCommand rather than a hand-built argv because the
// length-prefixed encoding is exactly where a mis-split would hand the guard a
// different claim than the launcher made, and a generated argument is an arbitrary
// string — the plugin directory below contains a space (#3083).
func TestAccountExecProtocol_CarriesTheGeneratedArgsDeclaration(t *testing.T) {
	pluginDir := "/plugins/with a space"
	trustedExecutable := "/opt/af detected/claude"
	generated := []string{"--session-id", "abc-123", "--plugin-dir", pluginDir}
	command := shellquote.Quote(trustedExecutable) + " --session-id abc-123 --plugin-dir " + shellquote.Quote(pluginDir)

	wrapped, err := WrapAccountCommand("/usr/local/bin/af", "claude", "work",
		AccountLaunchProof{TrustedExecutable: trustedExecutable, GeneratedArgs: generated}, nil, command)
	require.NoError(t, err)

	// Split with this package's own parser — the same one the guard uses — rather
	// than a second tokenizer that could disagree with it.
	call, ok := singleSimpleCall(wrapped)
	require.True(t, ok, "the wrapped launch must be a single simple command")
	words, ok := literalCommandArgs(call.Args)
	require.True(t, ok, "every word must survive quoting as a literal")
	require.Equal(t, AccountExecMarker, words[1])

	accountDir := t.TempDir()
	prevLookup := AccountLookup
	AccountLookup = func(agent, name string) (Account, error) {
		return Account{Agent: agent, Name: name, Dir: accountDir}, nil
	}
	sentinel := errors.New("stop before exec")
	var gotEnviron []string
	var gotArgv []string
	prevExec := processExec
	processExec = func(_ string, argv []string, environ []string) error {
		gotArgv = append([]string(nil), argv...)
		gotEnviron = append([]string(nil), environ...)
		return sentinel
	}
	t.Cleanup(func() { AccountLookup, processExec = prevLookup, prevExec })

	t.Setenv("ANTHROPIC_API_KEY", "sk-ambient-must-not-survive")
	err = execInvocation(words[2:], true)
	require.ErrorIs(t, err, sentinel,
		"a refusal here means the declaration did not survive argv, which is the #3083 127")

	require.Equal(t, command, gotArgv[2],
		"the command operand must reach /bin/sh verbatim, quoting and all")
	dir, present := envValue(gotEnviron, "CLAUDE_CONFIG_DIR")
	require.True(t, present, "the account's credential root must be injected")
	require.Equal(t, accountDir, dir)
	_, leaked := envValue(gotEnviron, "ANTHROPIC_API_KEY")
	require.False(t, leaked, "the ambient key outranks the config directory and must be gone")
}

// A malformed count must refuse rather than mis-split — the fail-closed direction
// for the protocol itself, since every claim the boundary verifies rides on it.
func TestAccountExecProtocol_RefusesAMiscountedInvocation(t *testing.T) {
	for _, args := range [][]string{
		{"claude", "0", "work"},                        // truncated before proof fields
		{"claude", "0", "work", "", "2", "--only-one"}, // count exceeds the words present
		{"claude", "0", "work", "", "-1", "cmd"},       // negative
		{"claude", "0", "work", "", "0", "cmd", "extra"},
		{"claude", "0", "work", "", "x", "cmd"}, // not a number
	} {
		require.Error(t, execInvocation(args, true), "argv %v must be refused, not guessed at", args)
	}
}

// A maximum-sized generated count must be REFUSED, not turned into a panic
// (#3083 review, P2). Bounds-checking with `trailing+generatedCount` overflows to a
// negative bound, the check passes, and the slice panics — so the protocol's
// fail-closed promise breaks exactly where a malformed invocation is most likely to
// be deliberate.
func TestAccountExecProtocol_RefusesAnOverflowingGeneratedCount(t *testing.T) {
	for _, count := range []string{"9223372036854775807", "9223372036854775806", "4611686018427387904"} {
		require.NotPanics(t, func() {
			require.Error(t, execInvocation([]string{"claude", "0", "work", "", count, "claude"}, true),
				"generated count %s must be refused", count)
		}, "a malformed count must return the generic refusal, never panic (count %s)", count)
	}
}
