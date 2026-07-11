package session

import (
	"io"
	"os"

	"github.com/creack/pty"
)

// PTYStream is the uniform interactive-attach primitive (#1592 Phase 1 PR5): a
// bidirectional byte stream to a live PTY plus a resize control. An attach
// driver copies the stream to/from the local terminal and forwards the detach
// key, without knowing HOW the PTY was obtained — the remote hook runtime opens
// it by running attach_cmd/terminal_cmd under a PTY; the local runtime's tmux
// client attach is its own PTYStream-shaped equivalent (kept behind tmux's
// server-mediated detach semantics for now, see TmuxSession.Attach). This is the
// seam a future agent-server / web client (Phase 2) reads and writes over a
// WebSocket instead of local stdio — the transport changes, the primitive does
// not.
type PTYStream interface {
	io.ReadWriteCloser

	// Resize sets the PTY window size. rows/cols are terminal cells.
	Resize(rows, cols uint16) error
}

// ptyFileStream adapts an *os.File PTY master (as returned by pty.Start) to a
// PTYStream. Read/Write/Close come from *os.File; Resize maps to the standard
// TIOCSWINSZ ioctl via creack/pty.
type ptyFileStream struct {
	*os.File
}

func (s ptyFileStream) Resize(rows, cols uint16) error {
	return pty.Setsize(s.File, &pty.Winsize{Rows: rows, Cols: cols})
}
