package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A program written with a path — `~/bin/claude` is the common spelling in
// program_overrides — is refused for an account-scoped agent, because only a
// bare name resolved through PATH (or af's own detected executable) proves out.
// The refusal must say so: "use an absolute path" would be wrong advice, since
// an absolute path is refused too.
func TestValidateAccountCommand_PathProgramRefusalNamesTheFix(t *testing.T) {
	account := Account{Agent: "claude", Name: "work", Dir: "/afhome/accounts/claude/work"}
	for _, command := range []string{
		"~/bin/claude",
		"~other/bin/claude",
		"~",
		"/home/siyer/bin/claude",
		"exec /home/siyer/bin/claude",
		"'/home/siyer/my bin'/claude",
	} {
		err := ValidateAccountCommand(command, account)
		require.Error(t, err, "command %q", command)
		require.Contains(t, err.Error(), "use the bare command name `claude` and put its directory on PATH", "command %q", command)
		require.Contains(t, err.Error(), "an absolute path is refused too", "command %q", command)
	}
	// The hint appears only when the executable is a path.
	for _, command := range []string{"claude --extra", "claudee", "env claude"} {
		err := ValidateAccountCommand(command, account)
		if err != nil {
			require.NotContains(t, err.Error(), "bare command name", "command %q", command)
		}
	}
	// A trusted executable is accepted, and a refusal for another reason does
	// not blame its path.
	trusted := account
	trusted.TrustedExecutable = "/home/siyer/bin/claude"
	require.NoError(t, ValidateAccountCommand("/home/siyer/bin/claude", trusted))
	err := ValidateAccountCommand("/home/siyer/bin/claude $X", trusted)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "bare command name")
}
