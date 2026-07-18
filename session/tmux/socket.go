package tmux

import (
	"context"
	"fmt"
	"strings"
)

// SocketPath returns the absolute filesystem path of the tmux server socket this
// session lives on, exactly as tmux itself resolves it (#{socket_path}).
//
// It exists for the config-agent attach (#2019). That takeover hands the
// terminal to `tmux attach-session` via tea.ExecProcess, and after $TMUX is
// scrubbed the child resolves its server through the DEFAULT socket
// (${TMUX_TMPDIR:-/tmp/tmux-<uid>}/default). The daemon that spawned the session
// and the TUI that attaches are different processes and can resolve DIFFERENT
// TMUX_TMPDIR values (an autostarted daemon may lack it while the user's shell
// sets it), so "default" would point at two different directories and the attach
// would fail to find the session. Reporting the authoritative socket path lets
// the attach pin it with `tmux -S <path>`, making resolution independent of
// either process's TMUX_TMPDIR.
//
// It is deliberately read-only: a single display-message query, bounded like
// panePID() (#1917). It opens no client and mutates no session state, so it does
// not belong to — and does not disturb — the Detach/Restore/capture/send core.
func (t *TmuxSession) SocketPath(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, tmuxCommandTimeout)
	defer cancel()
	// exactTarget forces an exact session match, mirroring panePID(): the bare
	// `=name` form yields empty output for display-message, and the trailing `:`
	// is what makes the format resolve (#1006).
	output, err := t.outputTmuxBounded(ctx, "display-message", "-p", "-t", exactTarget(t.sanitizedName), "#{socket_path}")
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("%w: display-message socket_path after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		return "", fmt.Errorf("failed to query socket path for session %s: %w", t.sanitizedName, err)
	}
	path := strings.TrimSpace(string(output))
	if path == "" {
		return "", fmt.Errorf("tmux reported an empty socket path for session %s", t.sanitizedName)
	}
	return path, nil
}
