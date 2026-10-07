package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

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
				if *killed {
					return nil, tmuxCantFindSessionError(t, sessionName)
				}
				// Before the kill the session is live but the capture must
				// not stall a fake pane on the process table — an EMPTY list
				// answers truthfully "no panes to reap" (teardown_mark's
				// shape). Answering can't-find-session here instead makes the
				// wait-for-exit teardown report an inconclusive close, which
				// withholds ErrSessionNotStarted even though the pane died.
				return nil, nil
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

	const pid = "98765431"
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

	const pid = "98765433"
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
	const pid = "98765432"
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
// shape maps to exactly one verdict — usable, missing, or unknown — and a
// usable dir returns the FileInfo the post-spawn check will pin.
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
		// A file in an intermediate component stats ENOTDIR — the same
		// conclusive-missing verdict as ENOENT, not a retriable unknown.
		{"file as intermediate component", filepath.Join(realFile, "sub"), ErrSpawnDirMissing},
		{"unverifiable (EINVAL)", "/nonexistent\x00dir", ErrSpawnDirUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, err := checkSpawnDir(tc.path)
			if tc.wantErr == nil {
				require.NoError(t, err)
				require.NotNil(t, info, "an admitted dir hands its identity to the post-spawn check")
				require.True(t, info.IsDir())
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

// TestStartSucceedsWhenPaneInWorktreeSubdir: a process tab's command may
// chdir INSIDE its own worktree (`cd frontend && npm run dev`) before the
// post-spawn check reads — the live cwd then names a descendant of the
// admitted directory, which is still a pane inside its own tree. The match is
// by inode walk, so an exact-SameFile comparison (which would tear this down)
// is not the rule.
func TestStartSucceedsWhenPaneInWorktreeSubdir(t *testing.T) {
	workDir := t.TempDir()
	subdir := filepath.Join(workDir, "frontend")
	require.NoError(t, os.Mkdir(subdir, 0o755))

	var killed bool
	sessionName := toTmuxName("subdir-dir", "")
	ptyFactory := NewMockPtyFactory(t)
	session := newTmuxSession(sessionName, "claude", ptyFactory,
		liveAfterSpawnExec(t, sessionName,
			map[string]string{"pane_current_path": subdir}, &killed))

	require.NoError(t, session.Start(workDir),
		"a pane that chdir'd deeper into its own worktree is a correct spawn")
	assert.False(t, killed)
	assert.Len(t, ptyFactory.cmds, 1)
}

// TestStartTearsDownOnSiblingPrefixPath pins the inode rule's edge: a pane
// observed in /x/wt-evil must NOT match an admitted /x/wt. A path-prefix
// comparison would call wt-evil a descendant of wt — the ancestor walk
// compares stat'd inodes instead, so only real containment counts.
func TestStartTearsDownOnSiblingPrefixPath(t *testing.T) {
	base := t.TempDir()
	workDir := filepath.Join(base, "wt")
	require.NoError(t, os.Mkdir(workDir, 0o755))
	sibling := filepath.Join(base, "wt-evil")
	require.NoError(t, os.Mkdir(sibling, 0o755))

	var killed bool
	sessionName := toTmuxName("prefix-dir", "")
	ptyFactory := NewMockPtyFactory(t)
	session := newTmuxSession(sessionName, "claude", ptyFactory,
		liveAfterSpawnExec(t, sessionName,
			map[string]string{"pane_current_path": sibling}, &killed))

	err := session.Start(workDir)

	require.Error(t, err)
	require.ErrorIs(t, err, ErrSpawnDirMissing)
	assert.True(t, killed, "a prefix-sharing SIBLING dir is outside the admitted worktree")
}

// TestStartSucceedsWhenPanePathEndsInWhitespace: a directory whose final
// component legitimately ends in whitespace must stat the path tmux
// reported, not a whitespace-stripped different one. With TrimSpace the
// answer below would stat the ADJACENT dir (which exists precisely so the
// truncated path resolves) and tear the good spawn down on a fabricated
// mismatch.
func TestStartSucceedsWhenPanePathEndsInWhitespace(t *testing.T) {
	base := t.TempDir()
	workDir := filepath.Join(base, "spaced ")
	adjacent := filepath.Join(base, "spaced")
	require.NoError(t, os.Mkdir(workDir, 0o755))
	require.NoError(t, os.Mkdir(adjacent, 0o755),
		"the trimmed-truncation trap: this sibling exists so a stripped path still stats")

	var killed bool
	sessionName := toTmuxName("whitespace-dir", "")
	ptyFactory := NewMockPtyFactory(t)
	session := newTmuxSession(sessionName, "claude", ptyFactory,
		liveAfterSpawnExec(t, sessionName,
			map[string]string{"pane_current_path": workDir}, &killed))

	require.NoError(t, session.Start(workDir),
		"only the line terminator may be stripped from tmux's answer")
	assert.False(t, killed)
	assert.Len(t, ptyFactory.cmds, 1)
}

// TestStartTearsDownWhenWorktreeInodeSwapped: the comparison is pinned to the
// identity checkSpawnDir admitted, not to whatever the path resolves to later.
// If the worktree is renamed away and a fresh directory recreated at the same
// path between check and verify, a pane tmux reports at that path is in the
// NEW inode — a directory the spawn was never validated against — and is torn
// down. Re-statting the path (the previous check) would call it a match.
func TestStartTearsDownWhenWorktreeInodeSwapped(t *testing.T) {
	workDir := t.TempDir()

	var killed, swapped bool
	probed := 0
	sessionName := toTmuxName("inode-swap-dir", "")
	exec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			s := cmd.String()
			switch {
			case strings.Contains(s, "has-session"):
				probed++
				if killed || probed == 1 {
					return fmt.Errorf("can't find session")
				}
				// The first SUCCESSFUL has-session is the post-spawn
				// existence poll — after new-session, before the dir check:
				// swap the admitted inode out from under the path now, the
				// rename-and-recreate race this check is pinned against.
				if !swapped {
					swapped = true
					require.NoError(t, os.Rename(workDir, workDir+"-moved"))
					require.NoError(t, os.Mkdir(workDir, 0o755))
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
				if strings.Contains(s, "#{pane_current_path}") {
					return []byte(workDir + "\n"), nil
				}
				return nil, nil
			case strings.Contains(s, "list-panes"):
				return nil, tmuxCantFindSessionError(t, sessionName)
			}
			return []byte("output"), nil
		},
	}
	ptyFactory := NewMockPtyFactory(t)
	session := newTmuxSession(sessionName, "claude", ptyFactory, exec)

	err := session.Start(workDir)

	require.Error(t, err)
	require.ErrorIs(t, err, ErrSpawnDirMissing)
	assert.True(t, killed, "the pane landed in a different inode than the admitted one")
}

// TestStartTearDownWaitsForPaneExit: a proven-misplaced pane is killed AND
// waited out before ErrSessionNotStarted authorizes worktree cleanup —
// kill-session only delivers SIGHUP, so teardown must first ask the pane's
// pid (the multi-field paneRowFormat display-message that opens
// closeAndWaitForPaneExit) before issuing kill-session. Plain Close sends the
// kill without ever asking.
func TestStartTearDownWaitsForPaneExit(t *testing.T) {
	workDir := t.TempDir()
	fallback := t.TempDir()

	var killed, paneRowQueried bool
	probed := false
	sessionName := toTmuxName("wait-exit-dir", "")
	exec := cmd_test.MockCmdExec{
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
				assert.True(t, paneRowQueried,
					"the teardown must identify the pane (pane_pid row query) before kill-session")
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
				if strings.Contains(s, "#{pane_dead}") && strings.Contains(s, "#{pane_pid}") {
					// paneRowFormat — the panePID query opening
					// CloseAndWaitForPaneExit. The placement check's own
					// bare #{pane_dead} probe lacks pane_pid, so only the
					// teardown's row query can arm this flag.
					paneRowQueried = true
					return nil, nil
				}
				if strings.Contains(s, "#{pane_current_path}") {
					return []byte(fallback + "\n"), nil
				}
				return nil, nil
			case strings.Contains(s, "list-panes"):
				return nil, tmuxCantFindSessionError(t, sessionName)
			}
			return []byte("output"), nil
		},
	}
	ptyFactory := NewMockPtyFactory(t)
	session := newTmuxSession(sessionName, "claude", ptyFactory, exec)

	err := session.Start(workDir)

	require.Error(t, err)
	require.ErrorIs(t, err, ErrSpawnDirMissing)
	assert.True(t, killed)
	assert.True(t, paneRowQueried,
		"waiting for the pane means asking which process to wait on")
}

