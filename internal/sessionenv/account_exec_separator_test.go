package sessionenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const skipPermissionsFlag = "--dangerously-skip-permissions"

// The launch shell is the oracle; the validator is not.
//
// Whatever the account boundary accepts is handed to the pane VERBATIM and run
// by `/bin/sh -c`, so a shape the boundary calls provable and /bin/sh then
// refuses is exactly the unexplained 127 this boundary exists to prevent. This
// runs the real /bin/sh on every accepted shape and requires exit 0 — it asserts
// the property rather than any particular verdict, so it stays honest if the
// accepted set changes again (#3557).
//
// On master `exec -- <agent>` is accepted by validation and exits 127 under
// dash, which is /bin/sh on Debian and Ubuntu.
func TestAcceptedAccountCommandLaunchesUnderBinSh(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the pane launches through /bin/sh only on unix")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("no /bin/sh to run the accepted command with: %v", err)
	}

	// A real executable, named so the bare-name rule can resolve it from PATH and
	// reached by absolute path for the trusted-executable rule. It ignores its
	// arguments, so any nonzero exit comes from the shell rather than the agent.
	binDir := t.TempDir()
	agentPath := filepath.Join(binDir, "claude")
	require.NoError(t, os.WriteFile(agentPath, []byte("#!/bin/sh\nexit 0\n"), 0o755))

	for _, tc := range []struct {
		name string
		base string
	}{
		{name: "bare name", base: "claude " + skipPermissionsFlag},
		{name: "exec bare name", base: "exec claude " + skipPermissionsFlag},
		{name: "exec separator bare name", base: "exec -- claude " + skipPermissionsFlag},
		{name: "absolute path", base: agentPath + " " + skipPermissionsFlag},
		{name: "exec absolute path", base: "exec " + agentPath + " " + skipPermissionsFlag},
		{name: "exec separator absolute path", base: "exec -- " + agentPath + " " + skipPermissionsFlag},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The whole producer/consumer pair, as session.accountLaunchProof drives
			// it: af appends its own words to the configured base, declares what it
			// added, and the boundary judges the string the pane will run.
			final := tc.base + " --session-id " + genSessionID
			proof, ok := GenerateAccountLaunchProof(tc.base, final, []string{skipPermissionsFlag})
			if !ok {
				proof = AccountLaunchProof{}
			}
			err := ValidateAccountCommand(final, Account{
				Agent:             "claude",
				Name:              "work",
				TrustedExecutable: proof.TrustedExecutable,
				GeneratedArgs:     proof.GeneratedArgs,
			})
			if err != nil {
				t.Logf("refused, so the pane never runs it: %v", err)
				return
			}
			cmd := exec.Command("/bin/sh", "-c", final)
			cmd.Env = []string{"PATH=" + binDir}
			output, runErr := cmd.CombinedOutput()
			require.NoError(t, runErr,
				"the account boundary accepted %q, but the pane's /bin/sh refused to run it: %s",
				final, output)
		})
	}
}

// The refusal has to name the shell, because the operator's own shell accepts
// the form: an alias written and tested in bash works there and dies in the
// pane, so "could not be proven" would send them looking in the wrong place.
func TestValidateAccountCommandRefusesExecSeparatorNamingTheShell(t *testing.T) {
	for _, command := range []string{
		"exec -- claude " + skipPermissionsFlag,
		"exec -- /opt/claude " + skipPermissionsFlag,
	} {
		err := ValidateAccountCommand(command, Account{
			Agent:             "claude",
			Name:              "work",
			TrustedExecutable: "/opt/claude",
			GeneratedArgs:     []string{skipPermissionsFlag},
		})
		require.Error(t, err, "%q must not be accepted: /bin/sh may refuse it", command)
		require.True(t, IsAccountCommandValidationError(err))
		for _, want := range []string{"exec --", "/bin/sh", "dash", "127"} {
			require.Contains(t, err.Error(), want,
				"the refusal must name the shell that refuses the command")
		}
	}
}

// The separator is the only part that goes: a plain `exec` prefix runs under
// every /bin/sh, and refusing it would re-break the alias #3540 fixed.
func TestValidateAccountCommandStillAcceptsPlainExecPrefix(t *testing.T) {
	const executable = "/opt/claude"
	base := "exec " + executable + " " + skipPermissionsFlag
	final := base + " --session-id " + genSessionID

	proof, ok := GenerateAccountLaunchProof(base, final, []string{skipPermissionsFlag})
	require.True(t, ok)
	require.Equal(t, executable, proof.TrustedExecutable)
	require.NoError(t, ValidateAccountCommand(final, Account{
		Agent:             "claude",
		Name:              "work",
		TrustedExecutable: proof.TrustedExecutable,
		GeneratedArgs:     proof.GeneratedArgs,
	}))
}

// TestCommandIsSingleSimpleCall pins the predicate the config loader's
// auto-detected claude override uses as its defense-in-depth guard: a
// composed override that is not even valid single-simple-call shell syntax
// (e.g. an unbalanced quote left by a corrupted probe) must report false so
// DefaultConfig omits it rather than persisting a value that kills every
// session pane on launch. The predicate is the same one the account launch
// proof uses, so the loader's admission decision matches the boundary that
// governs the persisted value at launch.
func TestCommandIsSingleSimpleCall(t *testing.T) {
	accepts := []struct {
		name    string
		command string
	}{
		{"bare name with flag", "claude --dangerously-skip-permissions"},
		{"flags and double-quoted arg", `claude --prompt "'hi'" --dangerously-skip-permissions`},
		{"single-quoted path with spaces", `'/opt/my claude/claude' --dangerously-skip-permissions`},
		{"double-quoted path with spaces", `"/opt/my claude/claude" --dangerously-skip-permissions`},
		{"absolute path with flag", "/usr/local/bin/claude --model opus --dangerously-skip-permissions"},
		{"exec prefix", "exec claude --dangerously-skip-permissions"},
	}
	for _, tc := range accepts {
		t.Run("accepts/"+tc.name, func(t *testing.T) {
			assert.True(t, CommandIsSingleSimpleCall(tc.command),
				"%q must parse as a single simple shell call", tc.command)
		})
	}
	refuses := []struct {
		name    string
		command string
	}{
		{"unbalanced opening double quote", `claude --prompt "'hi --dangerously-skip-permissions`},
		{"unbalanced opening single quote", "claude --prompt 'hi --dangerously-skip-permissions"},
		{"unbalanced closing single quote", "claude --prompt hi' --dangerously-skip-permissions"},
		{"empty", ""},
		{"binary and command", "claude --model opus && echo ready --dangerously-skip-permissions"},
		{"negated command", "! claude --dangerously-skip-permissions"},
		{"backgrounded command", "claude --dangerously-skip-permissions &"},
		{"pipelined command", "claude --dangerously-skip-permissions | cat"},
		{"redirect", "claude --dangerously-skip-permissions > /tmp/log"},
		{"two statements", "claude --dangerously-skip-permissions ; echo done"},
	}
	for _, tc := range refuses {
		t.Run("refuses/"+tc.name, func(t *testing.T) {
			assert.False(t, CommandIsSingleSimpleCall(tc.command),
				"%q must NOT parse as a single simple shell call", tc.command)
		})
	}
}
