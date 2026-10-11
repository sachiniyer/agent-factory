package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStartPreservesFinishedPaneAcrossPostExitPermissionChange is the #5174
// review fix for the dead arm's verdict source: a pane tmux marked dead
// before verification has no live evidence, but placement is already proven
// by the spawn itself — the exec shim chdirs into the admitted directory at
// the instant the command would run and stamps a refusal (exit code +
// scrollback marker) when it cannot. The refusal channel answering with NO
// refusal is the only post-exit proof that exists: a second point-in-time
// accessibility sample — here, the worktree losing its execute bit after the
// pane exited — describes the verification instant, not the spawn, and must
// never convict a pane the shim proved entered (#5174 review).
func TestStartPreservesFinishedPaneAcrossPostExitPermissionChange(t *testing.T) {
	workDir := t.TempDir()

	// Enterable at admission, denied by the time the walk finishes — the
	// post-exit flap the retired second sample would have read as a
	// historical spawn failure.
	var accessCalls atomic.Int32
	prevAccess := spawnDirEnterable
	spawnDirEnterable = func(string) error {
		if accessCalls.Add(1) == 1 {
			return nil
		}
		return syscall.EACCES
	}
	t.Cleanup(func() { spawnDirEnterable = prevAccess })

	var killed, probed bool
	sessionName := toTmuxName("dead-pane-silent", "")
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
				switch {
				case strings.Contains(s, "#{pane_dead}") && strings.Contains(s, "#{pane_pid}"):
					// paneRowFormat — teardown's panePID.
					return []byte("98765|1|\n"), nil
				case strings.Contains(s, "#{pane_dead}"):
					return []byte("1\n"), nil // affirmatively dead
				case strings.Contains(s, "#{pane_dead_status}"):
					return []byte("0\n"), nil // the shim's channel answered: command ran
				case strings.Contains(s, "#{pane_start_path}"):
					return []byte(workDir + "\n"), nil
				}
				// pane_current_path stays silent: tmux expands it empty for
				// a dead pane by design.
				return nil, nil
			case strings.Contains(s, "list-panes"):
				return nil, nil
			}
			return []byte("output"), nil
		},
	}
	session := newTmuxSession(sessionName, "claude", NewMockPtyFactory(t), exec)

	require.NoError(t, session.Start(workDir),
		"a post-exit accessibility change cannot convict a pane the shim proved entered")
	assert.False(t, killed,
		"a pane that provably ran inside the admitted directory is never torn down")
	assert.EqualValues(t, 1, accessCalls.Load(),
		"the admission check is the only enterability sample — no post-exit re-sample")
}

// TestStartAcceptsFinishedPaneOverEnterableDir is the other half of the
// dead-pane verdict — and what keeps the finished-command surface alive: a
// pane that exited before verification carries no live evidence, but needs
// none when the pathname verdict is affirmative. The admitted inode still
// answers the path and can still be entered, and chdir is tmux's only
// fallback trigger — it could not have failed on that directory, so the
// finished pane provably ran inside the worktree. The spawn succeeds and
// the retained pane's exit stays for the caller's finish surface to read
// (#5174 review).
func TestStartAcceptsFinishedPaneOverEnterableDir(t *testing.T) {
	workDir := t.TempDir()

	var killed bool
	sessionName := toTmuxName("dead-pane-enterable", "")
	session := newTmuxSession(sessionName, "claude", NewMockPtyFactory(t),
		liveAfterSpawnExec(t, sessionName, map[string]string{
			"pane_dead":       "1",
			"pane_start_path": workDir,
		}, &killed))

	require.NoError(t, session.Start(workDir),
		"a finished pane over the still-enterable admitted inode provably ran inside it")
	assert.False(t, killed, "a pane that provably landed is never torn down")
}

