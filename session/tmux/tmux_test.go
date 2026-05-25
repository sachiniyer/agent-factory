package tmux

import (
	"bytes"
	"errors"
	"fmt"
	cmd2 "github.com/sachiniyer/agent-factory/cmd"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	aflog "github.com/sachiniyer/agent-factory/log"

	"github.com/stretchr/testify/require"
)

// TestMain initializes the logger so tests that exercise paths writing to
// InfoLog/ErrorLog (e.g. Restore's re-spawn fallback) do not nil-deref.
func TestMain(m *testing.M) {
	aflog.Initialize(false)
	defer aflog.Close()
	os.Exit(m.Run())
}

type MockPtyFactory struct {
	t *testing.T

	// Array of commands and the corresponding file handles representing PTYs.
	cmds  []*exec.Cmd
	files []*os.File
}

func (pt *MockPtyFactory) Start(cmd *exec.Cmd) (*os.File, error) {
	filePath := filepath.Join(pt.t.TempDir(), fmt.Sprintf("pty-%s-%d", pt.t.Name(), rand.Int31()))
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_RDWR, 0644)
	if err == nil {
		pt.cmds = append(pt.cmds, cmd)
		pt.files = append(pt.files, f)
	}
	return f, err
}

func (pt *MockPtyFactory) Close() {}

func NewMockPtyFactory(t *testing.T) *MockPtyFactory {
	return &MockPtyFactory{
		t: t,
	}
}

func TestSanitizeName(t *testing.T) {
	// Without repo path (legacy naming)
	session := NewTmuxSession("asdf", "program")
	require.Equal(t, TmuxPrefix+"asdf", session.sanitizedName)

	session = NewTmuxSession("a sd f . . asdf", "program")
	require.Equal(t, TmuxPrefix+"asdf__asdf", session.sanitizedName)

	// With repo path (repo-scoped naming)
	session = NewTmuxSessionForRepo("asdf", "/home/user/repo", "program")
	hash := repoHash("/home/user/repo")
	require.Equal(t, TmuxPrefix+hash+"_asdf", session.sanitizedName)

	// Same title, different repo → different tmux name
	session2 := NewTmuxSessionForRepo("asdf", "/home/user/other-repo", "program")
	require.NotEqual(t, session.sanitizedName, session2.sanitizedName)

	// FromSanitizedName preserves exact name
	session3 := NewTmuxSessionFromSanitizedName("af_custom_name", "program")
	require.Equal(t, "af_custom_name", session3.sanitizedName)
}

// TestSanitizeNameTmuxRestrictedChars guards toTmuxName against the #574
// failure mode: tmux silently rewrites or escapes certain characters in
// session names, so a title containing them must be transformed before we
// hand it to tmux. Otherwise DoesSessionExist() polls for a name tmux never
// created and Start() times out, orphaning the session.
func TestSanitizeNameTmuxRestrictedChars(t *testing.T) {
	cases := []struct {
		name  string
		title string
		want  string
	}{
		// Existing dot behavior — regression guard.
		{"dot", "a.b", TmuxPrefix + "a_b"},
		// Issue #574: colon causes tmux to silently rewrite to '_'.
		{"colon", "test:session", TmuxPrefix + "test_session"},
		// '#' is tmux's format-escape — sanitize defensively.
		{"hash", "fix#574", TmuxPrefix + "fix_574"},
		// '$' is tmux's session-id prefix — tmux escapes it to '\$' in the
		// stored name, so has-session round-trips fail without sanitization.
		{"dollar", "a$b", TmuxPrefix + "a_b"},
		// All four together, plus whitespace stripping.
		{"all-four-and-spaces", "a.b :c #d $e", TmuxPrefix + "a_b_c_d_e"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session := NewTmuxSession(tc.title, "program")
			require.Equal(t, tc.want, session.sanitizedName)
		})
	}

	// Same checks for repo-scoped names.
	repoPath := "/home/user/repo"
	hash := repoHash(repoPath)
	scoped := NewTmuxSessionForRepo("test:session#1.x$2", repoPath, "program")
	require.Equal(t, TmuxPrefix+hash+"_test_session_1_x_2", scoped.sanitizedName)
}

// errPtyFactory is a PtyFactory that fails Start(). Used to verify Restore
// surfaces non-missing-session errors from the PTY layer.
type errPtyFactory struct {
	err error
}

func (e errPtyFactory) Start(_ *exec.Cmd) (*os.File, error) { return nil, e.err }
func (e errPtyFactory) Close()                              {}

