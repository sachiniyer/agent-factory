package tmux

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// errExit1 stands in for tmux's "can't find session" failure, which exits with
// status 1. Close must not key off this string — it probes has-session — but
// the mock returns it so the test mirrors the real failure shape.
var errExit1 = errors.New("exit status 1")

// killSessionProbeExec mocks the two commands Close issues on its teardown path:
// `tmux kill-session` (which fails when the session is already gone) and the
// `tmux has-session` probe that decides whether the failure was real. `present`
// controls what has-session reports — i.e. whether the session survived the
// kill. killCalls counts kill-session invocations so tests can assert the kill
// was actually attempted.
func killSessionProbeExec(present bool, killCalls *int) cmd_test.MockCmdExec {
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			s := strings.Join(c.Args, " ")
			switch {
			case strings.Contains(s, "kill-session"):
				if killCalls != nil {
					*killCalls++
				}
				// tmux exits 1 when the target session no longer exists.
				return errExit1
			case strings.Contains(s, "has-session"):
				if present {
					return nil
				}
				return errExit1
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			return []byte(""), nil
		},
	}
}

// TestClose_AlreadyDeadSession_ReturnsNil is the #967 fix: closing a session
// whose tmux process already died must succeed. kill-session exits 1, but the
// has-session probe confirms the session is gone — the desired end state of
// Close — so Close swallows the error and returns nil.
func TestClose_AlreadyDeadSession_ReturnsNil(t *testing.T) {
	var killCalls int
	exec := killSessionProbeExec(false /* session gone */, &killCalls)
	session := NewTmuxSessionFromSanitizedNameWithDeps("af_dead", "claude", NewMockPtyFactory(t), exec)

	state, err := session.Close()
	require.NoError(t, err,
		"a kill-session on an already-dead session is success, not an error (#967)")
	require.Equal(t, PaneStateKnown, state,
		"the has-session probe ANSWERED, so the session's fate is established")
	require.Equal(t, 1, killCalls, "Close must still attempt the kill-session")
}

// TestClose_SessionSurvivesKill_StillErrors is the negative half of the #967
// fix: a genuine teardown failure — kill-session errors AND the session is
// still present afterward — must still surface as an error, so the idempotency
// shortcut can't mask a session that refuses to die.
func TestClose_SessionSurvivesKill_StillErrors(t *testing.T) {
	exec := killSessionProbeExec(true /* session still present */, nil)
	session := NewTmuxSessionFromSanitizedNameWithDeps("af_stuck", "claude", NewMockPtyFactory(t), exec)

	state, err := session.Close()
	require.Error(t, err, "a session that survives kill-session is a real failure")
	require.Equal(t, PaneStateKnown, state,
		"tmux ANSWERED (the probe found the session alive), so the state is known and the "+
			"caller's pre-#1917 best-effort contract still governs")
	require.Contains(t, err.Error(), "error killing tmux session")
}

// TestCleanupSessions_ToleratesVanishedSession covers the same papercut on the
// by-name path: a session can disappear between `tmux ls` and kill-session
// (TOCTOU). A gone session is the goal of cleanup, so the vanished one must not
// fail the sweep, while a survivor still does.
func TestCleanupSessions_ToleratesVanishedSession(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", "/owned-home")
	const lsOutput = `af_gone: 1 windows (created Wed May 20 12:00:00 2026) [179x47]
af_stuck: 1 windows (created Wed May 20 12:01:00 2026) [179x47]`

	// af_stuck survives its kill; af_gone vanished before we got to it.
	stillPresent := map[string]bool{"af_stuck": true}
	nameOf := func(c *exec.Cmd) string {
		for i, a := range c.Args {
			if a == "-t" && i+1 < len(c.Args) {
				// Strip the exact-match `=name:` wrapper (`-t =name:`) so the modeled
				// session name matches the bare key tmux resolves to (#1006).
				return strings.TrimSuffix(strings.TrimPrefix(c.Args[i+1], "="), ":")
			}
			if strings.HasPrefix(a, "-t=") {
				return strings.TrimPrefix(a, "-t=")
			}
		}
		return ""
	}

	t.Run("vanished session is tolerated", func(t *testing.T) {
		exec := cmd_test.MockCmdExec{
			RunFunc: func(c *exec.Cmd) error {
				s := strings.Join(c.Args, " ")
				switch {
				case strings.Contains(s, "kill-session"):
					if nameOf(c) == "af_gone" {
						return errExit1
					}
					return nil
				case strings.Contains(s, "has-session"):
					if stillPresent[nameOf(c)] {
						return nil
					}
					return errExit1
				}
				return nil
			},
			OutputFunc: func(c *exec.Cmd) ([]byte, error) {
				if len(c.Args) > 1 && c.Args[1] == "show-environment" {
					// Both sessions carry this home's marker (#1122).
					return []byte("AF_HOME=/owned-home\n"), nil
				}
				if strings.Contains(strings.Join(c.Args, " "), "ls") {
					return []byte(lsOutput), nil
				}
				return []byte(""), nil
			},
		}
		require.NoError(t, CleanupSessions(exec),
			"a session that vanished between ls and kill must not fail cleanup (#967)")
	})

	t.Run("surviving session still errors", func(t *testing.T) {
		exec := cmd_test.MockCmdExec{
			RunFunc: func(c *exec.Cmd) error {
				s := strings.Join(c.Args, " ")
				switch {
				case strings.Contains(s, "kill-session"):
					return errExit1 // both kills fail
				case strings.Contains(s, "has-session"):
					if stillPresent[nameOf(c)] {
						return nil
					}
					return errExit1
				}
				return nil
			},
			OutputFunc: func(c *exec.Cmd) ([]byte, error) {
				if len(c.Args) > 1 && c.Args[1] == "show-environment" {
					// Both sessions carry this home's marker (#1122).
					return []byte("AF_HOME=/owned-home\n"), nil
				}
				if strings.Contains(strings.Join(c.Args, " "), "ls") {
					return []byte(lsOutput), nil
				}
				return []byte(""), nil
			},
		}
		err := CleanupSessions(exec)
		require.Error(t, err, "a session that survives kill-session must still fail cleanup")
		require.Contains(t, err.Error(), "af_stuck")
	})
}