// TestStartWithholdsNotStartedAfterBlindTeardown is the #5174 review fix for
// the cleanup-authorizing sentinel on the misplaced-pane teardown: when the
// misplaced pane exits between the placement read and the teardown's
// pane-PID capture, CloseAndWaitForPaneExit reports PaneStateKnown while its
// ancestry scan ran blind — on a session that never exported the AF_SESSION
// marker that sweep is vacuous, so a detached descendant can still hold the
// worktree. The misplaced verdict still reports, but the error must NOT
// carry ErrSessionNotStarted — the marker that authorizes deleting the tree.
func TestStartWithholdsNotStartedAfterBlindTeardown(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	workDir := t.TempDir()
	fallback := t.TempDir()

	var killed, probed, gone bool
	sessionName := toTmuxName("blind-teardown", "")
	exec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			s := cmd.String()
			switch {
			case strings.Contains(s, "has-session"):
				if killed || gone || !probed {
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
				switch {
				case strings.Contains(s, "#{pane_dead}") && strings.Contains(s, "#{pane_pid}"):
					// paneRowFormat — teardown's panePID. The pane exited
					// between the placement read and this capture: exit-0
					// empty is tmux's missing-session answer, the
					// errPaneQueryFoundNoPane half of the blind verdict.
					gone = true
					return nil, nil
				case strings.Contains(s, "#{pane_dead}"):
					return []byte("0\n"), nil // affirmatively live at verify time
				case strings.Contains(s, "#{pane_current_path}"):
					return []byte(fallback + "\n"), nil // positive placement mismatch
				}
				return nil, nil
			case strings.Contains(s, "list-panes"):
				// tmux's real missing-session answer — the
				// ErrSessionVanishedBeforeCapture half of blind.
				return nil, tmuxCantFindSessionError(t, sessionName)
			}
			return []byte("output"), nil
		},
	}
	session := newTmuxSession(sessionName, "claude", NewMockPtyFactory(t), exec)

	err := session.Start(workDir)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSpawnDirMissing,
		"the misplaced-pane verdict still reports")
	require.NotErrorIs(t, err, ErrSessionNotStarted,
		"a teardown that never observed a pane cannot authorize deleting the worktree — "+
			"a detached descendant may still hold it")
}

// TestStartTearsDownOnShimDirRefusal is the spawn-tied half of the #5174
// dead-pane verdict — and the exact scenario the review named: enterability
// removed after admission, restored before this check could sample it. A
// point-in-time Access re-check can only describe the verification instant,
// so this directory answers enterable NOW — yet pane_dead_status carries the
// shim's own exit code AND its scrollback the refusal marker, both recorded
// at the instant the command would have run: proof the pane never ran inside
// the worktree. The refused pane is torn down, never recorded as a clean
// spawn.
func TestStartTearsDownOnShimDirRefusal(t *testing.T) {
	workDir := t.TempDir()

	var killed bool
	sessionName := toTmuxName("shim-refusal", "")
	session := newTmuxSession(sessionName, "claude", NewMockPtyFactory(t),
		liveAfterSpawnExec(t, sessionName, map[string]string{
			"pane_dead":        "1",
			"pane_dead_status": fmt.Sprintf("%d", sessionenv.SpawnDirRefusalExitCode),
			"pane_start_path":  workDir,
			"capture-pane":     sessionenv.SpawnDirRefusalMarker + ": chdir /x: permission denied\n",
		}, &killed))

	err := session.Start(workDir)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSpawnDirMissing)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.ErrorIs(t, err, ErrSessionNotStarted,
		"a conclusively torn-down refusal means the launch left nothing running")
	assert.True(t, killed,
		"a pane the shim could not place is torn down, not kept as a finished tab")
}

// TestStartAcceptsSameExitCodeWithoutMarker: pane_dead_status carrying the
// shim's own exit code is NOT alone a refusal verdict — a command that
// legitimately exits with that number produces the same field. The scrollback
// marker is the corroborating fact a real refusal always has: without it the
// pane is a finished command, kept for the caller's finish surface.
func TestStartAcceptsSameExitCodeWithoutMarker(t *testing.T) {
	workDir := t.TempDir()

	var killed bool
	sessionName := toTmuxName("exit-same-code", "")
	session := newTmuxSession(sessionName, "claude", NewMockPtyFactory(t),
		liveAfterSpawnExec(t, sessionName, map[string]string{
			"pane_dead":        "1",
			"pane_dead_status": fmt.Sprintf("%d", sessionenv.SpawnDirRefusalExitCode),
			"capture-pane":     "the command's own output\n",
		}, &killed))

	require.NoError(t, session.Start(workDir),
		"a bare matching exit code is not the refusal — the marker is")
	assert.False(t, killed)
}

