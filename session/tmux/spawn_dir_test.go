package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// absentSessionExec answers every has-session with "not found": the session
// never exists server-side, so a Start must reach new-session to create it.
// The refusal tests assert it never gets that far.
func absentSessionExec() cmd_test.MockCmdExec {
	return cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "has-session") {
				return fmt.Errorf("can't find session")
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return []byte("output"), nil },
	}
}

// liveAfterSpawnExec models a server where the session appears by the time the
// existence poll probes it (has-session fails on the pre-spawn probe, then
// answers), reports paneStart on the post-spawn #{pane_start_path} query — the
// answer a healthy tmux records for the -c it was handed — and is killed by
// kill-session, which flips *killed and makes every later probe report absent.
func liveAfterSpawnExec(t *testing.T, sessionName, paneStart string, killed *bool) cmd_test.MockCmdExec {
	t.Helper()
	probed := false
	return cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			s := cmd.String()
			switch {
			case strings.Contains(s, "has-session"):
				if *killed || !probed {
					probed = true
					return fmt.Errorf("can't find session")
				}
				return nil
			case strings.Contains(s, "kill-session"):
				*killed = true
				return nil
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			s := cmd.String()
			switch {
			case strings.Contains(s, "show-options"):
				return nil, fmt.Errorf("no server running")
			case strings.Contains(s, "pane_start_path"):
				return []byte(paneStart + "\n"), nil
			case strings.Contains(s, "list-panes"):
				return nil, tmuxCantFindSessionError(t, sessionName)
			}
			return []byte("output"), nil
		},
	}
}

// TestStartRefusesMissingWorkDir is the #5172 refusal: a spawn whose start
// directory does not exist must never reach new-session — tmux would fall back
// to the server's cwd and start the pane in the daemon's own directory.
func TestStartRefusesMissingWorkDir(t *testing.T) {
	ptyFactory := NewMockPtyFactory(t)
	session := newTmuxSession(toTmuxName("missing-dir", ""), "claude", ptyFactory, absentSessionExec())

	err := session.Start(filepath.Join(t.TempDir(), "gone"))

	require.Error(t, err)
	require.ErrorIs(t, err, ErrSpawnDirMissing)
	require.ErrorIs(t, err, ErrSessionNotStarted,
		"a refused spawn never launched a pane, the same cleanup authority as any unstarted launch")
	require.ErrorIs(t, err, os.ErrNotExist,
		"the ENOENT in the chain is what the daemon's WORKTREE_MISSING classification keys on")
	assert.Empty(t, ptyFactory.cmds, "new-session must never run for a missing start dir")
}

// TestStartRefusesEmptyWorkDir: an empty -c asks tmux for its default — the
// server's cwd — the exact fallback this seam exists to forbid.
func TestStartRefusesEmptyWorkDir(t *testing.T) {
	ptyFactory := NewMockPtyFactory(t)
	session := newTmuxSession(toTmuxName("empty-dir", ""), "claude", ptyFactory, absentSessionExec())

	err := session.Start("")

	require.Error(t, err)
	require.ErrorIs(t, err, ErrSpawnDirMissing)
	require.ErrorIs(t, err, ErrSessionNotStarted)
	assert.Empty(t, ptyFactory.cmds)
}

// TestStartRefusesUnverifiableWorkDir: a stat error that is not ENOENT is not
// evidence the worktree is gone — the spawn is refused WITHOUT the missing
// verdict, so the caller holds the row and retries rather than declaring it
// lost. The second half proves a later attempt against the same directory
// succeeds once the stat answers.
func TestStartRefusesUnverifiableWorkDir(t *testing.T) {
	workDir := t.TempDir()

	prev := statSpawnDir
	statSpawnDir = func(path string) (os.FileInfo, error) {
		return nil, &os.PathError{Op: "stat", Path: path, Err: syscall.EPERM}
	}
	t.Cleanup(func() { statSpawnDir = prev })

	ptyFactory := NewMockPtyFactory(t)
	session := newTmuxSession(toTmuxName("unverifiable-dir", ""), "claude", ptyFactory, absentSessionExec())

	err := session.Start(workDir)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSpawnDirUnknown)
	require.ErrorIs(t, err, ErrSessionNotStarted)
	require.NotErrorIs(t, err, ErrSpawnDirMissing,
		"an unproven stat must not read as a missing worktree")
	require.NotErrorIs(t, err, os.ErrNotExist)
	assert.Empty(t, ptyFactory.cmds, "no spawn while the directory state is unproven")

	// The row is held for a later pass: once the stat answers, the same
	// directory spawns normally.
	statSpawnDir = prev
	var killed bool
	session2 := newTmuxSession(toTmuxName("unverifiable-dir-retry", ""), "claude",
		NewMockPtyFactory(t), liveAfterSpawnExec(t, "af_unverifiable-dir-retry", workDir, &killed))
	require.NoError(t, session2.Start(workDir))
	assert.False(t, killed)
}

