package tmux

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// #1006: tmux resolves `-t name` by exact match first, then falls back to a
// PREFIX match. The agent session `af_proj` is an exact prefix of its surviving
// shell tab `af_proj__shell`, so once the agent dies a bare `-t af_proj` target
// silently resolves to the shell session. Action commands must pass `-t =name`
// to force an exact match and never prefix-collide with a sibling.

// targetOf extracts the session target spec from a tmux argv, preserving the
// exact-match `=` marker. It handles both the two-arg (`-t`, `=name:`) form and
// the single-arg (`-t=name`) form; the latter is normalized to `=name` because
// `-t=name` is itself an exact-match target.
func targetOf(args []string) string {
	for i, a := range args {
		if a == "-t" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, "-t=") {
			return "=" + strings.TrimPrefix(a, "-t=")
		}
	}
	return ""
}

// resolveTmuxTarget models tmux's real target resolution: an `=`-prefixed
// target matches only the exact name; a bare target matches exactly first and
// otherwise falls back to a unique prefix match. The trailing `:` window
// separator that exact-match pane targets carry (`=name:`) is stripped before
// matching. Returns the resolved session name and whether a session was found.
func resolveTmuxTarget(target string, sessions map[string]bool) (string, bool) {
	if strings.HasPrefix(target, "=") {
		name := strings.TrimSuffix(strings.TrimPrefix(target, "="), ":")
		return name, sessions[name]
	}
	if sessions[target] {
		return target, true
	}
	var match string
	n := 0
	for s := range sessions {
		if strings.HasPrefix(s, target) {
			match, n = s, n+1
		}
	}
	if n == 1 {
		return match, true
	}
	return "", false
}

// TestActionCommandsUseExactMatchTargets asserts every tmux action command the
// daemon and TUI issue against an agent session carries an `=name` exact-match
// target. This fails before the #1006 fix (bare `-t name`) and passes after.
func TestActionCommandsUseExactMatchTargets(t *testing.T) {
	const name = "af_proj"
	var targets []string
	record := func(args []string) {
		if strings.Contains(strings.Join(args, " "), "has-session") {
			return // existence probe already uses -t=, asserted elsewhere
		}
		if tgt := targetOf(args); tgt != "" {
			targets = append(targets, tgt)
		}
	}
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			record(c.Args)
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			record(c.Args)
			return []byte("content"), nil
		},
	}

	session := newTmuxSession(name, "claude", NewMockPtyFactory(t), cmdExec)

	_, err := session.CapturePaneContent()
	require.NoError(t, err)
	_, err = session.CapturePaneContentWithOptions("-", "-")
	require.NoError(t, err)
	require.NoError(t, session.SendKeysCommand("hello"))

	require.NotEmpty(t, targets)
	for _, tgt := range targets {
		require.Equal(t, fmt.Sprintf("=%s:", name), tgt,
			"every action command must target the agent session by exact match (#1006); got %q", tgt)
	}
}

// TestDeadAgentWithLiveShellSiblingIsNotPrefixMatched is the end-to-end #1006
// regression: the agent session `af_proj` has died but its `af_proj__shell` tab
// survives. With bare `-t af_proj` targets, capture-pane prefix-matches the
// shell, returns shell output, and HasUpdated reports updated=true — masking
// the dead agent and skipping the liveness check. With exact `-t =af_proj`
// targets, capture and has-session both correctly miss, so the corpse is seen.
func TestDeadAgentWithLiveShellSiblingIsNotPrefixMatched(t *testing.T) {
	const agentName = "af_proj"
	shellName := agentName + "__shell"
	// Only the shell sibling is alive; the agent session is gone.
	sessions := map[string]bool{shellName: true}

	var capturedShell bool
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if strings.Contains(strings.Join(c.Args, " "), "has-session") {
				if _, ok := resolveTmuxTarget(targetOf(c.Args), sessions); ok {
					return nil
				}
				return fmt.Errorf("can't find session")
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if strings.Contains(strings.Join(c.Args, " "), "capture-pane") {
				resolved, ok := resolveTmuxTarget(targetOf(c.Args), sessions)
				if !ok {
					return nil, fmt.Errorf("exit status 1")
				}
				if resolved == shellName {
					capturedShell = true
				}
				return []byte("resolved:" + resolved), nil
			}
			return []byte("output"), nil
		},
	}

	session := newTmuxSession(agentName, "claude", NewMockPtyFactory(t), cmdExec)
	session.monitor = newStatusMonitor()

	// Capturing the dead agent must surface ErrSessionGone, never the shell's
	// content via a prefix match.
	_, err := session.CapturePaneContent()
	require.ErrorIs(t, err, ErrSessionGone,
		"a dead agent with a live __shell sibling must report gone, not capture the shell (#1006)")
	require.False(t, capturedShell, "capture-pane must not resolve to the sibling shell session")

	// HasUpdated must not report the agent as updated/alive off the shell's
	// content; it must latch the monitor dead.
	updated, hasPrompt, _ := session.HasUpdated()
	require.False(t, updated, "a dead agent must not be marked updated via its shell sibling (#1006)")
	require.False(t, hasPrompt)
	require.True(t, session.monitor.dead, "monitor must latch dead once the agent session is confirmed gone")
}
