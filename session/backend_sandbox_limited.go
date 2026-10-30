package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// ErrOutputOverLimit reports that a remote command produced more output than the
// caller allowed. The command was killed the moment the limit was crossed; nothing
// past the limit was ever held.
var ErrOutputOverLimit = errors.New("remote command output exceeded its limit")

// limitedShell is the optional capability of a sandboxShell that can bound a
// command's stdout while it streams (#5865). The host is untrusted by W7.3's own
// design, so a response that is read in full before it is checked bounds nothing.
type limitedShell interface {
	// RunLimited runs script and returns its stdout, holding at most limit bytes.
	// Past the limit the command is killed and ErrOutputOverLimit is returned.
	RunLimited(timeout time.Duration, script string, limit int64) ([]byte, error)
}

// limitWriter accepts at most limit bytes, then cancels the command and refuses
// the rest. It never buffers beyond the limit.
type limitWriter struct {
	limit    int64
	buf      bytes.Buffer
	exceeded bool
	cancel   context.CancelFunc
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if w.exceeded {
		return 0, ErrOutputOverLimit
	}
	room := w.limit - int64(w.buf.Len())
	if int64(len(p)) > room {
		w.buf.Write(p[:room])
		w.exceeded = true
		w.cancel()
		return int(room), ErrOutputOverLimit
	}
	return w.buf.Write(p)
}

// RunLimited satisfies limitedShell over the operator's ssh invocation, with the
// same command composition, process group and deadline as Run.
func (p *sandboxProvisioner) RunLimited(timeout time.Duration, script string, limit int64) ([]byte, error) {
	if p.runCommandFn != nil {
		// A test seam that returns whole buffers cannot stream; it is bounded after
		// the fact, which is all a seam can honestly do.
		out, err := p.runCommandFn(timeout, script, nil, false)
		if err == nil && int64(len(out)) > limit {
			return nil, ErrOutputOverLimit
		}
		return out, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := p.buildRunCommand(ctx, script, nil)
	out := &limitWriter{limit: limit, cancel: cancel}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = out, &boundedBuffer{buf: &stderr, max: 4096}
	err := cmd.Run()
	if out.exceeded {
		return nil, ErrOutputOverLimit
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
	}
	if err != nil && ctx.Err() != nil {
		return out.buf.Bytes(), fmt.Errorf("%q timed out after %s: %w", p.sshCmd, timeout, ctx.Err())
	}
	return out.buf.Bytes(), err
}

// boundedBuffer keeps the first max bytes written and discards the rest, so a
// chatty stderr cannot grow the daemon either.
type boundedBuffer struct {
	buf *bytes.Buffer
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}
