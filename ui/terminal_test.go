package ui

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/require"
)

// These tests cover the terminal (Shell) tab on the merged TabPane (#930 PR 2).
// The shell session is now owned by the Instance and created at start, so the
// tests drive a started local instance and exercise the shell slot
// (isAgentSlot=false) of TabPane. The hard-won TerminalPane fixes survive here:
// #920 Deleting fallback, #669 fallback-resets-scroll, #746 scroll-uses-selected
// view, #496 session-gone fallback (no ERROR log), and the #843 remote
// terminal_cmd behavior.

// tmuxTargetName extracts the tmux session name from a command's -t/-s flag,
// handling "-t name" / "-s name", "-t=name" / "-s=name", and the exact-match
// "-t =name:" / "-s =name:" spellings (#1006).
func tmuxTargetName(cmd *exec.Cmd) string {
	for i, a := range cmd.Args {
		switch {
		case (a == "-t" || a == "-s") && i+1 < len(cmd.Args):
			return strings.TrimSuffix(strings.TrimPrefix(cmd.Args[i+1], "="), ":")
		case strings.HasPrefix(a, "-t="):
			return strings.TrimPrefix(a, "-t=")
		case strings.HasPrefix(a, "-s="):
			return strings.TrimPrefix(a, "-s=")
		}
	}
	return ""
}

// shellMockExec returns a tmux mock that tracks session existence per name, so
// an instance's agent session and its shell sibling are independent (sharing a
// single existence flag would make the shell session's Start fail with "already
// exists"). capture-pane returns captureContent.
func shellMockExec(captureContent string) cmd_test.MockCmdExec {
	existing := map[string]bool{}
	return cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			s := cmd.String()
			name := tmuxTargetName(cmd)
			switch {
			case strings.Contains(s, "has-session"):
				if existing[name] {
					return nil
				}
				return fmt.Errorf("session does not exist")
			case strings.Contains(s, "new-session"):
				existing[name] = true
				return nil
			case strings.Contains(s, "kill-session"):
				delete(existing, name)
				return nil
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if strings.Contains(cmd.String(), "capture-pane") {
				return []byte(captureContent), nil
			}
			return []byte(""), nil
		},
	}
}

// makeShellInstance creates a started local instance with a live agent + shell
// tab pair. The shell tab is created by LocalBackend.Start as a sibling of the
// injected mock agent session, so its capture returns captureContent and stays
// hermetic.
func makeShellInstance(t *testing.T, title, captureContent string) *session.Instance {
	t.Helper()
	// Isolate config reads from the developer's real ~/.agent-factory (#658).
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	workdir := t.TempDir()
	setupGitRepo(t, workdir)

	name := fmt.Sprintf("test-shell-%s-%d", title, time.Now().UnixNano())
	cmdExec := shellMockExec(captureContent)

	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   name,
		Path:    workdir,
		Program: "bash",
	})
	require.NoError(t, err)

	pty := &MockPtyFactory{t: t, cmdExec: cmdExec}
	inst.SetTmuxSession(tmux.NewTmuxSessionWithDeps(name, "bash", pty, cmdExec))
	require.NoError(t, inst.Start(true))
	return inst
}

// makeRemoteInstance creates a started instance backed by a HookBackend with the
// given hooks.
func makeRemoteInstance(t *testing.T, title string, hooks config.RemoteHooks) *session.Instance {
	t.Helper()
	instance, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	instance.SetBackend(&session.HookBackend{Hooks: hooks})
	instance.SetStartedForTest(true)
	require.True(t, instance.IsRemote(), "precondition: instance must report as remote")
	return instance
}

func TestTabPaneShellUpdateContent(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const expected = "$ whoami\nuser\n$ ls\nfile1.txt  file2.txt"
	inst := makeShellInstance(t, "update", expected)
	defer func() { _ = inst.Kill() }()

	p := NewTabPane()
	p.SetSize(80, 30)

	require.NoError(t, p.UpdateContent(inst, 1))

	p.mu.Lock()
	require.False(t, p.content.fallback, "should not be in fallback after successful shell capture")
	require.Equal(t, expected, p.content.text, "content should match the shell pane capture")
	p.mu.Unlock()

	require.Contains(t, p.String(), "whoami", "rendered output should contain the captured content")
}

