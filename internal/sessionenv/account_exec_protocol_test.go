//go:build !windows

package sessionenv

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/shellquote"
)

// Tagged !windows because it drives execInvocation and processExec, which live in
// exec_unix.go. The windows stub has no shim protocol at all, so an untagged file
// here breaks that build — and CI cross-builds.

// installAccountLaunchProof sets the out-of-band proof variable exactly as the
// launcher does, so a test can drive the real end-to-end path without rebuilding
// tmux's session environment by hand. It returns the variable's name so a test
// can also assert the shim strips it before exec.
func installAccountLaunchProof(t *testing.T, proof AccountLaunchProof) string {
	t.Helper()
	entry, err := AccountLaunchProofEnvEntry(proof)
	require.NoError(t, err)
	name, value, ok := strings.Cut(entry, "=")
	require.True(t, ok)
	t.Setenv(name, value)
	return name
}

// The shim's protocol must carry the launcher's declaration intact, end to end:
// install the proof out of band, wrap a rewritten claude launch, split it the
// way /bin/sh would, run the shim's own parser over it, and assert the
// ENVIRONMENT the pane would have received. Without the proof arriving
// intact, ApplyAccount refuses and execInvocation returns an error instead — so
// the scoped environ IS the proof.
//
// Driven through WrapAccountCommand rather than a hand-built argv because the
// length-prefixed encoding is exactly where a mis-split would hand the guard a
// different claim than the launcher made, and a generated argument is an
// arbitrary string — the plugin directory below contains a space (#3083).
func TestAccountExecProtocol_CarriesTheGeneratedArgsDeclaration(t *testing.T) {
	pluginDir := "/plugins/with a space"
	trustedExecutable := "/opt/af detected/claude"
	generated := []string{"--session-id", "abc-123", "--plugin-dir", pluginDir}
	command := shellquote.Quote(trustedExecutable) + " --session-id abc-123 --plugin-dir " + shellquote.Quote(pluginDir)
	proof := AccountLaunchProof{TrustedExecutable: trustedExecutable, GeneratedArgs: generated}

	wrapped, err := WrapAccountCommand("/usr/local/bin/af", "claude", "work", nil, command)
	require.NoError(t, err)

	// Split with this package's own parser — the same one the guard uses — rather
	// than a second tokenizer that could disagree with it.
	call, ok := singleSimpleCall(wrapped)
	require.True(t, ok, "the wrapped launch must be a single simple command")
	words, ok := literalCommandArgs(call.Args)
	require.True(t, ok, "every word must survive quoting as a literal")
	require.Equal(t, AccountExecMarker, words[1])
	// The proof no longer rides in argv: the account-scoped markers carry only the
	// agent, the extras count, the account name, and the command.
	require.Equal(t, []string{"/usr/local/bin/af", AccountExecMarker, "claude", "0", "work", command},
		words, "the account marker argv must carry no proof fields")

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

	proofName := installAccountLaunchProof(t, proof)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ambient-must-not-survive")
	err = execInvocation(words[2:], true)
	require.ErrorIs(t, err, sentinel,
		"a refusal here means the declaration did not survive the channel, which is the #3083 127")

	require.Equal(t, command, gotArgv[2],
		"the command operand must reach /bin/sh verbatim, quoting and all")
	dir, present := envValue(gotEnviron, "CLAUDE_CONFIG_DIR")
	require.True(t, present, "the account's credential root must be injected")
	require.Equal(t, accountDir, dir)
	_, leaked := envValue(gotEnviron, "ANTHROPIC_API_KEY")
	require.False(t, leaked, "the ambient key outranks the config directory and must be gone")
	_, proofLeaked := envValue(gotEnviron, proofName)
	require.False(t, proofLeaked, "the proof variable must be stripped before the agent runs")
}

// A repo-controlled program_overrides value that re-invokes af under
// AccountExecMarker supplies its own argv but cannot supply the launcher's
// out-of-band proof: it runs in an UNSCOPED outer pane, whose environment never
// received the proof. The shim must refuse rather than honor the argv, matching
// the #3051 fail-closed property — this is the regression witness for the
// forgeable-argv TrustedExecutable bypass.
//
// The command is a bare `claude` (so AgentForCommand agrees with the claimed
// agent) to isolate the proof-missing refusal from the disagreement refusal.
func TestAccountExecProtocol_RefusesAReinvocationWithoutLaunchProof(t *testing.T) {
	accountDir := t.TempDir()
	prevLookup := AccountLookup
	AccountLookup = func(agent, name string) (Account, error) {
		if agent != "claude" || name != "work" {
			return Account{}, errors.New("no such account")
		}
		return Account{Agent: "claude", Name: "work", Dir: accountDir}, nil
	}
	var execed bool
	prevExec := processExec
	processExec = func(string, []string, []string) error {
		execed = true
		return errors.New("must not reach exec")
	}
	t.Cleanup(func() { AccountLookup, processExec = prevLookup, prevExec })
	t.Setenv(accountLaunchProofEnvVar, "") // no launcher set the proof

	// Attacker argv from a repo whose program_overrides re-invokes af under the
	// account marker. The command is a bare `claude`, so the disagreement guard
	// does not fire; the proof-missing refusal must.
	attacker := []string{"claude", "0", "work", "claude"}
	err := execInvocation(attacker, true)
	require.Error(t, err, "a re-invocation without the launcher's proof must be refused")
	require.False(t, execed, "the account scope must never be applied to a forged argv")
}

