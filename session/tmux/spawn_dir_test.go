package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
// answers) and is killed by kill-session, which flips *killed and makes every
// later probe report absent. fields maps each display-message format the
// post-spawn dir check may ask (pane_start_path, pane_pid, pane_current_path)
// to the answer a healthy tmux gives — a healthy tmux >= 3.4 records the -c it
// was handed as pane_start_path. A field absent from the map answers empty,
// tmux's "unsupported" expansion.
func liveAfterSpawnExec(t *testing.T, sessionName string, fields map[string]string, killed *bool) cmd_test.MockCmdExec {
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
			case strings.Contains(s, "display-message"):
				for field, answer := range fields {
					if answer != "" && strings.Contains(s, "#{"+field+"}") {
						return []byte(answer + "\n"), nil
					}
				}
				return nil, nil
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
		NewMockPtyFactory(t), liveAfterSpawnExec(t, "af_unverifiable-dir-retry",
			map[string]string{"pane_start_path": workDir}, &killed))
	require.NoError(t, session2.Start(workDir))
	assert.False(t, killed)
}

// TestStartTearsDownPaneInWrongDir is the belt-and-braces half of #5172, in
// the shape the #5174 play-test measured on real tmux 3.4: pane_start_path
// ECHOES the -c it was handed — the requested worktree — while the pane
// actually started in tmux's fallback cwd. Trusting the echo first means
// never asking where the pane is; the check must consult the kernel
// (/proc/<pane_pid>/cwd) or pane_current_path, see the real directory, and
// tear the session down. On the previous source order this test fails:
// the echo matches, the check returns early, and the misplaced pane lives.
func TestStartTearsDownPaneInWrongDir(t *testing.T) {
	workDir := t.TempDir()
	fallback := t.TempDir() // where tmux "fell back" to — any dir that is not workDir

	const pid = "424243"
	procRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(procRoot, pid), 0755))
	require.NoError(t, os.Symlink(fallback, filepath.Join(procRoot, pid, "cwd")))
	prev := procfsRoot
	procfsRoot = procRoot
	t.Cleanup(func() { procfsRoot = prev })

	var killed bool
	sessionName := toTmuxName("wrong-dir", "")
	ptyFactory := NewMockPtyFactory(t)
	session := newTmuxSession(sessionName, "claude", ptyFactory,
		liveAfterSpawnExec(t, sessionName, map[string]string{
			// What tmux >= 3.4 really answers: the request echoed back,
			// alongside the truth in the pane's live cwd.
			"pane_start_path":   workDir,
			"pane_pid":          pid,
			"pane_current_path": fallback,
		}, &killed))

	err := session.Start(workDir)

	require.Error(t, err)
	require.ErrorIs(t, err, ErrSpawnDirMissing)
	assert.True(t, killed, "a pane that landed outside its requested dir must be torn down")
	require.ErrorIs(t, err, ErrSessionNotStarted,
		"a conclusively killed pane means the launch left nothing running")
	assert.Len(t, ptyFactory.cmds, 1,
		"new-session DID run — the bug shape is a spawn tmux accepted and misplaced")
}

// TestStartTearsDownPaneInWrongDirViaFallback: on tmux < 3.4 pane_start_path
// expands empty and pane_pid may be unanswered too — #{pane_current_path}
// alone proves the pane landed outside the requested dir, and the session is
// still torn down with the same missing class.
func TestStartTearsDownPaneInWrongDirViaFallback(t *testing.T) {
	workDir := t.TempDir()
	fallback := t.TempDir()

	var killed bool
	sessionName := toTmuxName("wrong-dir-fallback", "")
	session := newTmuxSession(sessionName, "claude", NewMockPtyFactory(t),
		liveAfterSpawnExec(t, sessionName,
			map[string]string{"pane_current_path": fallback}, &killed))

	err := session.Start(workDir)

	require.Error(t, err)
	require.ErrorIs(t, err, ErrSpawnDirMissing)
	assert.True(t, killed, "a pane proven misplaced by any source must be torn down")
	require.ErrorIs(t, err, ErrSessionNotStarted)
}