func TestTabPaneShellFallbackStates(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	p := NewTabPane()
	p.SetSize(80, 30)

	t.Run("nil instance", func(t *testing.T) {
		require.NoError(t, p.UpdateContent(nil, 1))
		p.mu.Lock()
		require.True(t, p.content.fallback)
		require.Contains(t, p.content.text, "Select an instance")
		p.mu.Unlock()
	})

	t.Run("not started instance", func(t *testing.T) {
		inst, err := session.NewInstance(session.InstanceOptions{
			Title: "not-started", Path: t.TempDir(), Program: "bash",
		})
		require.NoError(t, err)

		require.NoError(t, p.UpdateContent(inst, 1))
		p.mu.Lock()
		require.True(t, p.content.fallback)
		require.Contains(t, p.content.text, "not started")
		p.mu.Unlock()
	})

	// #920: a Deleting instance reports Started()==false during teardown. It
	// must show "Tearing down session..." rather than the not-started fallback.
	t.Run("deleting instance", func(t *testing.T) {
		inst, err := session.NewInstance(session.InstanceOptions{
			Title: "deleting", Path: t.TempDir(), Program: "bash",
		})
		require.NoError(t, err)
		inst.SetBackend(session.NewFakeBackend())
		inst.SetStatus(session.Deleting)

		require.NoError(t, p.UpdateContent(inst, 1))
		p.mu.Lock()
		require.True(t, p.content.fallback)
		require.Contains(t, p.content.text, "Tearing down session...")
		require.NotContains(t, p.content.text, "not started",
			"deleting instance must NOT show the not-started fallback (#920)")
		p.mu.Unlock()
	})
}

func TestTabPaneShellScrolling(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const numLines = 100
	lines := make([]string, numLines)
	for i := range numLines {
		lines[i] = fmt.Sprintf("line %d", i+1)
	}
	inst := makeShellInstance(t, "scroll", strings.Join(lines, "\n"))
	defer func() { _ = inst.Kill() }()

	p := NewTabPane()
	p.SetSize(80, 30)

	require.False(t, p.IsScrolling(), "should not be scrolling initially")

	require.NoError(t, p.ScrollUp(inst, 1))
	require.True(t, p.IsScrolling(), "ScrollUp should enter scroll mode")
	require.NotEmpty(t, p.viewport.View(), "viewport should have content in scroll mode")

	require.NoError(t, p.ScrollDown(inst, 1))
	require.True(t, p.IsScrolling(), "should still be scrolling after ScrollDown")

	require.NoError(t, p.ResetToNormalMode(inst, 1))
	require.False(t, p.IsScrolling(), "ResetToNormalMode should exit scroll mode")
}

// TestTabPaneShellFallbackResetsScrollMode is the #669 regression for the shell
// slot: entering a fallback (nil/!Started) while scrolling must exit scroll mode
// and clear the viewport, so String() (which checks isScrolling first) renders
// the fallback message, not the prior view's scroll content.
func TestTabPaneShellFallbackResetsScrollMode(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const priorContent = "stale-history-from-prior-instance"
	prior := makeShellInstance(t, "prior", priorContent)
	defer func() { _ = prior.Kill() }()

	t.Run("nil instance", func(t *testing.T) {
		p := NewTabPane()
		p.SetSize(80, 30)
		require.NoError(t, p.ScrollUp(prior, 1))
		require.True(t, p.IsScrolling(), "precondition: in scroll mode")
		require.Contains(t, p.viewport.View(), priorContent)

		require.NoError(t, p.UpdateContent(nil, 1))

		p.mu.Lock()
		require.True(t, p.content.fallback)
		require.False(t, p.isScrolling, "fallback must clear scroll state (#669)")
		p.mu.Unlock()

		rendered := p.String()
		require.Contains(t, rendered, "Select an instance")
		require.NotContains(t, rendered, priorContent)
	})

	t.Run("not started instance", func(t *testing.T) {
		p := NewTabPane()
		p.SetSize(80, 30)
		require.NoError(t, p.ScrollUp(prior, 1))
		require.True(t, p.IsScrolling())

		notStarted, err := session.NewInstance(session.InstanceOptions{
			Title: "not-started-669", Path: t.TempDir(), Program: "bash",
		})
		require.NoError(t, err)

		require.NoError(t, p.UpdateContent(notStarted, 1))

		p.mu.Lock()
		require.True(t, p.content.fallback)
		require.False(t, p.isScrolling)
		p.mu.Unlock()

		rendered := p.String()
		require.Contains(t, rendered, "not started")
		require.NotContains(t, rendered, priorContent)
	})
}

