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

// cantFindSessionExitError builds a genuine *exec.ExitError whose Stderr is
// tmux's exact "can't find session: <name>" absence diagnostic — the shape
// missingTmuxSession (and thus probeSessionStrict / tmuxProvedSessionAbsent)
// recognizes as positively confirmed absence, never a generic failure. Built by
// running a shell snippet because an *exec.ExitError's Stderr can only be
// populated by the os/exec runner; the name is a sanitized tmux name
// (letters/digits/_/-), so it threads through sh as an argument without
// quoting concerns. The close-branch tests use this so the post-kill strict
// probe sees the same authoritative absence a real tmux server answers with,
// rather than the plain error the lossy probe used to conflate with absence.
//
// The shell is resolved to an ABSOLUTE path at package initialization (before
// any test rewrites PATH via t.Setenv), so the fixture keeps working in tests
// that replace PATH with a temp dir of fake agent binaries
// (forceAgentOnSearchPath): exec.Command skips LookPath for an absolute path, so
// the rewritten PATH cannot hide sh from it.
func cantFindSessionExitError(name string) error {
	_, err := exec.Command(shForTestFixtures, "-c", `printf '%s\n' "$1" >&2; exit "$2"`,
		"sh", "can't find session: "+name, "1").Output()
	return err
}

// shForTestFixtures is the absolute path to sh, resolved once at package
// initialization (before TestMain or any test mutates PATH). See
// cantFindSessionExitError for why the absolute path matters.
var shForTestFixtures = func() string {
	if p, err := exec.LookPath("sh"); err == nil {
		return p
	}
	return "/bin/sh"
}()

// nameFromHasSessionArgs reads the session name a has-session command targeted.
// has-session uses `has-session -t=<name>` (an exact bare-name target, never the
// `=name:` pane form), so the `-t=<name>` arg carries the name verbatim.
func nameFromHasSessionArgs(args []string) string {
	for _, a := range args {
		if name, ok := strings.CutPrefix(a, "-t="); ok {
			return name
		}
	}
	return ""
}

// killSessionProbeExec mocks the two commands Close issues on its teardown path:
// `tmux kill-session` (which fails when the session is already gone) and the
// `tmux has-session` probe that decides whether the failure was real. `present`
// controls what has-session reports — i.e. whether the session survived the
// kill. killCalls counts kill-session invocations so tests can assert the kill
// was actually attempted.
//
// The post-kill probe now runs the strict path through probeSessionStrict
// (outputTmuxBoundedWith -> OutputFunc), so has-session's absence answer is
// routed through OutputFunc as a real *exec.ExitError carrying tmux's exact
// "can't find session: <name>" diagnostic — the only shape the strict probe
// accepts as positively confirmed absence. The lossy callers that still run
// has-session through RunFunc (sessionExists / ProbeSession) keep treating any
// non-nil error as absence, so RunFunc returns the plain exit-1 error for them.
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
			if strings.Contains(strings.Join(c.Args, " "), "has-session") {
				if present {
					return []byte(""), nil
				}
				return nil, cantFindSessionExitError(nameFromHasSessionArgs(c.Args))
			}
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
	_, absentExit := exec.Command("sh", "-c", "printf \"can't find session: af_gone\\n\" >&2; exit 1").Output()
	var tmuxExit *exec.ExitError
	require.ErrorAs(t, absentExit, &tmuxExit)
	require.Equal(t, 1, tmuxExit.ExitCode())

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
					return absentExit
				}
				return nil
			},
			OutputFunc: func(c *exec.Cmd) ([]byte, error) {
				if strings.Contains(strings.Join(c.Args, " "), "has-session") {
					if stillPresent[nameOf(c)] {
						return []byte(""), nil
					}
					return nil, absentExit
				}
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
					return absentExit
				}
				return nil
			},
			OutputFunc: func(c *exec.Cmd) ([]byte, error) {
				if strings.Contains(strings.Join(c.Args, " "), "has-session") {
					if stillPresent[nameOf(c)] {
						return []byte(""), nil
					}
					return nil, absentExit
				}
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