// TestStartSucceedsWhenProcCwdIsWorktreeSubdir: the proc source's SYMLINK
// must be resolved before the ancestor walk — os.Stat already follows it
// for the identity compare, but a walk over the LINK's own ancestors
// (/proc/<pid> → /proc → /) can never reach the worktree, so the strongest
// source was tearing down panes that pane_current_path would have accepted
// (#5174 review).
func TestStartSucceedsWhenProcCwdIsWorktreeSubdir(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs is Linux-only")
	}
	workDir := t.TempDir()
	subdir := filepath.Join(workDir, "frontend")
	require.NoError(t, os.Mkdir(subdir, 0o755))

	const pid = "98765435"
	procRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(procRoot, pid), 0o755))
	require.NoError(t, os.Symlink(subdir, filepath.Join(procRoot, pid, "cwd")))
	prev := procfsRoot
	procfsRoot = procRoot
	t.Cleanup(func() { procfsRoot = prev })

	var killed bool
	sessionName := toTmuxName("proc-subdir", "")
	session := newTmuxSession(sessionName, "claude", NewMockPtyFactory(t),
		liveAfterSpawnExec(t, sessionName,
			map[string]string{"pane_pid": pid}, &killed))

	require.NoError(t, session.Start(workDir),
		"a pane in a worktree SUBDIR proven by proc-cwd must be accepted")
	assert.False(t, killed)
}