// TestRestoreAttachesWhenSessionExists covers issue #386 case (a): when the
// tmux session is alive, Restore must attach to it (not re-spawn) regardless
// of whether a workDir was supplied.
func TestRestoreAttachesWhenSessionExists(t *testing.T) {
	ptyFactory := NewMockPtyFactory(t)

	cmdExec := cmd_test.MockCmdExec{
		// All cmdExec.Run calls succeed → has-session reports the session exists.
		RunFunc:    func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return []byte("output"), nil },
	}

	session := newTmuxSession(toTmuxName("existing", ""), "claude", ptyFactory, cmdExec)

	require.NoError(t, session.Restore("/some/work/dir"))

	require.Equal(t, 1, len(ptyFactory.cmds), "expected exactly one PTY command (attach-session)")
	require.Equal(t, "tmux attach-session -t af_existing", cmd2.ToString(ptyFactory.cmds[0]))
}

// TestRestoreRespawnsWhenSessionMissing covers issue #386 case (b): when the
// tmux server has died (e.g. across a machine reboot) the session is gone but
// the worktree on disk is fine. Restore must transparently re-spawn the
// session in workDir using the same program.
func TestRestoreRespawnsWhenSessionMissing(t *testing.T) {
	ptyFactory := NewMockPtyFactory(t)

	// First two has-session calls report missing (the outer Restore check, then
	// the existence check at the top of Start). After tmux new-session runs via
	// the PTY factory, subsequent has-session calls report exists so Start's
	// poll loop and the inner Restore("") call can succeed.
	hasSessionCalls := 0
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "has-session") {
				hasSessionCalls++
				if hasSessionCalls <= 2 {
					return fmt.Errorf("can't find session")
				}
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return []byte("output"), nil },
	}

	workdir := t.TempDir()
	session := newTmuxSession(toTmuxName("missing", ""), "claude", ptyFactory, cmdExec)

	require.NoError(t, session.Restore(workdir))

	require.Equal(t, 2, len(ptyFactory.cmds),
		"expected new-session followed by attach-session via PTY")
	// Re-spawn must include the resume-most-recent flag so the prior
	// conversation isn't lost on lazy respawn after a reboot (#595).
	require.Equal(t,
		fmt.Sprintf("tmux new-session -d -s af_missing -c %s claude --continue", workdir),
		cmd2.ToString(ptyFactory.cmds[0]))
	require.Equal(t, "tmux attach-session -t af_missing", cmd2.ToString(ptyFactory.cmds[1]))
}

// TestRestoreSurfacesPtyError covers issue #386 case (c): real failures
// distinct from "session does not exist" — here a PTY allocation failure on
// the attach path — must propagate to the caller unchanged so an operator
// can act on them rather than silently fall back to a re-spawn.
func TestRestoreSurfacesPtyError(t *testing.T) {
	ptyFactory := errPtyFactory{err: fmt.Errorf("pty allocation failed")}

	cmdExec := cmd_test.MockCmdExec{
		// Session exists → Restore takes the attach branch where PTY error fires.
		RunFunc:    func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return []byte("output"), nil },
	}

	session := newTmuxSession(toTmuxName("ptyerr", ""), "claude", ptyFactory, cmdExec)

	err := session.Restore("/some/work/dir")
	require.Error(t, err)
	require.Contains(t, err.Error(), "pty allocation failed")
}

// TestRestoreReturnsErrorWhenSessionMissingAndNoWorkDir guards the contract
// used by Detach() and Start()'s internal Restore("") call: when no workDir
// is provided, a missing session is a real error and must not silently
// re-spawn (which would lose history on Detach or recurse infinitely from
// Start).
func TestRestoreReturnsErrorWhenSessionMissingAndNoWorkDir(t *testing.T) {
	ptyFactory := NewMockPtyFactory(t)

	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "has-session") {
				return fmt.Errorf("can't find session")
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return []byte("output"), nil },
	}

	session := newTmuxSession(toTmuxName("gone", ""), "claude", ptyFactory, cmdExec)

	err := session.Restore("")
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not exist")
	require.Equal(t, 0, len(ptyFactory.cmds), "expected no PTY commands when no workDir is provided")
}