// TestTabPaneShellScrollUsesSelectedView is the #746 regression: the scroll path
// runs off the bubbletea loop and can fire before the async UpdateContent for a
// newly selected instance. Capture is keyed off the passed instance (not a
// title cache), so scrolling must reflect the selected instance's shell history,
// never the previously rendered instance's.
func TestTabPaneShellScrollUsesSelectedView(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const contentA = "terminal-history-from-instance-A"
	const contentB = "terminal-history-from-instance-B"
	instA := makeShellInstance(t, "A", contentA)
	defer func() { _ = instA.Kill() }()
	instB := makeShellInstance(t, "B", contentB)
	defer func() { _ = instB.Kill() }()

	p := NewTabPane()
	p.SetSize(80, 30)

	// A is rendered first, so it becomes the current view.
	require.NoError(t, p.UpdateContent(instA, 1))

	// User scrolls while B is the selected instance, before UpdateContent(B) ran.
	require.NoError(t, p.ScrollUp(instB, 1))
	require.True(t, p.IsScrolling())
	require.Contains(t, p.viewport.View(), contentB,
		"scroll must capture the selected instance's (B) shell history")
	require.NotContains(t, p.viewport.View(), contentA,
		"scroll must not capture the previously rendered instance's (A) history (#746)")

	// The scroll survives the next refresh for B.
	require.NoError(t, p.UpdateContent(instB, 1))
	require.True(t, p.IsScrolling())
	require.Contains(t, p.viewport.View(), contentB)
}

// TestTabPaneSwitchTabResetsScroll verifies that switching tab SLOTS (same
// instance) while scrolling resets scroll mode — the unified pane shares one
// scroll state across both tabs, so the agent-tab viewport must not leak into a
// shell-tab render.
func TestTabPaneSwitchTabResetsScroll(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := makeShellInstance(t, "switch", "agent-and-shell-content")
	defer func() { _ = inst.Kill() }()

	p := NewTabPane()
	p.SetSize(80, 30)

	// Scroll on the agent slot.
	require.NoError(t, p.ScrollUp(inst, 0))
	require.True(t, p.IsScrolling())

	// Switch to the shell slot for the SAME instance: scroll must reset.
	require.NoError(t, p.UpdateContent(inst, 1))
	require.False(t, p.IsScrolling(),
		"switching tabs must drop the previous tab's scroll state")
}