// TestStartFallsThroughWhenProcCwdUnresolvable: procfs keeps answering stat
// on a pane cwd deleted after the spawn (readlink shows "<path> (deleted)"),
// but the path itself cannot be resolved — there is no ancestry to walk, so
// the observation is inconclusive and MUST fall through to the next source
// rather than tear down on the link's own ancestors (#5174 review).
func TestStartFallsThroughWhenProcCwdUnresolvable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs is Linux-only")
	}
	workDir := t.TempDir()
	otherDir := t.TempDir() // the inode the dangling cwd resolves to for stat

	const pid = "98765436"
	procRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(procRoot, pid), 0o755))
	require.NoError(t, os.Symlink(workDir+" (deleted)", filepath.Join(procRoot, pid, "cwd")))
	prev := procfsRoot
	procfsRoot = procRoot
	t.Cleanup(func() { procfsRoot = prev })

	// Real procfs stats a deleted-but-current cwd fine — the kernel resolves
	// the link to the unlinked inode — while its path no longer exists. The
	// fixture reproduces exactly that split: stat answers, EvalSymlinks fails.
	prevStat := statSpawnDir
	statSpawnDir = func(path string) (os.FileInfo, error) {
		if path == filepath.Join(procRoot, pid, "cwd") {
			return prevStat(otherDir)
		}
		return prevStat(path)
	}
	t.Cleanup(func() { statSpawnDir = prevStat })

	var killed bool
	sessionName := toTmuxName("proc-deleted", "")
	session := newTmuxSession(sessionName, "claude", NewMockPtyFactory(t),
		liveAfterSpawnExec(t, sessionName, map[string]string{
			"pane_pid":          pid,
			"pane_current_path": workDir,
		}, &killed))

	require.NoError(t, session.Start(workDir),
		"an unresolvable proc cwd is inconclusive — the next source decides")
	assert.False(t, killed)
}

// TestStartStopsDirQueriesOnServerTimeout: a display-message deadline is the
// SERVER not answering — paying tmuxCommandTimeout once per source (pane_pid,
// current_path, start_path ≈ 30s inside Start) stacks the same silence three
// times on the daemon's create path. The first timeout must stop the walk:
// exactly one pane-dir query may run (#5174 review).
func TestStartStopsDirQueriesOnServerTimeout(t *testing.T) {
	shortTmuxTimeout(t, 100*time.Millisecond)
	workDir := t.TempDir()

	var dirQueries atomic.Int32
	probed := false
	sessionName := toTmuxName("wedged-sources", "")
	exec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			s := cmd.String()
			switch {
			case strings.Contains(s, "has-session"):
				if !probed {
					probed = true
					return fmt.Errorf("can't find session")
				}
				return nil
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			s := cmd.String()
			switch {
			case strings.Contains(s, "show-options"):
				return nil, fmt.Errorf("no server running")
			case strings.Contains(s, "display-message") &&
				(strings.Contains(s, "#{pane_pid}") ||
					strings.Contains(s, "#{pane_current_path}") ||
					strings.Contains(s, "#{pane_start_path}")):
				dirQueries.Add(1)
				// Outlive the shrunken deadline: the ctx has already fired
				// by the time this returns, which is how a wedged server
				// presents to paneFormatField.
				time.Sleep(200 * time.Millisecond)
				return nil, nil
			}
			return []byte("output"), nil
		},
	}
	session := newTmuxSession(sessionName, "claude", NewMockPtyFactory(t), exec)

	require.NoError(t, session.Start(workDir),
		"a silent server skips the check — unavailability is never a verdict")
	assert.Equal(t, int32(1), dirQueries.Load(),
		"a wedged server gets one pane-dir deadline, not one per source")
}