// A forged argv whose command does NOT resolve to the selected agent is refused
// on disagreement alone, even if a proof were somehow supplied inline — the
// account scope is granted only to a command that IS the selected agent
// (#3051/#4356). This is the second leg of the boundary.
func TestAccountExecProtocol_RefusesADisagreeingCommand(t *testing.T) {
	prevLookup := AccountLookup
	AccountLookup = func(agent, name string) (Account, error) {
		return Account{Agent: agent, Name: name, Dir: t.TempDir()}, nil
	}
	var execed bool
	prevExec := processExec
	processExec = func(string, []string, []string) error {
		execed = true
		return errors.New("must not reach exec")
	}
	t.Cleanup(func() { AccountLookup, processExec = prevLookup, prevExec })
	installAccountLaunchProof(t, AccountLaunchProof{TrustedExecutable: "./evil"})

	// An attacker executable that does not basename to a supported agent: the
	// disagreement guard refuses it before any proof is honored.
	err := execInvocation([]string{"claude", "0", "work", "./evil"}, true)
	require.Error(t, err, "a command that is not the selected agent must be refused")
	require.False(t, execed, "a disagreeing command must not reach exec")
}

// A malformed invocation must refuse rather than mis-split — the fail-closed
// direction for the protocol itself, since every claim the boundary verifies
// rides on it. The proof is installed so these exercise the COUNT and shape
// checks rather than the proof-missing gate.
func TestAccountExecProtocol_RefusesAMiscountedInvocation(t *testing.T) {
	installAccountLaunchProof(t, AccountLaunchProof{})
	for _, args := range [][]string{
		{"claude", "0", "work"},                    // truncated before the command
		{"claude", "0", "work", "cmd", "extra"},    // count 0 but two trailing words
		{"claude", "-1", "work", "cmd"},            // negative count
		{"claude", "x", "work", "cmd"},             // count not a number
		{"claude", "2", "work", "only-one", "cmd"}, // count exceeds the words present
	} {
		require.Error(t, execInvocation(args, true), "argv %v must be refused, not guessed at", args)
	}
}

// A maximum-sized count must be REFUSED, not turned into a panic (#3083 review,
// P2). Bounds-checking with `offset+count` overflows and the slice below would
// PANIC or mis-decide instead of returning this generic refusal — so the
// protocol's fail-closed promise holds exactly where a malformed invocation is
// most likely to be deliberate.
func TestAccountExecProtocol_RefusesAnOverflowingCount(t *testing.T) {
	installAccountLaunchProof(t, AccountLaunchProof{})
	for _, count := range []string{"9223372036854775807", "9223372036854775806", "4611686018427387904"} {
		args := []string{"claude", count, "work", "claude"}
		require.NotPanics(t, func() {
			require.Error(t, execInvocation(args, true),
				"count %s must be refused", count)
		}, "a malformed count must return the generic refusal, never panic (count %s)", count)
	}
}

// Regression witness for byte-preservation in the launch-proof wire format:
// Unix executable and generated path strings may carry arbitrary non-NUL bytes
// (a filesystem path is not required to be valid UTF-8). encoding/json replaces
// invalid UTF-8 in a Go string with U+FFFD, so a TrustedExecutable or generated
// path containing such a byte would decode to a different string than the
// resolver derived byte-for-byte, and accountLaunchProofsMatch would then
// reject an otherwise-valid account-scoped launch (#review, Codex P2 on
// 458eb57 — account_launch_proof.go:109). The envelope base64-wraps each
// field's raw bytes before JSON-marshaling, so the round-trip is byte-exact and
// the matcher compares the same strings the resolver produced.
func TestAccountExecProtocol_WirePreservesNonUTF8Bytes(t *testing.T) {
	// 0xe9 and 0xff are invalid-UTF-8 lead/continuation bytes that the JSON
	// encoder would otherwise replace with U+FFFD.
	proof := AccountLaunchProof{
		TrustedExecutable: "/opt/caf\xe9claude",
		GeneratedArgs:     []string{"--plugin-dir", "/plugins/\xffdir", "--flag\xf0"},
	}

	entry, err := AccountLaunchProofEnvEntry(proof)
	require.NoError(t, err)
	name, value, ok := strings.Cut(entry, "=")
	require.True(t, ok)
	t.Setenv(name, value)

	decoded, err := decodeAccountLaunchProofEnv(value)
	require.NoError(t, err, "a non-UTF-8 proof must round-trip the wire format")
	require.Equal(t, proof, decoded,
		"the decoded proof must be byte-identical to the original so accountLaunchProofsMatch compares the resolver's derivation exactly")
	require.True(t, accountLaunchProofsMatch(proof, decoded),
		"the matcher must agree on a byte-exact round-trip")
}
