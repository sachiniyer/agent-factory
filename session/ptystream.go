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
// LOCAL full-screen attach seam (Backend.AttachTerminal): the driver copies it
// over the local process's stdio. The browser web client does NOT read or write
// over this interface — it gets its terminal from the daemon's WebSocket PTY
// broker (daemon/ws_pty.go), which fans the agent-server's ring buffer of raw
// PTY bytes to WS subscribers and applies their input/resize back
// (AgentServer.Subscribe/Input/Resize). Same idea — a byte stream plus resize —
// over a different seam and transport.
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
