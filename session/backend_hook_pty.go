package session

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// hookPTY holds a persistent attach_cmd process for preview capture.
// Instead of allocating a real PTY (which SSH rejects without a terminal),
// we use a pipe-based approach that captures whatever the attach_cmd writes.
type hookPTY struct {
	cmd      *exec.Cmd
	stdout   *os.File // read end of stdout pipe
	buf      []byte
	mu       sync.Mutex
	closed   bool
	waitOnce sync.Once
	waitDone chan struct{}
	waitErr  error
}

// ed2Marker is ED2 (erase entire display). The documented remote preview
// pattern (docs/remote-hooks.md) emits it at the top of every capture-loop
// iteration, so hookPTY ingestion treats it as a snapshot boundary (#810).
var ed2Marker = []byte("\x1b[2J")

// ingest appends a chunk of attach_cmd output to the preview buffer.
//
// ED2 (\x1b[2J) marks the start of a fresh snapshot: the documented preview
// contract is a clear-screen + capture loop, so everything up to and
// including the last ED2 is a stale frame and is dropped instead of
// concatenated (#810). The search runs over the accumulated buffer, not just
// the incoming chunk, so a sequence split across read boundaries is detected
// once its tail arrives — the head bytes are already in buf.
func (hp *hookPTY) ingest(chunk []byte) {
	const maxBuf = 64 * 1024
	hp.mu.Lock()
	defer hp.mu.Unlock()
	hp.buf = append(hp.buf, chunk...)
	if idx := bytes.LastIndex(hp.buf, ed2Marker); idx >= 0 {
		hp.buf = hp.buf[idx+len(ed2Marker):]
	}
	if len(hp.buf) > maxBuf {
		hp.buf = hp.buf[len(hp.buf)-maxBuf:]
	}
}

// --- Process management for preview capture ---

func (b *HookBackend) ensurePTY(i *Instance) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.ptys == nil {
		b.ptys = make(map[string]*hookPTY)
	}
	if existing, ok := b.ptys[i.Title]; ok {
		// If the existing entry is still alive, reuse it. Otherwise close and
		// drop it before creating a fresh process after attach_cmd exits.
		existing.mu.Lock()
		alive := !existing.closed
		existing.mu.Unlock()
		if alive {
			return nil
		}
		existing.closeStdout()
		delete(b.ptys, i.Title)
	}

	slug := hookNameForInstance(i)
	cmd := exec.Command(b.Hooks.AttachCmd, slug)

	// Use pipes instead of a PTY. The attach_cmd for preview doesn't need
	// a real terminal — we just want to capture whatever it outputs.
	// (SSH-based attach scripts will fail gracefully here since they
	// require a TTY, and that's fine — preview just shows empty until
	// the user attaches interactively.)
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create pipe for preview of %s: %w", i.Title, err)
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stdoutW

	if err := cmd.Start(); err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return fmt.Errorf("start attach_cmd for preview of %s: %w", i.Title, err)
	}
	// Close the write end in the parent so reads get EOF when the child exits.
	_ = stdoutW.Close()

	hp := &hookPTY{cmd: cmd, stdout: stdoutR, waitDone: make(chan struct{})}
	b.ptys[i.Title] = hp

	// Background goroutine reads output into a ring buffer with ED2
	// snapshot-reset semantics (see hookPTY.ingest).
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdoutR.Read(buf)
			if n > 0 {
				hp.ingest(buf[:n])
			}
			if err != nil {
				break
			}
		}
		// The reader has hit EOF or an error, which means the attach_cmd
		// child has closed its stdout (typically because it exited).
		// If stopPreview already marked us closed, the detaching caller
		// owns the Wait() call via reap (and may need to Kill the
		// process); otherwise the child exited on its own, so we Wait()
		// here to reap it and mark the entry closed so a subsequent
		// ensurePTY call can recreate it.
		hp.mu.Lock()
		alreadyClosed := hp.closed
		hp.mu.Unlock()
		if alreadyClosed {
			return
		}
		if err := hp.wait(); err != nil {
			log.ErrorLog.Printf("attach_cmd preview process exited: %v", err)
		}
		hp.mu.Lock()
		hp.closed = true
		hp.mu.Unlock()
	}()
	return nil
}

// stopPreview removes the preview entry for title and signals its process to
// stop, without waiting for it to exit. It returns the detached hookPTY (nil
// if none was registered) so the caller decides where to pay the grace-period
// wait in reap: Kill reaps synchronously so the preview process is gone before
// delete_cmd tears down the remote session it is connected to; Attach reaps in
// a background goroutine because it runs on the bubbletea event loop, where a
// blocking wait freezes the TUI (#817).
//
// The entry is deleted from b.ptys before the process exits. That is safe:
// once detached, the dying process can only write into its own hp.buf via its
// own pipe — Preview no longer finds the entry, and a replacement started by
// ensurePTY gets a fresh pipe and buffer, so output cannot interleave.
// Closing the pipe's read end makes the process's next write fail with EPIPE,
// nudging well-behaved attach scripts to exit during the grace period.
func (b *HookBackend) stopPreview(title string) *hookPTY {
	b.mu.Lock()
	defer b.mu.Unlock()

	hp, ok := b.ptys[title]
	if !ok {
		return nil
	}
	delete(b.ptys, title)

	hp.mu.Lock()
	hp.closed = true
	hp.mu.Unlock()

	_ = hp.stdout.Close()
	return hp
}

// reap gives the detached preview process a moment to exit, then kills it.
// It blocks for up to the 2s grace period, so it must never run on the TUI
// event loop — see stopPreview for which callers reap where.
func (hp *hookPTY) reap() {
	select {
	case <-hp.waitAsync():
	case <-time.After(2 * time.Second):
		_ = hp.cmd.Process.Kill()
	}
}

// closePTY synchronously stops and reaps the preview process for title.
// Used by Kill (delete_cmd must not race a live preview connection) and by
// test cleanup; Attach uses stopPreview + a background reap instead (#817).
func (b *HookBackend) closePTY(title string) {
	if hp := b.stopPreview(title); hp != nil {
		hp.reap()
	}
}

func (hp *hookPTY) waitAsync() <-chan struct{} {
	hp.waitOnce.Do(func() {
		go func() {
			hp.waitErr = hp.cmd.Wait()
			close(hp.waitDone)
		}()
	})
	return hp.waitDone
}

func (hp *hookPTY) wait() error {
	<-hp.waitAsync()
	return hp.waitErr
}

// SetPreviewBufferForTest seeds the raw preview buffer for title, creating
// the entry if needed. Exported in a non-test file (mirroring FakeBackend)
// so ui tests can drive the hook preview path through PreviewPane without
// spawning a real attach_cmd process.
func (b *HookBackend) SetPreviewBufferForTest(title string, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ptys == nil {
		b.ptys = make(map[string]*hookPTY)
	}
	hp, ok := b.ptys[title]
	if !ok {
		hp = &hookPTY{waitDone: make(chan struct{})}
		b.ptys[title] = hp
	}
	hp.mu.Lock()
	hp.buf = append([]byte(nil), data...)
	hp.mu.Unlock()
}

func (b *HookBackend) getPTY(title string) *hookPTY {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ptys == nil {
		return nil
	}
	return b.ptys[title]
}
