package tmux

import (
	"context"
	"fmt"
	"strconv"
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
	// ExactTarget forces an exact session match, mirroring panePID(): the bare
	// `=name` form yields empty output for display-message, and the trailing `:`
	// is what makes the format resolve (#1006).
	output, err := t.outputTmuxBounded(ctx, "display-message", "-p", "-t", ExactTarget(t.sanitizedName), "#{socket_path}")
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

// serverSocketPID answers the server's (pid, socket_path) pair as ONE
// display-message value — the single-value socket read #7469 R5 requires:
// the socket path is free-form text, so it rides no multi-row listing where
// an embedded newline would print raw and change the row count. The pid
// rides beside it to corroborate that the socket names the same server a
// pane row answered for: a session respawned onto another server between
// the two reads must never pair one server's socket with another's pid.
// The pid fields are compared by the caller; any answer that does not parse
// is "could not read" — an incomplete record, never a wrong one.
func (t *TmuxSession) serverSocketPID() (pid int, socket string, err error) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	output, err := t.outputTmuxBounded(ctx, "display-message", "-p", "-t",
		ExactTarget(t.sanitizedName), "#{pid} #{socket_path}")
	if err != nil {
		return 0, "", err
	}
	return parseServerSocketReply(string(output))
}

// parseServerSocketReply reads "pid socket_path": the pid is the first
// field, the socket is EVERYTHING after the first space — verbatim,
// including spaces, commas, separators, and embedded newlines, all of which
// a socket path may legally hold. Only tmux's own trailing newline is
// trimmed. A non-numeric pid or an empty socket is not an answer.
func parseServerSocketReply(output string) (int, string, error) {
	// TrimSuffix, not TrimRight: the delimiter is exactly ONE newline, and
	// a socket filename may itself end in one — tmux writes the path
	// verbatim, so "pid /sock\n\n" carries socket "/sock\n" and stripping
	// both newlines would record a socket the carrier's env tuple never
	// names — an own carrier misread as foreign (Codex on #7469).
	trimmed := strings.TrimSuffix(output, "\n")
	pidRaw, socket, ok := strings.Cut(trimmed, " ")
	if !ok {
		return 0, "", fmt.Errorf("malformed server socket reply %q", trimmed)
	}
	pid, err := strconv.Atoi(pidRaw)
	if err != nil || pid <= 0 {
		return 0, "", fmt.Errorf("malformed server pid in socket reply %q", trimmed)
	}
	if socket == "" {
		return 0, "", fmt.Errorf("empty socket path in reply %q", trimmed)
	}
	return pid, socket, nil
}

// corroboratedServerSocket fills the socket half of a row-derived server
// identity from the server's own single-value answer, but only when the
// answer's pid matches the row's — the corroboration that keeps a mid-stamp
// server replacement from pairing one server's socket with another's pid
// (#7469 R5). A read failure or a pid mismatch leaves Socket empty, and the
// record stays incomplete — undecidable, never a wrong-but-valid one.
func corroboratedServerSocket(id ServerIdentity, read func() (int, string, error)) ServerIdentity {
	if id.PID <= 0 || id.Socket != "" {
		return id
	}
	pid, socket, err := read()
	if err != nil || pid != id.PID {
		return id
	}
	id.Socket = socket
	return id
}