// TestStartSkipsProcCwdForDeadPane: a remain-on-exit pane keeps reporting its
// ORIGINAL pane_pid after tmux marks it dead, and once reaped the kernel may
// recycle that pid — /proc/<pid>/cwd then names an UNRELATED process, which
// must not convict a correctly placed retained pane before
// pane_current_path answers (#5174 review).
func TestStartSkipsProcCwdForDeadPane(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs is Linux-only")
	}
	workDir := t.TempDir()
	foreign := t.TempDir() // the recycled pid's unrelated cwd

	const pid = "98765434"
	procRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(procRoot, pid), 0o755))
	require.NoError(t, os.Symlink(foreign, filepath.Join(procRoot, pid, "cwd")))
	prev := procfsRoot
	procfsRoot = procRoot
	t.Cleanup(func() { procfsRoot = prev })

	var killed bool
	probed := false
	sessionName := toTmuxName("dead-pane-proc", "")
	exec := cmd_test.MockCmdExec{
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
				// Bare single-field probes only: the pane-dead read, the
				// pane-pid read, and the current-path read are separate
				// commands, each matched on its own marker.
				switch {
				case strings.Contains(s, "#{pane_dead}") && !strings.Contains(s, "#{pane_pid}"):
					return []byte("1\n"), nil
				case strings.Contains(s, "#{pane_pid}") && !strings.Contains(s, "#{pane_dead}"):
					return []byte(pid + "\n"), nil
				case strings.Contains(s, "#{pane_current_path}"):
					return []byte(workDir + "\n"), nil
				}
				return nil, nil
			case strings.Contains(s, "list-panes"):
				return nil, nil
			}
			return []byte("output"), nil
		},
	}
	session := newTmuxSession(sessionName, "claude", NewMockPtyFactory(t), exec)

	require.NoError(t, session.Start(workDir),
		"a pane tmux reports dead must not be placed by a possibly-recycled pid")
	assert.False(t, killed,
		"procfs named a foreign directory only because the pid was recycled — nothing may be torn down")
}

// TestStartDoesNotConvictOnStartPathAlone: pane_start_path stores the -c tmux
// was handed, so it can only ever confirm the request. When it is the only
// answering source and it resolves OUTSIDE the admitted inode — the admitted
// directory was renamed-and-replaced, or a path symlink retargeted — the
// mismatch is about the stored name, not the pane, and nothing may be torn
// down (#5174 review).
func TestStartDoesNotConvictOnStartPathAlone(t *testing.T) {
	workDir := t.TempDir()
	replacement := t.TempDir() // resolves to a different inode than workDir

	var killed bool
	sessionName := toTmuxName("startpath-only", "")
	session := newTmuxSession(sessionName, "claude", NewMockPtyFactory(t),
		liveAfterSpawnExec(t, sessionName,
			map[string]string{"pane_start_path": replacement}, &killed))

	require.NoError(t, session.Start(workDir),
		"an echo-source mismatch is inconclusive — never a teardown")
	assert.False(t, killed)
}

// TestStartAcceptsPaneWhenWorktreePathRenamedMidSpawn covers the review gap:
// a rename/unlink of the admitted directory between checkSpawnDir and the
// post-spawn re-stat leaves the PATHNAME dead but the INODE live — the pane
// entered the real directory and stays correctly placed by identity. Only a
// positive source verdict may convict it; the dead path alone cannot (#5174).
func TestStartAcceptsPaneWhenWorktreePathRenamedMidSpawn(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "admitted")
	renamed := filepath.Join(filepath.Dir(workDir), "renamed")
	require.NoError(t, os.MkdirAll(workDir, 0755))

	var killed bool
	sessionName := toTmuxName("renamed-mid-spawn", "")
	inner := liveAfterSpawnExec(t, sessionName, map[string]string{
		// The pane's live cwd is the renamed directory — the same inode the
		// spawn was admitted on.
		"pane_current_path": renamed,
	}, &killed)
	probed, renamedOnce := false, false
	exec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			// The rename lands INSIDE the spawn seam: new-session travels the
			// pty factory, so the first POST-spawn has-session — after
			// checkSpawnDir admitted the path, before the post-spawn re-stat —
			// is where the pathname dies.
			if strings.Contains(cmd.String(), "has-session") && probed && !killed && !renamedOnce {
				renamedOnce = true
				require.NoError(t, os.Rename(workDir, renamed))
			}
			if strings.Contains(cmd.String(), "has-session") {
				probed = true
			}
			return inner.Run(cmd)
		},
		OutputFunc: inner.Output,
	}
	session := newTmuxSession(sessionName, "claude", NewMockPtyFactory(t), exec)

	require.NoError(t, session.Start(workDir),
		"a pane holding the admitted inode stays bound even though the path it entered under is gone")
	assert.False(t, killed, "a correctly placed pane is never torn down")
}
