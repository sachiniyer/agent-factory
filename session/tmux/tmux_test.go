package tmux

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cmd2 "github.com/sachiniyer/agent-factory/cmd"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	aflog "github.com/sachiniyer/agent-factory/log"

	"github.com/stretchr/testify/require"
)

// TestMain initializes the logger so tests that exercise paths writing to
// InfoLog/ErrorLog (e.g. Restore's re-spawn fallback) do not nil-deref.
func TestMain(m *testing.M) {
	// #837: fail the package loudly if any test touches the real config.json.
	verifyRealConfig := testguard.ConfigTripwire()
	// #1056: fail loudly if a test leaks an af_ session onto the ambient tmux
	// server, and default the whole package into a sandboxed
	// AGENT_FACTORY_HOME so stray config/state/log writes land in a temp dir
	// instead of the developer's real one. Sandbox AFTER the tripwires
	// snapshot the real environment, BEFORE logging resolves its file path.
	verifyTmux := testguard.TmuxTripwire()
	restoreHome := testguard.SandboxHome()
	// #1122: default the whole package onto a private tmux server so a test
	// that forgets IsolateTmux can never create or sweep sessions on the
	// developer's real server. CleanupSessions runs in this package's tests —
	// exactly the sweep that killed every production session in outage #3.
	restoreTmux := testguard.SandboxTmux()
	aflog.Initialize(false)
	code := m.Run()
	aflog.Close()
	restoreTmux()
	restoreHome()
	if err := verifyRealConfig(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	if err := verifyTmux(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
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

// TestSanitizeNameTmuxRestrictedChars guards toTmuxName against the #574/#2207
// failure mode: tmux silently rewrites, expands, or escapes parts of session
// names. The positive policy must transform every rune outside the reviewed
// safe set before tmux sees it; otherwise Start probes a different spelling
// and times out while the created session keeps running.
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
		// tmux 3.4 visually escapes a backslash in the stored session name.
		{"backslash", `a\b`, TmuxPrefix + "a_b"},
		// All tmux-special characters together, plus whitespace stripping.
		{"tmux-specials-and-spaces", `a.b :c #d $e \f`, TmuxPrefix + "a_b_c_d_e_f"},
		// The positive policy rejects punctuation even when one tmux version
		// happens to preserve it, avoiding another parser-version denylist gap.
		{"other-punctuation", "a/b,c;d@e%f(g)", TmuxPrefix + "a_b_c_d_e_f_g_"},
		// tmux preserves valid UTF-8; keep letters and combining marks readable.
		{"unicode", "日本語-e\u0301", TmuxPrefix + "日本語-e\u0301"},
		// Non-whitespace controls are visually escaped by tmux and must not pass.
		{"control", "a\x1bb", TmuxPrefix + "a_b"},
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

// TestRestoreSwapsMonitorWhenSessionExists covers issue #386 case (a) as of
// #1592 Phase 2 PR7: when the tmux session is alive, Restore is a pure logical
// rebind — it opens NO `tmux attach-session` render client (the clientless
// agent-server owns the data plane) and only swaps in a fresh status monitor.
func TestRestoreSwapsMonitorWhenSessionExists(t *testing.T) {
	ptyFactory := NewMockPtyFactory(t)

	cmdExec := cmd_test.MockCmdExec{
		// All cmdExec.Run calls succeed → has-session reports the session exists.
		RunFunc:    func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return []byte("output"), nil },
	}

	session := newTmuxSession(toTmuxName("existing", ""), "claude", ptyFactory, cmdExec)

	require.NoError(t, session.Restore("/some/work/dir"))

	require.Equal(t, 0, len(ptyFactory.cmds), "Restore must open no PTY / attach-session client")
	require.NotNil(t, session.monitor, "Restore must install a fresh status monitor")
}

// TestRestoreRespawnsWhenSessionMissing covers issue #386 case (b): when the
// tmux server has died (e.g. across a machine reboot) the session is gone but
// the worktree on disk is fine. Restore must transparently re-spawn the
// session in workDir using the same program. The re-spawn's `new-session` is
// the ONLY PTY command now — the inner Restore("") opens no attach client (PR7).
func TestRestoreRespawnsWhenSessionMissing(t *testing.T) {
	// Pin the exact new-session argv regardless of the box's tmux version;
	// marker injection is covered by TestStartInjectsEnvMarkers.
	forceNewSessionEnvMarkers(t, false)
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

	require.Equal(t, 1, len(ptyFactory.cmds),
		"expected only the re-spawn new-session PTY command (no attach-session)")
	// Re-spawn must include the resume-most-recent flag so the prior
	// conversation isn't lost on lazy respawn after a reboot (#595).
	require.Equal(t,
		fmt.Sprintf("tmux new-session -d -s af_missing -c %s claude --continue", workdir),
		cmd2.ToString(ptyFactory.cmds[0]))
}

// TestRestoreReturnsErrorWhenSessionMissingAndNoWorkDir guards the contract
// used by Start()'s internal Restore("") call: when no workDir is provided, a
// missing session is a real error and must not silently re-spawn (which would
// recurse infinitely from Start).
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
	// Pin the exact new-session argv regardless of the box's tmux version;
	// marker injection is covered by TestStartInjectsEnvMarkers.
	forceNewSessionEnvMarkers(t, false)
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
	// Only the new-session PTY command now: the internal Restore("") opens no
	// attach-session client (#1592 Phase 2 PR7).
	require.Equal(t, 1, len(ptyFactory.cmds))
	require.Equal(t, fmt.Sprintf("tmux new-session -d -s af_test-session -c %s claude", workdir),
		cmd2.ToString(ptyFactory.cmds[0]))

	require.Equal(t, 1, len(ptyFactory.files))

	// The new-session PTY is closed once the session exists (Start does not hold
	// a render client open).
	_, err = ptyFactory.files[0].Stat()
	require.Error(t, err)
}

// TestStartTimeoutCleanupSucceeds guards issue #696: when the positively
// sanitized session never appears before the startup timeout and cleanup
// confirms it absent, the error must authorize cleanup without rendering a nil
// error as the literal "<nil>".
func TestStartTimeoutCleanupSucceeds(t *testing.T) {
	ptyFactory := NewMockPtyFactory(t)

	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			// Session never appears; kill-session (cleanup) succeeds.
			if strings.Contains(cmd.String(), "has-session") {
				return fmt.Errorf("session not found")
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return []byte("output"), nil },
	}

	session := newTmuxSession(toTmuxName("timeout-ok", ""), "claude", ptyFactory, cmdExec)

	err := session.Start(t.TempDir())
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSessionNotStarted)
	require.Contains(t, err.Error(), "timed out waiting for tmux session af_timeout-ok")
	require.NotContains(t, err.Error(), "<nil>")
	require.NotContains(t, err.Error(), "cleanup error")
}