// TestTabPaneShellSessionGoneFallback is the #496 regression for the shell slot:
// when the shell session vanishes the pane must enter a fallback rather than
// propagate an error that handleError logs at ERROR every tick.
func TestTabPaneShellSessionGoneFallback(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	var gone atomic.Bool
	existing := map[string]bool{}
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			s := cmd.String()
			name := tmuxTargetName(cmd)
			switch {
			case strings.Contains(s, "has-session"):
				if gone.Load() {
					return fmt.Errorf("session gone")
				}
				if existing[name] {
					return nil
				}
				return fmt.Errorf("session does not exist")
			case strings.Contains(s, "new-session"):
				existing[name] = true
				return nil
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if strings.Contains(cmd.String(), "capture-pane") {
				if gone.Load() {
					return nil, fmt.Errorf("exit status 1")
				}
				return []byte("hello world"), nil
			}
			return []byte(""), nil
		},
	}

	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	workdir := t.TempDir()
	setupGitRepo(t, workdir)
	name := fmt.Sprintf("test-shell-gone-%d", time.Now().UnixNano())
	inst, err := session.NewInstance(session.InstanceOptions{Title: name, Path: workdir, Program: "bash"})
	require.NoError(t, err)
	pty := &MockPtyFactory{t: t, cmdExec: cmdExec}
	inst.SetTmuxSession(tmux.NewTmuxSessionWithDeps(name, "bash", pty, cmdExec))
	require.NoError(t, inst.Start(true))
	defer func() { _ = inst.Kill() }()

	p := NewTabPane()
	p.SetSize(80, 30)

	// Happy path: shell content renders.
	require.NoError(t, p.UpdateContent(inst, 1))
	p.mu.Lock()
	require.False(t, p.content.fallback)
	require.Contains(t, p.content.text, "hello world")
	p.mu.Unlock()

	// Session vanishes; prove UpdateContent does not log at ERROR.
	var errBuf bytes.Buffer
	prev := log.ErrorLog.Writer()
	log.ErrorLog.SetOutput(&errBuf)
	defer log.ErrorLog.SetOutput(prev)

	gone.Store(true)

	require.NoError(t, p.UpdateContent(inst, 1),
		"a gone shell session must NOT bubble an error up to handleError (#496)")
	p.mu.Lock()
	require.True(t, p.content.fallback, "shell pane must enter a fallback when the session is gone")
	require.Contains(t, p.content.text, "Terminal session")
	p.mu.Unlock()
	require.Empty(t, errBuf.String(), "no ERROR log line on the session-gone fallback path")
}

// TestTabPaneShellScrollModeSessionGoneExternally is the #977 regression: a shell
// tab in scroll mode whose tmux session is killed externally must transition to
// the "Terminal session not available." fallback instead of leaving the captured
// scrollback pinned on screen forever. updateShellLocked must run the TabAlive
// liveness check BEFORE its scroll-mode early-return, mirroring the agent slot.
func TestTabPaneShellScrollModeSessionGoneExternally(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const scrollback = "scrollback-line-from-live-shell"
	var gone atomic.Bool
	existing := map[string]bool{}
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			s := cmd.String()
			name := tmuxTargetName(cmd)
			switch {
			case strings.Contains(s, "has-session"):
				// A session killed externally fails has-session, which is what
				// DoesSessionExist()/TabAlive keys off of (#977).
				if gone.Load() {
					return fmt.Errorf("session gone")
				}
				if existing[name] {
					return nil
				}
				return fmt.Errorf("session does not exist")
			case strings.Contains(s, "new-session"):
				existing[name] = true
				return nil
			case strings.Contains(s, "kill-session"):
				delete(existing, name)
				return nil
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if strings.Contains(cmd.String(), "capture-pane") {
				return []byte(scrollback), nil
			}
			return []byte(""), nil
		},
	}

	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	workdir := t.TempDir()
	setupGitRepo(t, workdir)
	name := fmt.Sprintf("test-shell-scroll-gone-%d", time.Now().UnixNano())
	inst, err := session.NewInstance(session.InstanceOptions{Title: name, Path: workdir, Program: "bash"})
	require.NoError(t, err)
	pty := &MockPtyFactory{t: t, cmdExec: cmdExec}
	inst.SetTmuxSession(tmux.NewTmuxSessionWithDeps(name, "bash", pty, cmdExec))
	require.NoError(t, inst.Start(true))
	defer func() { _ = inst.Kill() }()

	p := NewTabPane()
	p.SetSize(80, 30)

	// Enter scroll mode on the live shell tab: the viewport fills with scrollback.
	require.NoError(t, p.ScrollUp(inst, 1))
	require.True(t, p.IsScrolling(), "precondition: shell tab is in scroll mode")
	require.Contains(t, p.viewport.View(), scrollback,
		"precondition: viewport holds the live shell's scrollback")

	// The shell session is killed externally while the user keeps scrolling.
	gone.Store(true)

	require.NoError(t, p.UpdateContent(inst, 1),
		"a gone shell session must NOT bubble an error up to handleError")

	p.mu.Lock()
	stillScrolling := p.isScrolling
	hasFallback := p.content.fallback
	fallbackText := p.content.text
	p.mu.Unlock()

	require.False(t, stillScrolling, "scroll mode must exit when shell session dies (#977)")
	require.True(t, hasFallback, "must show fallback when shell session is gone (#977)")
	require.Contains(t, fallbackText, "Terminal session not available.")

	rendered := p.String()
	require.Contains(t, rendered, "Terminal session not available.")
	require.NotContains(t, rendered, scrollback,
		"stale scrollback must not remain on screen after the session dies (#977)")
}