// TestStartTearsDownPaneInWrongDir is the belt-and-braces half of #5172: even
// when new-session reports success, a pane whose recorded start path is not
// the requested directory — tmux fell back to the server's cwd — is killed,
// and the outcome reports the same missing class as a refused spawn.
func TestStartTearsDownPaneInWrongDir(t *testing.T) {
	workDir := t.TempDir()
	fallback := t.TempDir() // where tmux "fell back" to — any dir that is not workDir

	var killed bool
	sessionName := toTmuxName("wrong-dir", "")
	ptyFactory := NewMockPtyFactory(t)
	session := newTmuxSession(sessionName, "claude", ptyFactory,
		liveAfterSpawnExec(t, sessionName, fallback, &killed))

	err := session.Start(workDir)

	require.Error(t, err)
	require.ErrorIs(t, err, ErrSpawnDirMissing)
	assert.True(t, killed, "a pane that landed outside its requested dir must be torn down")
	require.ErrorIs(t, err, ErrSessionNotStarted,
		"a conclusively killed pane means the launch left nothing running")
	assert.Len(t, ptyFactory.cmds, 1,
		"new-session DID run — the bug shape is a spawn tmux accepted and misplaced")
}

// TestStartSucceedsWhenPaneDirMatches is the unchanged normal path: a spawn
// into an existing directory whose pane records that same directory reports
// success and tears nothing down.
func TestStartSucceedsWhenPaneDirMatches(t *testing.T) {
	workDir := t.TempDir()

	var killed bool
	sessionName := toTmuxName("right-dir", "")
	ptyFactory := NewMockPtyFactory(t)
	session := newTmuxSession(sessionName, "claude", ptyFactory,
		liveAfterSpawnExec(t, sessionName, workDir, &killed))

	require.NoError(t, session.Start(workDir))
	assert.False(t, killed, "a correctly placed pane is never torn down")
	assert.Len(t, ptyFactory.cmds, 1)
}

// TestCheckSpawnDirClassifications pins the table directly: every start-dir
// shape maps to exactly one verdict — usable, missing, or unknown.
func TestCheckSpawnDirClassifications(t *testing.T) {
	realDir := t.TempDir()
	realFile := filepath.Join(t.TempDir(), "a-file")
	require.NoError(t, os.WriteFile(realFile, []byte("x"), 0o600))

	for _, tc := range []struct {
		name    string
		path    string
		wantErr error
	}{
		{"usable dir", realDir, nil},
		{"empty", "", ErrSpawnDirMissing},
		{"absent", filepath.Join(t.TempDir(), "gone"), ErrSpawnDirMissing},
		{"not a directory", realFile, ErrSpawnDirMissing},
		{"unverifiable (EINVAL)", "/nonexistent\x00dir", ErrSpawnDirUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkSpawnDir(tc.path)
			if tc.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.wantErr)
			require.ErrorIs(t, err, ErrSessionNotStarted)
			if tc.wantErr == ErrSpawnDirMissing {
				require.ErrorIs(t, err, os.ErrNotExist)
			} else {
				require.NotErrorIs(t, err, os.ErrNotExist)
			}
		})
	}
}

// TestVerifySpawnedPaneDirUnknownOnWedgedServer: when tmux will not answer the
// start-path query at all, the pane's dir is unknown — still torn down, still
// not a missing-worktree verdict.
func TestVerifySpawnedPaneDirUnknownOnWedgedServer(t *testing.T) {
	workDir := t.TempDir()

	var killed bool
	sessionName := toTmuxName("wedged-dir", "")
	probed := false
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			s := cmd.String()
			switch {
			case strings.Contains(s, "has-session"):
				if killed || !probed {
					probed = true
					return fmt.Errorf("can't find session")
				}
				return nil
			case strings.Contains(s, "kill-session"):
				killed = true
				return nil
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			s := cmd.String()
			switch {
			case strings.Contains(s, "show-options"):
				return nil, fmt.Errorf("no server running")
			case strings.Contains(s, "pane_start_path"):
				return nil, errors.New("display-message exploded")
			case strings.Contains(s, "list-panes"):
				return nil, tmuxCantFindSessionError(t, sessionName)
			}
			return []byte("output"), nil
		},
	}
	ptyFactory := NewMockPtyFactory(t)
	session := newTmuxSession(sessionName, "claude", ptyFactory, cmdExec)

	err := session.Start(workDir)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSpawnDirUnknown)
	require.NotErrorIs(t, err, ErrSpawnDirMissing,
		"an unreadable answer is not proof the worktree is missing")
	assert.True(t, killed, "an unverifiable pane is still torn down")
}