func TestStartTimeoutKeepsCleanupUnsafeWhileKilledPaneStillRuns(t *testing.T) {
	oldWait := paneExitWait
	paneExitWait = 20 * time.Millisecond
	t.Cleanup(func() { paneExitWait = oldWait })
	ptyFactory := NewMockPtyFactory(t)
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "has-session") {
				return fmt.Errorf("session not found")
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if strings.Contains(cmd.String(), "display-message") {
				return []byte(fmt.Sprintf("%d\n", os.Getpid())), nil
			}
			return nil, nil
		},
	}
	session := newTmuxSession(toTmuxName("timeout-live-pane", ""), "claude", ptyFactory, cmdExec)

	err := session.Start(t.TempDir())
	require.Error(t, err)
	require.ErrorIs(t, err, ErrTmuxTimeout)
	require.NotErrorIs(t, err, ErrSessionNotStarted,
		"a pane that has not exited must never authorize fresh-worktree removal")
}

// TestStartTimeoutCleanupFails is the companion to the #696 guard: when
// cleanup GENUINELY fails, the timeout error must include the cleanup cause.
// Post-#967 a kill-session failure only counts as a cleanup failure if the
// session survives the kill, so has-session reports the session present once
// the cleanup kill is attempted (the poll loop still times out because the
// session never appeared in time).
func TestStartTimeoutCleanupFails(t *testing.T) {
	ptyFactory := NewMockPtyFactory(t)

	killAttempted := false
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "kill-session") {
				killAttempted = true
				return fmt.Errorf("kill-session exploded")
			}
			if strings.Contains(cmd.String(), "has-session") {
				// The poll loop sees the session missing (so Start times
				// out); the post-kill Close probe sees it present, so the
				// failed kill is a genuine cleanup failure that must surface.
				if killAttempted {
					return nil
				}
				return fmt.Errorf("session not found")
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return []byte("output"), nil },
	}

	session := newTmuxSession(toTmuxName("timeout-bad", ""), "claude", ptyFactory, cmdExec)

	err := session.Start(t.TempDir())
	require.Error(t, err)
	require.ErrorIs(t, err, ErrTmuxTimeout)
	require.NotErrorIs(t, err, ErrSessionNotStarted)
	require.Contains(t, err.Error(), "timed out waiting for tmux session af_timeout-bad")
	require.Contains(t, err.Error(), "cleanup error")
	require.Contains(t, err.Error(), "kill-session exploded")
	require.NotContains(t, err.Error(), "<nil>")
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

	updated, hasPrompt, _ := session.HasUpdated()
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
		updated, hasPrompt, _ = session.HasUpdated()
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
	updated, _, _ := session.HasUpdated()
	require.True(t, updated, "first capture after Restore should report updated")
}

// TestHasUpdatedTransientErrorKeepsLogging covers the other branch of #489:
// if capture-pane fails but ExistsOrUnknown still reports the session is
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
		updated, hasPrompt, _ := session.HasUpdated()
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
