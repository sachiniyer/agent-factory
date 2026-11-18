package tmux

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTmuxArgvSkipsConfigFilesUnderNoConfig pins R1's tmux-config half: under
// --no-tmux-config every tmux invocation af builds carries -f /dev/null, so an
// image's /etc/tmux.conf or a user's ~/.tmux.conf cannot choose a program
// (default-shell, hooks, run-shell) or write session environment
// (set-environment -g) between af's credential-mount pins and the agent's exec
// (#7529 R1). -f is the flag the client reads files under at server start, so
// prepending it covers the spawn — calls against an already-running server
// ignore it.
func TestTmuxArgvSkipsConfigFilesUnderNoConfig(t *testing.T) {
	require.Equal(t, []string{"-S", "sock", "new-session"},
		tmuxArgv("-S", "sock", "new-session"), "off by default: the daemon host's tmux keeps its config")

	SetNoConfigFiles(true)
	t.Cleanup(func() { SetNoConfigFiles(false) })
	require.Equal(t, []string{"-f", "/dev/null", "-S", "sock", "new-session"},
		tmuxArgv("-S", "sock", "new-session"))

	cmd := boundedTmuxCommand(context.Background(), "-S", "sock", "has-session")
	require.Equal(t, []string{"tmux", "-f", "/dev/null", "-S", "sock", "has-session"}, cmd.Args,
		"every tmux invocation, not only new-session, must skip the image's config files")
}

// TestSetEnvEmptyRidesLaunchSnapshot pins the envEmpty half of the plumbing:
// the pin list the agent-server flag delivered reaches the wrapped pane
// command's pins section through the same programMu-guarded snapshot as the
// passthrough (#7529 R1).
func TestSetEnvEmptyRidesLaunchSnapshot(t *testing.T) {
	s := NewTmuxSession("env-empty-test", "codex")
	require.NoError(t, s.SetEnvEmpty([]string{"HTTPS_PROXY", "OPENAI_API_KEY"}))
	t.Cleanup(func() { require.NoError(t, s.SetEnvEmpty(nil)) })
	_, _, _, envEmpty, _, _, _, _ := s.launchSnapshot()
	require.Equal(t, []string{"HTTPS_PROXY", "OPENAI_API_KEY"}, envEmpty)
}