// TestTabPaneShellScrollModeAlreadyDead is the #998 regression (sibling of
// #977): entering scroll mode on a shell tab whose tmux session is ALREADY dead
// before entry must transition to the "Terminal session not available." fallback
// instead of leaving the last live capture pinned on screen. enterScrollModeLocked's
// !TabAlive branch used to bare-return, leaving p.content (fallback==false) holding
// stale terminal output and isScrolling==false, so String() rendered the stale
// content. It must call setFallbackState, mirroring updateShellLocked's !TabAlive
// branch and the ErrSessionGone capture path.
func TestTabPaneShellScrollModeAlreadyDead(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const liveCapture = "live-shell-output-before-death"
	var gone atomic.Bool
	existing := map[string]bool{}
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			s := cmd.String()
			name := tmuxTargetName(cmd)
			switch {
			case strings.Contains(s, "has-session"):
				if gone.Load() {
					return fmt.Errorf("session gone")
				}
				if existing[name] {
					return nil
				}
				return fmt.Errorf("session does not exist")
			case strings.Contains(s, "new-session"):
				existing[name] = true
				return nil
			case strings.Contains(s, "kill-session"):
				delete(existing, name)
				return nil
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if strings.Contains(cmd.String(), "capture-pane") {
				return []byte(liveCapture), nil
			}
			return []byte(""), nil
		},
	}

	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	workdir := t.TempDir()
	setupGitRepo(t, workdir)
	name := fmt.Sprintf("test-shell-scroll-dead-%d", time.Now().UnixNano())
	inst, err := session.NewInstance(session.InstanceOptions{Title: name, Path: workdir, Program: "bash"})
	require.NoError(t, err)
	pty := &MockPtyFactory{t: t, cmdExec: cmdExec}
	inst.SetTmuxSession(tmux.NewTmuxSessionWithDeps(name, "bash", pty, cmdExec))
	require.NoError(t, inst.Start(true))
	defer func() { _ = inst.Kill() }()

	p := NewTabPane()
	p.SetSize(80, 30)

	// Render the live shell tab so p.content holds the last capture (stale content
	// once the session dies) and currentInstance/currentTab are adopted (so the
	// dropStaleView guard does not reset state on the ScrollUp path below).
	require.NoError(t, p.UpdateContent(inst, 1))
	require.False(t, p.IsScrolling(), "precondition: not yet scrolling")
	require.Contains(t, p.String(), liveCapture,
		"precondition: pane renders the live shell capture")

	// The shell session is already dead before the user attempts to scroll.
	gone.Store(true)

	require.NoError(t, p.ScrollUp(inst, 1),
		"entering scroll mode on a dead shell must not bubble an error")

	p.mu.Lock()
	stillScrolling := p.isScrolling
	hasFallback := p.content.fallback
	fallbackText := p.content.text
	p.mu.Unlock()

	require.False(t, stillScrolling,
		"entering scroll mode on an already-dead shell must not leave the pane scrolling (#998)")
	require.True(t, hasFallback,
		"an already-dead shell must show a fallback on scroll-mode entry, not stale content (#998)")
	require.Contains(t, fallbackText, "Terminal session not available.")

	rendered := p.String()
	require.Contains(t, rendered, "Terminal session not available.",
		"rendered frame must show the dead-session fallback (#998)")
	require.NotContains(t, rendered, liveCapture,
		"stale terminal content must not remain on screen for an already-dead shell (#998)")
}

