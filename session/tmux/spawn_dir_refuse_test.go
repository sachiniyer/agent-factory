package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/stretchr/testify/require"
)

// #5172 slice 1: `tmux new-session -c <missing>` does not fail — it silently
// falls back to the server's current directory, so a restore or respawn of a
// session whose worktree was deleted launches its agent into an unrelated
// checkout or the daemon's $HOME. Every af spawn funnels through Start, so
// Start is the one seam that refuses a conclusively-missing directory. The
// stat-to-chdir race left — removal between the stat and the server's own -c
// resolution — is slice 2's job.

// spawnAttemptExec answers has-session with a determinate absence, then
// records whether new-session was ever issued — the fact these tests exist
// to pin.
func spawnAttemptExec(t *testing.T, name string, spawned *bool) cmd_test.MockCmdExec {
	t.Helper()
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if handled, err := answeredAbsent(t, c, name); handled {
				return err
			}
			if strings.Contains(c.String(), "new-session") {
				*spawned = true
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			return nil, errors.New("no server running")
		},
	}
}

func TestStartRefusesAMissingWorkDir(t *testing.T) {
	forceNewSessionEnvMarkers(t, true)
	name := toTmuxName("spawn-missing-dir", "")
	missing := filepath.Join(t.TempDir(), "deleted-worktree")

	var spawned bool
	session := newTmuxSession(name, "claude", NewMockPtyFactory(t),
		spawnAttemptExec(t, name, &spawned))

	err := session.Start(missing)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSpawnDirMissing)
	require.ErrorIs(t, err, ErrSessionNotStarted,
		"the refusal happens before new-session, so no pane can exist")
	require.Contains(t, err.Error(), name)
	require.Contains(t, err.Error(), missing)
	require.False(t, spawned, "a refused spawn must never issue new-session")
}

func TestStartRefusesAWorkDirThatIsNotADirectory(t *testing.T) {
	forceNewSessionEnvMarkers(t, true)
	name := toTmuxName("spawn-file-dir", "")
	file := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))

	var spawned bool
	session := newTmuxSession(name, "claude", NewMockPtyFactory(t),
		spawnAttemptExec(t, name, &spawned))

	err := session.Start(file)
	require.ErrorIs(t, err, ErrSpawnDirMissing)
	require.Contains(t, err.Error(), name)
	require.Contains(t, err.Error(), file)
	require.False(t, spawned)
}

// The restore respawn branch is the #5172 wound: a persisted session whose
// worktree was deleted while the tmux session was also gone used to re-spawn
// with -c <missing> and land in the server's cwd. The refusal names the
// session AND the path so the daemon's lost/rebuild routing has an actionable
// cause.
func TestRestoreRefusesRespawnIntoMissingWorktree(t *testing.T) {
	forceNewSessionEnvMarkers(t, true)
	name := toTmuxName("restore-missing-wt", "")
	missing := filepath.Join(t.TempDir(), "gone")

	var spawned bool
	session := newTmuxSession(name, "claude", NewMockPtyFactory(t),
		spawnAttemptExec(t, name, &spawned))

	res, err := session.RestoreWithResult(missing)
	require.Error(t, err)
	require.Equal(t, RestoreReattached, res,
		"a refused respawn is not a respawn — callers must not retire lost-state facts on it")
	require.Contains(t, err.Error(), name)
	require.Contains(t, err.Error(), missing)
	require.Contains(t, err.Error(), "unavailable")
	require.False(t, spawned, "the respawn must not reach new-session")
}

// The seam must not change the happy path: an existing directory still spawns.
// (The rest of Start's machinery is mocked out — this only asserts the
// admission check itself does not misfire on a real directory.)
func TestStartAdmitsAnExistingWorkDir(t *testing.T) {
	forceNewSessionEnvMarkers(t, true)
	name := toTmuxName("spawn-ok-dir", "")
	workDir := t.TempDir()

	var probed bool
	exec_ := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			s := c.String()
			switch {
			case strings.Contains(s, "has-session"):
				if !probed {
					probed = true
					return determinateAbsence(t, name)
				}
				return nil
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("no server running")
		},
	}
	session := newTmuxSession(name, "claude", NewMockPtyFactory(t), exec_)
	// The spawn proceeds past admission; whatever fails downstream is not
	// ErrSpawnDirMissing.
	if err := session.Start(workDir); err != nil {
		require.NotErrorIs(t, err, ErrSpawnDirMissing,
			"an existing directory must not be refused: %v", err)
	}
}