// TestStartTearsDownOnShimRefusalMarker: where pane_dead_status expands
// empty — a tmux that does not know the field, or a pane killed by a signal —
// the same verdict survives in the dead pane's scrollback: the shim's refusal
// line is the only content a refused pane ever held.
func TestStartTearsDownOnShimRefusalMarker(t *testing.T) {
	workDir := t.TempDir()

	var killed bool
	sessionName := toTmuxName("shim-refusal-marker", "")
	session := newTmuxSession(sessionName, "claude", NewMockPtyFactory(t),
		liveAfterSpawnExec(t, sessionName, map[string]string{
			"pane_dead": "1",
			// pane_dead_status deliberately absent — the empty-answer path.
			"capture-pane": sessionenv.SpawnDirRefusalMarker + ": chdir /x: permission denied\n",
		}, &killed))

	err := session.Start(workDir)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSpawnDirMissing)
	assert.True(t, killed, "the scrollback refusal verdict convicts the same way")
}

// TestStartAcceptsSignalKilledPaneOverEnterableDir is the marker check's
// negative half — and what keeps a signaled command's finish surface alive:
// pane_dead_status is ALSO empty for a pane tmux killed by signal, so the
// scrollback is the only way to tell "the shim refused to enter" from "the
// command ran inside and was signaled". No marker + the admitted inode still
// answering the path enterably = the pane provably ran inside; the spawn
// succeeds and its partial output stays for the finish surface.
func TestStartAcceptsSignalKilledPaneOverEnterableDir(t *testing.T) {
	workDir := t.TempDir()

	var killed bool
	sessionName := toTmuxName("signal-killed", "")
	session := newTmuxSession(sessionName, "claude", NewMockPtyFactory(t),
		liveAfterSpawnExec(t, sessionName, map[string]string{
			"pane_dead":    "1",
			"capture-pane": "partial output before the kill\n",
		}, &killed))

	require.NoError(t, session.Start(workDir),
		"a dead pane with no refusal verdict and an enterable admitted inode provably ran inside")
	assert.False(t, killed)
}

// deadFlipExec is liveAfterSpawnExec with a pane_dead that changes mid-walk:
// single-field #{pane_dead} queries pop deadAnswers in order (the last is
// reused once the queue runs dry), modelling a short remain-on-exit command
// that is live when the verifier opens and dead by the time its evidence is
// consumed (#5174 review). The teardown's combined pane_pid|pane_dead row
// answers from the LAST state the walk observed, so a verdict and the kill
// it drives see one consistent pane.
func deadFlipExec(t *testing.T, sessionName string, deadAnswers []string, fields map[string]string, killed *bool) cmd_test.MockCmdExec {
	t.Helper()
	probed := false
	idx := 0
	deadNow := func() string {
		if len(deadAnswers) == 0 {
			return fields["pane_dead"]
		}
		answer := deadAnswers[min(idx, len(deadAnswers)-1)]
		idx++
		return answer
	}
	deadSeen := func() string {
		if len(deadAnswers) == 0 {
			return fields["pane_dead"]
		}
		return deadAnswers[min(idx, len(deadAnswers)-1)]
	}
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
				if strings.Contains(s, "#{pane_pid}") && strings.Contains(s, "#{pane_dead}") {
					return []byte("98765|" + deadSeen() + "|" + fields["pane_dead_status"] + "\n"), nil
				}
				if strings.Contains(s, "#{pane_dead}") {
					return []byte(deadNow() + "\n"), nil
				}
				for field, answer := range fields {
					if answer != "" && strings.Contains(s, "#{"+field+"}") {
						return []byte(answer + "\n"), nil
					}
				}
				return nil, nil
			case strings.Contains(s, "capture-pane"):
				return []byte(fields["capture-pane"]), nil
			case strings.Contains(s, "list-panes"):
				if *killed {
					return nil, tmuxCantFindSessionError(t, sessionName)
				}
				return nil, nil
			}
			return []byte("output"), nil
		},
	}
}