func TestStartTmuxSession(t *testing.T) {
	ptyFactory := NewMockPtyFactory(t)

	created := false
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "has-session") && !created {
				created = true
				return fmt.Errorf("session not found")
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte("output"), nil
		},
	}

	workdir := t.TempDir()
	session := newTmuxSession(toTmuxName("test-session", ""), "claude", ptyFactory, cmdExec)

	err := session.Start(workdir)
	require.NoError(t, err)
	require.Equal(t, 2, len(ptyFactory.cmds))
	require.Equal(t, fmt.Sprintf("tmux new-session -d -s af_test-session -c %s claude", workdir),
		cmd2.ToString(ptyFactory.cmds[0]))
	require.Equal(t, "tmux attach-session -t af_test-session",
		cmd2.ToString(ptyFactory.cmds[1]))

	require.Equal(t, 2, len(ptyFactory.files))

	// File should be closed.
	_, err = ptyFactory.files[0].Stat()
	require.Error(t, err)
	// File should be open
	_, err = ptyFactory.files[1].Stat()
	require.NoError(t, err)
}

// captureErrorLog redirects log.ErrorLog at the test's ErrorLog into the
// returned buffer for the duration of the test, restoring the previous
// destination on cleanup.
func captureErrorLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := aflog.ErrorLog.Writer()
	aflog.ErrorLog.SetOutput(&buf)
	t.Cleanup(func() { aflog.ErrorLog.SetOutput(prev) })
	return &buf
}

// makeAttachedSession builds a TmuxSession that has already been "Restored":
// a statusMonitor is in place, but no real PTY exists. captureOK controls
// whether capture-pane succeeds; sessionAlive controls what has-session
// reports. The returned counters are incremented on each respective call so
// tests can assert call counts.
func makeAttachedSession(t *testing.T, captureOK, sessionAlive *atomic.Bool) (*TmuxSession, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var captureCalls atomic.Int32
	var hasSessionCalls atomic.Int32

	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "has-session") {
				hasSessionCalls.Add(1)
				if sessionAlive.Load() {
					return nil
				}
				return fmt.Errorf("can't find session")
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if strings.Contains(cmd.String(), "capture-pane") {
				captureCalls.Add(1)
				if !captureOK.Load() {
					return nil, fmt.Errorf("exit status 1")
				}
				return []byte("pane content"), nil
			}
			return []byte("output"), nil
		},
	}

	session := newTmuxSession(toTmuxName("monitor", ""), "claude", NewMockPtyFactory(t), cmdExec)
	session.monitor = newStatusMonitor()
	return session, &captureCalls, &hasSessionCalls
}

// TestHasUpdatedSilentWhenSessionGone covers issue #489: when capture-pane
// fails because the tmux session is gone, HasUpdated must log exactly once,
// mark the monitor dead, and short-circuit subsequent polls so the
// per-second daemon loop cannot flood agent-factory.log.
func TestHasUpdatedSilentWhenSessionGone(t *testing.T) {
	var captureOK, sessionAlive atomic.Bool
	// capture-pane fails AND has-session reports dead.
	session, captureCalls, hasSessionCalls := makeAttachedSession(t, &captureOK, &sessionAlive)

	logs := captureErrorLog(t)

	updated, hasPrompt := session.HasUpdated()
	require.False(t, updated)
	require.False(t, hasPrompt)
	require.True(t, session.monitor.dead, "monitor must latch dead after confirming session is gone")
	require.Equal(t, int32(1), captureCalls.Load())
	require.Equal(t, int32(1), hasSessionCalls.Load())
	firstLog := logs.String()
	require.Contains(t, firstLog, "going silent")
	require.Equal(t, 1, strings.Count(firstLog, "\n"), "exactly one log line on the first failure")

	// 50 more ticks while the session is still gone must produce zero new
	// log lines and zero additional capture-pane / has-session calls.
	for i := 0; i < 50; i++ {
		updated, hasPrompt = session.HasUpdated()
		require.False(t, updated)
		require.False(t, hasPrompt)
	}
	require.Equal(t, firstLog, logs.String(), "no further logs while dead")
	require.Equal(t, int32(1), captureCalls.Load(), "no further capture-pane calls while dead")
	require.Equal(t, int32(1), hasSessionCalls.Load(), "no further has-session calls while dead")
}

