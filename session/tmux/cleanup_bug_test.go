package tmux

import (
	"os/exec"
	"strings"
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
	t.Setenv("AGENT_FACTORY_HOME", "/owned-home")
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
			// Every session carries this home's ownership marker (#1122) so
			// the test isolates the prefix-anchoring behavior.
			if len(cmd.Args) > 1 && cmd.Args[1] == "show-environment" {
				return []byte("AF_HOME=/owned-home\n"), nil
			}
			return []byte(tmuxOutput), nil
		},
	}

	err := CleanupSessions(cmdExec)
	require.NoError(t, err)
	require.Equal(t, []string{"=af_real_session:"}, killedSessions,
		"only `af_`-prefixed sessions should be killed, each via an `=name:` exact-match target (#1006); `my_af_project` must be left alone")
}

// TestCleanupSessionsHandlesMixedAndColonNames covers the full matrix from
// the #613 audit: prefixed, suffixed-but-not-prefixed, mid-name, and a
// session name that itself contains colons (tmux usually rewrites those,
// but the cleanup path should still extract the name through the first ':'
// without duplicating the kill).
func TestCleanupSessionsHandlesMixedAndColonNames(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", "/owned-home")
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
			if len(cmd.Args) > 1 && cmd.Args[1] == "show-environment" {
				return []byte("AF_HOME=/owned-home\n"), nil
			}
			return []byte(tmuxOutput), nil
		},
	}

	err := CleanupSessions(cmdExec)
	require.NoError(t, err)
	require.Equal(t, []string{"=af_real_session:", "=af_a:"}, killedSessions,
		"only `af_`-prefixed sessions should be killed via `=name:` exact-match targets (#1006), with `af_a:b:c` truncated to `af_a` and killed exactly once")
}

// TestCleanupSessionsOnlyKillsOwnedSessions guards the #1122 home-scoping:
// the af_ prefix alone is not ownership. A session stamped with another
// agent-factory home's AF_HOME marker (a second install, or a test sandbox
// whose sweep escaped onto this server) and a session with no marker at all
// (pre-marker build, tmux <3.2) must both survive the sweep; only sessions
// carrying THIS home's marker die.
func TestCleanupSessionsOnlyKillsOwnedSessions(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", "/owned-home")
	tmuxOutput := `af_mine: 1 windows (created Wed May 20 12:00:00 2026) [179x47]
af_theirs: 1 windows (created Wed May 20 12:01:00 2026) [179x47]
af_legacy: 1 windows (created Wed May 20 12:02:00 2026) [179x47]`

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
			if len(cmd.Args) > 1 && cmd.Args[1] == "show-environment" {
				switch {
				case strings.Contains(strings.Join(cmd.Args, " "), "af_mine"):
					return []byte("AF_HOME=/owned-home\n"), nil
				case strings.Contains(strings.Join(cmd.Args, " "), "af_theirs"):
					return []byte("AF_HOME=/another-home\n"), nil
				default:
					// tmux exits non-zero when the variable is unset.
					return nil, errExit1
				}
			}
			return []byte(tmuxOutput), nil
		},
	}

	err := CleanupSessions(cmdExec)
	require.NoError(t, err)
	require.Equal(t, []string{"=af_mine:"}, killedSessions,
		"only the session carrying this home's AF_HOME marker may be killed; foreign-home and marker-less sessions must survive (#1122)")
}