// TestStartRevalidatesDeathBeforeEchoConfirms is the confirm-only half of the
// stale-live-bit finding: pane_dead answers "0" at the walk's open, procfs
// and pane_current_path go silent as the pane exits, and pane_start_path —
// the request's own echo — is the only answer still standing. A stale
// paneLive would let that echo approve itself; the revalidation sees the
// fresh dead mark instead, drops to the dead arm, and the shim's recorded
// refusal convicts (#5174 review).
func TestStartRevalidatesDeathBeforeEchoConfirms(t *testing.T) {
	workDir := t.TempDir()

	var killed bool
	sessionName := toTmuxName("midwalk-dead-echo", "")
	session := newTmuxSession(sessionName, "claude", NewMockPtyFactory(t),
		deadFlipExec(t, sessionName, []string{"0", "1"}, map[string]string{
			"pane_pid":        "99999999", // no /proc entry — procfs goes silent
			"pane_start_path": workDir,    // the request's own echo
			// The dead arm's convict: the shim's own refusal channel —
			// the spawn-tied evidence the echo could never produce.
			"pane_dead_status": strconv.Itoa(sessionenv.SpawnDirRefusalExitCode),
			"capture-pane":     sessionenv.SpawnDirRefusalMarker + ": chdir /x: permission denied\n",
		}, &killed))

	err := session.Start(workDir)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSpawnDirMissing,
		"a pane dead by verification is judged on the shim's refusal channel — the echo must not approve it")
	assert.True(t, killed)
}

// TestStartRevalidatesDeathBeforeSilenceAccepts is the no-source half: every
// placement source answers empty, and the pane_dead bit cached at the walk's
// open still reads live. The fresh re-probe finds the dead mark, so the
// verdict reaches the dead arm — which convicts on the shim's refusal
// channel — instead of the silence-accept the stale bit would have taken
// (#5174 review).
func TestStartRevalidatesDeathBeforeSilenceAccepts(t *testing.T) {
	workDir := t.TempDir()

	var killed bool
	sessionName := toTmuxName("midwalk-dead-silent", "")
	session := newTmuxSession(sessionName, "claude", NewMockPtyFactory(t),
		deadFlipExec(t, sessionName, []string{"0", "1"}, map[string]string{
			"pane_pid": "", // paneLive makes proc-cwd ask, but nothing answers
			// See the echo half — the dead arm convicts on the shim's
			// recorded refusal, never on later accessibility samples.
			"pane_dead_status": strconv.Itoa(sessionenv.SpawnDirRefusalExitCode),
			"capture-pane":     sessionenv.SpawnDirRefusalMarker + ": chdir /x: permission denied\n",
		}, &killed))

	err := session.Start(workDir)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSpawnDirMissing,
		"the no-source path must decide on the fresh dead mark, not silence under a stale live bit")
	assert.True(t, killed)
}

// TestStartAcceptsEchoUnderLiveRevalidation is the companion accept: the
// re-probe proves the pane still live, so pane_start_path's matching echo
// may confirm — a live pane whose procfs read raced remains a correct spawn.
func TestStartAcceptsEchoUnderLiveRevalidation(t *testing.T) {
	workDir := t.TempDir()

	var killed bool
	sessionName := toTmuxName("midwalk-live-echo", "")
	session := newTmuxSession(sessionName, "claude", NewMockPtyFactory(t),
		deadFlipExec(t, sessionName, []string{"0", "0"}, map[string]string{
			"pane_pid":        "99999999",
			"pane_start_path": workDir,
		}, &killed))

	require.NoError(t, session.Start(workDir),
		"a fresh live proof lets the matching echo confirm")
	assert.False(t, killed)
}

