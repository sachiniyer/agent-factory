package tmux

import (
	"os/exec"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/stretchr/testify/require"
)

// TestCleanupSessionsOnlyKillsPrefixedSessions guards against #613: an
// unanchored regex used to match `af_` anywhere on a `tmux ls` line, which
// caused a user's `my_af_project` session to be misread as `af_project` and
// destroyed. After the fix, only sessions whose names start with the `af_`
// prefix are killed.
func TestCleanupSessionsOnlyKillsPrefixedSessions(t *testing.T) {
	tmuxOutput := `my_af_project: 1 windows (created Wed May 20 12:00:00 2026) [179x47]
af_real_session: 1 windows (created Wed May 20 12:01:00 2026) [179x47]`

	var killedSessions []string
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			for i, arg := range cmd.Args {
				if arg == "-t" && i+1 < len(cmd.Args) {
					killedSessions = append(killedSessions, cmd.Args[i+1])
				}
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte(tmuxOutput), nil
		},
	}

	err := CleanupSessions(cmdExec)
	require.NoError(t, err)
	require.Equal(t, []string{"af_real_session"}, killedSessions,
		"only `af_`-prefixed sessions should be killed; `my_af_project` must be left alone")
}

// TestCleanupSessionsHandlesMixedAndColonNames covers the full matrix from
// the #613 audit: prefixed, suffixed-but-not-prefixed, mid-name, and a
// session name that itself contains colons (tmux usually rewrites those,
// but the cleanup path should still extract the name through the first ':'
// without duplicating the kill).
func TestCleanupSessionsHandlesMixedAndColonNames(t *testing.T) {
	tmuxOutput := `my_af_personal: 1 windows (created Wed May 20 12:00:00 2026) [179x47]
af_real_session: 1 windows (created Wed May 20 12:01:00 2026) [179x47]
prefix_af_other: 1 windows (created Wed May 20 12:02:00 2026) [179x47]
af_a:b:c: 1 windows (created Wed May 20 12:03:00 2026) [179x47]`

	var killedSessions []string
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			for i, arg := range cmd.Args {
				if arg == "-t" && i+1 < len(cmd.Args) {
					killedSessions = append(killedSessions, cmd.Args[i+1])
				}
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte(tmuxOutput), nil
		},
	}

	err := CleanupSessions(cmdExec)
	require.NoError(t, err)
	require.Equal(t, []string{"af_real_session", "af_a"}, killedSessions,
		"only `af_`-prefixed sessions should be killed, with `af_a:b:c` truncated to `af_a` and killed exactly once")
}
