package api

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// currentTmuxName identifies the calling pane, never the server's most recent
// session. Held in a var so command tests can substitute an identity.
var currentTmuxName = func() (string, error) {
	context := os.Getenv("TMUX")
	if context == "" {
		return "", fmt.Errorf("not running inside a tmux session")
	}
	pane := os.Getenv("TMUX_PANE")
	// Only accept a pane ID; an empty or arbitrary tmux target could resolve to
	// a session unrelated to the caller.
	if len(pane) < 2 || pane[0] != '%' || strings.Trim(pane[1:], "0123456789") != "" {
		return "", fmt.Errorf("could not determine tmux session name: missing or invalid TMUX_PANE")
	}
	// TMUX is socket,pid,session-index. Split from the right because socket
	// paths may contain commas. Explicit -S also supports non-default servers.
	socket := context
	for range 2 {
		end := strings.LastIndexByte(socket, ',')
		if end < 0 {
			return "", fmt.Errorf("could not determine tmux session name: invalid TMUX socket context")
		}
		socket = socket[:end]
	}
	if !filepath.IsAbs(socket) {
		return "", fmt.Errorf("could not determine tmux session name: invalid TMUX socket path")
	}
	identity := os.Getenv(tmux.EnvMarkerSession)
	out, err := exec.Command("tmux", "-S", socket, "display-message", "-p", "-t", pane, "#{session_name}").Output()
	if err != nil {
		return "", fmt.Errorf("not running inside a tmux session: %w", err)
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return "", fmt.Errorf("could not determine tmux session name")
	}
	// AF_SESSION is stamped into agent environments at pane creation. It is
	// primary when present, but stale/nested environments must fail closed.
	// Older tmux versions cannot stamp it, so the explicit pane is the fallback.
	if identity != "" {
		if identity != name {
			return "", fmt.Errorf("AF_SESSION %q does not match tmux pane session %q", identity, name)
		}
		return identity, nil
	}
	return name, nil
}