// TestPaneDirInsideStatFailureIsInconclusive is the unit half of the
// ancestor-walk finding: a mid-walk stat failure must not collapse into a
// resolvable "outside" verdict — the failed component may be the admitted
// inode itself (#5174 review).
func TestPaneDirInsideStatFailureIsInconclusive(t *testing.T) {
	workDir := t.TempDir()
	sub := filepath.Join(workDir, "sub")
	require.NoError(t, os.Mkdir(sub, 0755))
	sibling := t.TempDir()

	want, err := statSpawnDir(workDir)
	require.NoError(t, err)

	resolvedWorkDir, err := filepath.EvalSymlinks(workDir)
	require.NoError(t, err)

	prev := statSpawnDir
	statSpawnDir = func(path string) (os.FileInfo, error) {
		if path == resolvedWorkDir {
			return nil, &os.PathError{Op: "stat", Path: path, Err: syscall.ENOENT}
		}
		return prev(path)
	}
	t.Cleanup(func() { statSpawnDir = prev })

	inside, resolvable := paneDirInside(sub, want)
	assert.False(t, inside)
	assert.False(t, resolvable,
		"a walk that could not stat the admitted directory cannot conclude outside")

	// A path whose ancestry never crosses the failed component still resolves
	// its outside verdict — the failure downgrades only the walks it touches.
	inside, resolvable = paneDirInside(sibling, want)
	assert.False(t, inside)
	assert.True(t, resolvable)
}

// TestStartDoesNotConvictOnUnwalkableObservation is the Start-level half: the
// observed pane_current_path resolves under the admitted directory but a
// mid-walk stat fails, so no source can conclusively place it — inconclusive
// evidence tears nothing down (#5174 review).
func TestStartDoesNotConvictOnUnwalkableObservation(t *testing.T) {
	// The requested directory arrives through a symlink so the failing stat
	// hits only the RESOLVED path the ancestor walk produces — admission and
	// the no-source re-stat ask for the link itself and must still answer.
	realWt := t.TempDir()
	sub := filepath.Join(realWt, "sub")
	require.NoError(t, os.Mkdir(sub, 0755))
	workDir := filepath.Join(t.TempDir(), "wt-link")
	require.NoError(t, os.Symlink(realWt, workDir))

	resolvedSub, err := filepath.EvalSymlinks(sub)
	require.NoError(t, err)
	resolvedWorkDir, err := filepath.EvalSymlinks(workDir)
	require.NoError(t, err)

	prev := statSpawnDir
	statSpawnDir = func(path string) (os.FileInfo, error) {
		if path == resolvedWorkDir {
			return nil, &os.PathError{Op: "stat", Path: path, Err: syscall.ENOENT}
		}
		return prev(path)
	}
	t.Cleanup(func() { statSpawnDir = prev })

	var killed bool
	sessionName := toTmuxName("unwalkable-obs", "")
	session := newTmuxSession(sessionName, "claude", NewMockPtyFactory(t),
		liveAfterSpawnExec(t, sessionName, map[string]string{
			"pane_dead":         "0",
			"pane_pid":          "99999999",
			"pane_current_path": resolvedSub,
		}, &killed))

	require.NoError(t, session.Start(workDir),
		"a stat failure inside the ancestry walk is inconclusive, never an outside verdict")
	assert.False(t, killed)
}

