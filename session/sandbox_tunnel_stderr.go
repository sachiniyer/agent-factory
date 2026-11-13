package session

import (
	"bytes"
	"fmt"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/internal/credscrub"
)

// tunnelStderrKeep is how much of a tunnel's stderr is kept: enough for the
// last few lines ssh writes before it gives up, bounded for a tunnel that
// lives for days and writes a warning now and then.
const tunnelStderrKeep = 4 << 10

// tunnelStderrTail keeps the end of what a tunnel generation writes to
// stderr, so a forward that never comes up can say why (#7121). Both spawn
// paths used to send it to /dev/null, and then a tunnel that could not log in
// failed with nothing but a timeout, while the operator's sandbox.ssh worked by
// hand through a ControlMaster the tunnel never uses.
//
// It is the command's Stderr, so os/exec copies into it on its own goroutine.
// The lifeline passes its stdio to the shell it runs, so the tunnel's ssh
// writes here on that path too.
type tunnelStderrTail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tunnelStderrTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - tunnelStderrKeep; over > 0 {
		t.buf = append([]byte(nil), t.buf[over:]...)
	}
	return len(p), nil
}

// lastLine is the last non-blank line written so far, scrubbed of anything
// shaped like a credential (a ProxyCommand wrapper may print one), and
// shortened to one readable line.
func (t *tunnelStderrTail) lastLine() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := bytes.Split(bytes.TrimRight(t.buf, "\r\n\t "), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(string(lines[i]))
		if line == "" {
			continue
		}
		line = credscrub.Scrub(line)
		const max = 300
		if r := []rune(line); len(r) > max {
			line = string(r[:max]) + "…"
		}
		return line
	}
	return ""
}

// withTunnelStderr adds the last line child's ssh wrote to err, when there is
// one. A generation started before stderr was kept, or one that wrote
// nothing, leaves err as it is.
func withTunnelStderr(err error, child *tunnelChild) error {
	if err == nil || child == nil || child.cmd == nil {
		return err
	}
	tail, ok := child.cmd.Stderr.(*tunnelStderrTail)
	if !ok {
		return err
	}
	if line := tail.lastLine(); line != "" {
		return fmt.Errorf("%w; the tunnel's ssh last wrote: %s", err, line)
	}
	return err
}