// TestStartSucceedsWhenPaneDirMatches is the unchanged normal path: a spawn
// into an existing directory whose live cwd agrees — proven by the strongest
// available source, not by the pane_start_path echo — reports success and
// tears nothing down.
func TestStartSucceedsWhenPaneDirMatches(t *testing.T) {
	workDir := t.TempDir()

	const pid = "424244"
	procRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(procRoot, pid), 0755))
	require.NoError(t, os.Symlink(workDir, filepath.Join(procRoot, pid, "cwd")))
	prev := procfsRoot
	procfsRoot = procRoot
	t.Cleanup(func() { procfsRoot = prev })

	var killed bool
	sessionName := toTmuxName("right-dir", "")
	ptyFactory := NewMockPtyFactory(t)
	session := newTmuxSession(sessionName, "claude", ptyFactory,
		liveAfterSpawnExec(t, sessionName, map[string]string{
			"pane_start_path":   workDir, // the >= 3.4 echo
			"pane_pid":          pid,
			"pane_current_path": workDir,
		}, &killed))

	require.NoError(t, session.Start(workDir))
	assert.False(t, killed, "a correctly placed pane is never torn down")
	assert.Len(t, ptyFactory.cmds, 1)
}

// TestStartSucceedsWhenPaneStartPathEmpty is the tmux < 3.4 regression the
// #5174 review caught: pane_start_path expands to an EMPTY answer on tmux 3.2a
// (Ubuntu 22.04) and 3.3a (Debian 12), which the first cut read as "no usable
// start path" and tore every spawn down. An empty field means "this tmux
// cannot tell us" — the check falls through to a source every supported tmux
// has, here #{pane_current_path}, and the spawn succeeds.
func TestStartSucceedsWhenPaneStartPathEmpty(t *testing.T) {
	workDir := t.TempDir()

	var killed bool
	sessionName := toTmuxName("oldtmux-dir", "")
	ptyFactory := NewMockPtyFactory(t)
	session := newTmuxSession(sessionName, "claude", ptyFactory,
		liveAfterSpawnExec(t, sessionName,
			map[string]string{"pane_current_path": workDir}, &killed))

	require.NoError(t, session.Start(workDir),
		"an empty pane_start_path (tmux < 3.4) must not kill an otherwise good spawn")
	assert.False(t, killed)
	assert.Len(t, ptyFactory.cmds, 1)
}

// TestStartSucceedsViaProcCwd: on Linux the pane root's /proc/<pid>/cwd is the
// strongest source — the kernel's record of the pane's live cwd, proved here
// with a procfs fixture so the test does not depend on the host's process
// table. It is consulted before either tmux format field.
func TestStartSucceedsViaProcCwd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs fallback is Linux-only")
	}
	workDir := t.TempDir()
	const pid = "424242"
	procRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(procRoot, pid), 0755))
	require.NoError(t, os.Symlink(workDir, filepath.Join(procRoot, pid, "cwd")))

	prev := procfsRoot
	procfsRoot = procRoot
	t.Cleanup(func() { procfsRoot = prev })

	var killed bool
	sessionName := toTmuxName("proc-cwd-dir", "")
	ptyFactory := NewMockPtyFactory(t)
	session := newTmuxSession(sessionName, "claude", ptyFactory,
		liveAfterSpawnExec(t, sessionName,
			map[string]string{"pane_pid": pid}, &killed))

	require.NoError(t, session.Start(workDir),
		"the /proc/<pid>/cwd fallback proves the pane landed in the requested dir")
	assert.False(t, killed)
	assert.Len(t, ptyFactory.cmds, 1)
}

// TestStartSucceedsWhenNoPaneDirSource: when tmux will not report the pane's
// directory by ANY source — the whole post-check is skipped (logged once at
// INFO), never treated as a mismatch. Unavailability is not evidence.
func TestStartSucceedsWhenNoPaneDirSource(t *testing.T) {
	workDir := t.TempDir()

	var killed bool
	sessionName := toTmuxName("silent-dir", "")
	ptyFactory := NewMockPtyFactory(t)
	session := newTmuxSession(sessionName, "claude", ptyFactory,
		liveAfterSpawnExec(t, sessionName, map[string]string{}, &killed))

	require.NoError(t, session.Start(workDir),
		"a server that cannot report the pane dir must not kill the spawn")
	assert.False(t, killed, "nothing is torn down when no source can answer")
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

// TestStartSucceedsOnWedgedDirQuery: a display-message that errors entirely is
// silence, not a mismatch — the spawn survives with no teardown and no verdict.
func TestStartSucceedsOnWedgedDirQuery(t *testing.T) {
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
			case strings.Contains(s, "display-message"):
				return nil, errors.New("display-message exploded")
			case strings.Contains(s, "list-panes"):
				return nil, tmuxCantFindSessionError(t, sessionName)
			}
			return []byte("output"), nil
		},
	}
	ptyFactory := NewMockPtyFactory(t)
	session := newTmuxSession(sessionName, "claude", ptyFactory, cmdExec)

	require.NoError(t, session.Start(workDir),
		"unavailable information is never a reason to tear down")
	assert.False(t, killed, "an unanswered pane-dir query kills nothing")
}