// TestStartDiscardsProcCwdWhenPaneDiesMidWalk is the #5174 review fix for
// the procfs source's stale-liveness window: pane_dead answers "0" when the
// walk opens, so /proc/<pane_pid>/cwd is read — and shows a directory
// OUTSIDE the worktree, exactly what a recycled pid's owner would report.
// Before the answer may convict, pane_dead is re-read: the pane exited
// between the two samples, so the procfs record is a stranger's and must be
// discarded. The verdict falls to the dead arm, where the shim's channel
// (status "0", no refusal) proves the pane entered the admitted directory
// (#5174 review).
func TestStartDiscardsProcCwdWhenPaneDiesMidWalk(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs fallback is Linux-only")
	}
	workDir := t.TempDir()
	strangerDir := t.TempDir() // the recycled pid's real cwd — outside workDir

	const pid = "98765001"
	procRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(procRoot, pid), 0755))
	require.NoError(t, os.Symlink(strangerDir, filepath.Join(procRoot, pid, "cwd")))
	prev := procfsRoot
	procfsRoot = procRoot
	t.Cleanup(func() { procfsRoot = prev })

	var killed bool
	sessionName := toTmuxName("proc-died-midwalk", "")
	session := newTmuxSession(sessionName, "claude", NewMockPtyFactory(t),
		deadFlipExec(t, sessionName, []string{"0", "1"}, map[string]string{
			"pane_pid":         pid,
			"pane_dead_status": "0", // the shim's channel: command ran, no refusal
		}, &killed))

	require.NoError(t, session.Start(workDir),
		"a procfs cwd read after the pane's death names a recycled pid, never a verdict")
	assert.False(t, killed)
}

// TestStartHoldsFinishedPaneWhenRefusalChannelUnread is the inconclusive
// half of the shim-verdict arm: pane_dead_status expands empty AND the
// scrollback capture fails — nothing then established whether the shim
// passed or refused, so the dead pane is held with an Unknown verdict
// instead of accepted on silence or torn down on a guess (#5174 review).
func TestStartHoldsFinishedPaneWhenRefusalChannelUnread(t *testing.T) {
	workDir := t.TempDir()

	var killed, probed bool
	sessionName := toTmuxName("dead-channel-unread", "")
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
				switch {
				case strings.Contains(s, "#{pane_dead}") && strings.Contains(s, "#{pane_pid}"):
					return []byte("98765|1|\n"), nil
				case strings.Contains(s, "#{pane_dead}"):
					return []byte("1\n"), nil
				}
				// pane_dead_status and everything else: this tmux does not
				// answer — the refusal channel's status half is unread.
				return nil, nil
			case strings.Contains(s, "capture-pane"):
				// The scrollback half fails too — a server hiccup, a wedged
				// pane, anything. An unread channel is inconclusive, never
				// "no refusal".
				return nil, fmt.Errorf("capture-pane refused")
			case strings.Contains(s, "list-panes"):
				return nil, nil
			}
			return []byte("output"), nil
		},
	}
	session := newTmuxSession(sessionName, "claude", NewMockPtyFactory(t), exec)

	err := session.Start(workDir)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSpawnDirUnknown,
		"an unread refusal channel is inconclusive — held for retry, not approved")
	assert.False(t, killed,
		"no positive mismatch means the dead pane keeps its finish surface")
}

// TestStartPreservesFinishedPaneWhenPathStatUnverifiable is the pathname
// arm's inconclusive half: the pane is dead, the shim's channel answered
// clean, but the no-source re-stat of the worktree path fails with
// something other than ENOENT — an EACCES/EIO that says nothing about
// where the pane ran. Inconclusive evidence preserves the survivor under
// an Unknown verdict rather than tearing it down (#5174 review).
func TestStartPreservesFinishedPaneWhenPathStatUnverifiable(t *testing.T) {
	workDir := t.TempDir()

	// Admission stats cleanly; the dead arm's re-stat hits a permission
	// wall — inconclusive, never a disappearance.
	var statCalls atomic.Int32
	prevStat := statSpawnDir
	statSpawnDir = func(path string) (os.FileInfo, error) {
		if path == workDir && statCalls.Add(1) > 1 {
			return nil, &os.PathError{Op: "stat", Path: path, Err: syscall.EACCES}
		}
		return prevStat(path)
	}
	t.Cleanup(func() { statSpawnDir = prevStat })

	var killed bool
	sessionName := toTmuxName("dead-stat-unread", "")
	session := newTmuxSession(sessionName, "claude", NewMockPtyFactory(t),
		liveAfterSpawnExec(t, sessionName, map[string]string{
			"pane_dead":        "1",
			"pane_dead_status": "0",
		}, &killed))

	err := session.Start(workDir)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSpawnDirUnknown)
	assert.False(t, killed,
		"an unverifiable pathname must not tear down the pane it cannot place")
}
