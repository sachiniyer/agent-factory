package sessionenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

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