// TestTabPaneRemoteFallbackStates covers the #843 Terminal-tab UX for remote
// sessions on the merged pane.
func TestTabPaneRemoteFallbackStates(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	t.Run("terminal_cmd configured shows attach prompt", func(t *testing.T) {
		inst := makeRemoteInstance(t, "remote-843-on", config.RemoteHooks{TerminalCmd: "/bin/true"})
		p := NewTabPane()
		p.SetSize(80, 30)
		require.NoError(t, p.UpdateContent(inst, 1))
		p.mu.Lock()
		require.True(t, p.content.fallback)
		p.mu.Unlock()
		require.Contains(t, p.String(), "Press Enter to open a terminal")
	})

	t.Run("terminal_cmd absent keeps not-available fallback", func(t *testing.T) {
		inst := makeRemoteInstance(t, "remote-843-off", config.RemoteHooks{})
		p := NewTabPane()
		p.SetSize(80, 30)
		require.NoError(t, p.UpdateContent(inst, 1))
		p.mu.Lock()
		require.True(t, p.content.fallback)
		require.Contains(t, p.content.text, "not available for remote sessions")
		require.Contains(t, p.content.text, "terminal_cmd")
		p.mu.Unlock()
	})
}

// TestTabbedWindowAttachTerminalRemote verifies the shell-tab attach path for
// remote instances (#843) via TabbedWindow.AttachTerminalForInstance: with
// terminal_cmd configured it runs the hook (with the session slug); without it
// the attach is refused with an error naming terminal_cmd.
func TestTabbedWindowAttachTerminalRemote(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	tw := newTestTabbedWindow()

	t.Run("not configured refuses with actionable error", func(t *testing.T) {
		inst := makeRemoteInstance(t, "remote-843-noattach", config.RemoteHooks{})
		_, err := tw.AttachTerminalForInstance(inst, 1)
		require.Error(t, err)
		require.Contains(t, err.Error(), "terminal_cmd")
	})

	t.Run("configured runs terminal_cmd with the session slug", func(t *testing.T) {
		dir := t.TempDir()
		argsFile := filepath.Join(dir, "terminal-args")
		script := filepath.Join(dir, "terminal.sh")
		require.NoError(t, os.WriteFile(script,
			[]byte("#!/bin/sh\necho \"$1\" > \""+argsFile+"\"\n"), 0755))

		inst := makeRemoteInstance(t, "remote-843-attach", config.RemoteHooks{TerminalCmd: script})
		done, err := tw.AttachTerminalForInstance(inst, 1)
		require.NoError(t, err)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("terminal_cmd did not exit")
		}

		raw, err := os.ReadFile(argsFile)
		require.NoError(t, err, "terminal_cmd should have run")
		require.Equal(t, session.Slugify("remote-843-attach"), strings.TrimSpace(string(raw)),
			"terminal_cmd must receive the session slug")
	})
}

// TestTabbedWindowAttachTerminalRefusesNil guards the #716 captured-instance
// contract: AttachTerminalForInstance(nil) must error, and an instance with no
// live shell tab must refuse to attach rather than attaching the wrong session.
func TestTabbedWindowAttachTerminalRefusesNil(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	tw := newTestTabbedWindow()

	_, err := tw.AttachTerminalForInstance(nil, 1)
	require.Error(t, err, "AttachTerminalForInstance(nil) must error, not attach")

	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "unstarted", Path: t.TempDir(), Program: "claude",
	})
	require.NoError(t, err)
	require.False(t, inst.Started(), "precondition: instance must not be started")

	_, err = tw.AttachTerminalForInstance(inst, 1)
	require.Error(t, err, "an instance with no live shell tab must refuse to attach")
}