// TestHasUpdatedRespawnResetsDead documents that a re-spawn via Restore
// produces a fresh statusMonitor with dead cleared, so polling resumes
// normally after the session comes back. This is the recovery path for
// issue #489 — operators don't have to restart the daemon after a stale
// instance is healed.
func TestHasUpdatedRespawnResetsDead(t *testing.T) {
	var captureOK, sessionAlive atomic.Bool
	session, _, _ := makeAttachedSession(t, &captureOK, &sessionAlive)
	_ = captureErrorLog(t)

	// First poll: session gone, monitor goes dead.
	session.HasUpdated()
	require.True(t, session.monitor.dead)

	// Session comes back; Restore attaches and installs a fresh monitor.
	sessionAlive.Store(true)
	captureOK.Store(true)
	require.NoError(t, session.Restore("/some/work/dir"))
	require.NotNil(t, session.monitor)
	require.False(t, session.monitor.dead, "fresh monitor after Restore must not be dead")

	// Polling resumes and produces a normal updated=true on first content.
	updated, _ := session.HasUpdated()
	require.True(t, updated, "first capture after Restore should report updated")
}

// TestHasUpdatedTransientErrorKeepsLogging covers the other branch of #489:
// if capture-pane fails but DoesSessionExist still reports the session is
// alive (a rare transient error), the monitor must NOT latch dead and the
// error must still be visible in the log every tick — the spam fix should
// not silently swallow real problems.
func TestHasUpdatedTransientErrorKeepsLogging(t *testing.T) {
	var captureOK, sessionAlive atomic.Bool
	// capture-pane fails BUT has-session reports the session is alive.
	sessionAlive.Store(true)
	session, captureCalls, hasSessionCalls := makeAttachedSession(t, &captureOK, &sessionAlive)

	logs := captureErrorLog(t)

	for i := 0; i < 3; i++ {
		updated, hasPrompt := session.HasUpdated()
		require.False(t, updated)
		require.False(t, hasPrompt)
	}
	require.False(t, session.monitor.dead, "transient capture-pane error must not latch dead")
	require.Equal(t, int32(3), captureCalls.Load())
	require.Equal(t, int32(3), hasSessionCalls.Load())
	require.Equal(t, 3, strings.Count(logs.String(), "error capturing pane content in status monitor"))
	require.NotContains(t, logs.String(), "going silent")
}

// TestCapturePaneContentWrapsErrSessionGoneWhenGone covers #496: a vanished
// tmux session must surface to non-daemon callers as ErrSessionGone so the
// preview pane can render an inactive-session state instead of
// handleError-ing at ERROR.
func TestCapturePaneContentWrapsErrSessionGoneWhenGone(t *testing.T) {
	var captureOK, sessionAlive atomic.Bool
	// capture-pane fails AND has-session reports dead.
	session, _, _ := makeAttachedSession(t, &captureOK, &sessionAlive)

	_, err := session.CapturePaneContent()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSessionGone,
		"capture-pane failure on a gone session must wrap ErrSessionGone")
}

// TestCapturePaneContentTransientErrorDoesNotWrap guards the other branch:
// if has-session still reports alive, the wrap must NOT happen — callers
// should still see/log the original error and operators get visibility into
// rare transient capture-pane failures.
func TestCapturePaneContentTransientErrorDoesNotWrap(t *testing.T) {
	var captureOK, sessionAlive atomic.Bool
	sessionAlive.Store(true) // alive but capture fails
	session, _, _ := makeAttachedSession(t, &captureOK, &sessionAlive)

	_, err := session.CapturePaneContent()
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrSessionGone),
		"transient capture-pane error must not be misclassified as ErrSessionGone")
	require.Contains(t, err.Error(), "error capturing pane content")
}

// TestCapturePaneContentWithOptionsWrapsErrSessionGone mirrors the
// CapturePaneContent test for the scroll-mode/full-history path that
// PreviewFullHistory and the terminal pane's scroll mode use.
func TestCapturePaneContentWithOptionsWrapsErrSessionGone(t *testing.T) {
	var captureOK, sessionAlive atomic.Bool
	session, _, _ := makeAttachedSession(t, &captureOK, &sessionAlive)

	_, err := session.CapturePaneContentWithOptions("-", "-")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSessionGone)
}

// TestSetDetachedSizeReturnsErrSessionGoneWhenPtmxNil covers #496: when the
// PTY has been cleared (Detach failure in #474, Close, or never restored),
// SetDetachedSize must surface ErrSessionGone instead of panicking on
// nil.Fd() or emitting "bad file descriptor" at ERROR.
func TestSetDetachedSizeReturnsErrSessionGoneWhenPtmxNil(t *testing.T) {
	var captureOK, sessionAlive atomic.Bool
	session, _, _ := makeAttachedSession(t, &captureOK, &sessionAlive)
	require.Nil(t, session.ptmx, "precondition: helper builds a session without a PTY")

	err := session.SetDetachedSize(80, 24)
	require.ErrorIs(t, err, ErrSessionGone)
}
