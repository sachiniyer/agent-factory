package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
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
			if len(cmd.Args) > 1 && cmd.Args[1] == "list-panes" {
				return nil, nil
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
			if len(cmd.Args) > 1 && cmd.Args[1] == "list-panes" {
				return nil, nil
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
	missingMarkerErr := tmuxMissingEnvironmentError(t)
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
					if cmd.Args[len(cmd.Args)-1] == EnvMarkerHome {
						return nil, missingMarkerErr
					}
					// The unfiltered fallback authoritatively represents a missing
					// marker by succeeding; its contents are never authorization.
					return []byte("PATH=/usr/bin\n"), nil
				}
			}
			if len(cmd.Args) > 1 && cmd.Args[1] == "list-panes" {
				return nil, nil
			}
			return []byte(tmuxOutput), nil
		},
	}

	err := CleanupSessions(cmdExec)
	require.NoError(t, err)
	require.Equal(t, []string{"=af_mine:"}, killedSessions,
		"only the session carrying this home's AF_HOME marker may be killed; foreign-home and marker-less sessions must survive (#1122)")
}

// TestCleanupSessionsClassifiesHomeByFilesystem guards #7435 item 2: the
// sweep's AF_HOME compare used to be filepath.Clean on both sides, so a
// marker that only Cleans to this home was admitted. Now only literal
// equality or a SameFile stat proves the home: a lexical alias through a
// missing directory is unproven (left alone, logged as unproven — not as a
// different home, which is the nit's distinction), while a symlink that stats
// to the home is the same home and its session dies.
func TestCleanupSessionsClassifiesHomeByFilesystem(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	// A regular file sits where a directory would need to be: the path Cleans
	// to the home but traversal through it fails with ENOTDIR on every
	// platform — stat can never prove it. The ".." is spliced in by hand
	// because filepath.Join CLEANS its result: a Join-built alias collapses
	// to the home literally and the fixture proves nothing (#7435).
	notDir := filepath.Join(filepath.Dir(home), "not-a-dir")
	require.NoError(t, os.WriteFile(notDir, []byte("x"), 0o600))
	alias := notDir + string(os.PathSeparator) + ".." + string(os.PathSeparator) + filepath.Base(home)
	require.Equal(t, home, filepath.Clean(alias),
		"the alias fixture must differ only lexically — a Clean-based compare would admit it")
	require.NotEqual(t, home, alias,
		"the alias must not BE the home literally, or the fixture tests nothing")
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(home, link))
	foreign := t.TempDir()
	require.NotEqual(t, home, foreign)

	tmuxOutput := `af_alias: 1 windows (created Wed May 20 12:00:00 2026) [179x47]
af_link: 1 windows (created Wed May 20 12:01:00 2026) [179x47]
af_foreign: 1 windows (created Wed May 20 12:02:00 2026) [179x47]`

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
				case strings.Contains(strings.Join(cmd.Args, " "), "af_alias"):
					return []byte("AF_HOME=" + alias + "\n"), nil
				case strings.Contains(strings.Join(cmd.Args, " "), "af_link"):
					return []byte("AF_HOME=" + link + "\n"), nil
				default:
					return []byte("AF_HOME=" + foreign + "\n"), nil
				}
			}
			if len(cmd.Args) > 1 && cmd.Args[1] == "list-panes" {
				return nil, nil
			}
			return []byte(tmuxOutput), nil
		},
	}

	err := CleanupSessions(cmdExec)
	require.NoError(t, err)
	require.Equal(t, []string{"=af_link:"}, killedSessions,
		"only the session whose AF_HOME stats to this home may be killed; a lexical alias and a foreign home must survive (#7435)")
}

func tmuxMissingEnvironmentError(t *testing.T) error {
	t.Helper()
	cmd := exec.Command("sh", "-c", "printf 'unknown variable: AF_HOME\\n' >&2; exit 1")
	_, err := cmd.Output()
	if err == nil {
		t.Fatal("missing-variable command unexpectedly succeeded")
	}
	return err
}
