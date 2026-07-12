package session

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/creack/pty"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func (b *HookBackend) Attach(i *Instance) (chan struct{}, error) {
	i.mu.RLock()
	s := i.started
	i.mu.RUnlock()

	if !s {
		return nil, fmt.Errorf("cannot attach instance that has not been started")
	}

	// Stop the background preview process so it doesn't compete, but reap it
	// in the background: Attach runs on the bubbletea event loop, and waiting
	// out the 2s grace period here froze the whole TUI whenever the preview
	// process didn't exit promptly (#817). The dying preview only writes into
	// its own buffer via its own pipe (see stopPreview), so it cannot
	// interleave output with the interactive attach_cmd started below.
	if hp := b.stopPreview(i.Title); hp != nil {
		go hp.reap()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			// Restart preview process after detach. Failure is non-fatal —
			// the user can still re-attach interactively; we just lose the
			// background preview snapshot.
			if err := b.ensurePTY(i); err != nil {
				log.WarningLog.Printf("hook backend: preview process failed to restart for %s: %v", i.Title, err)
			}
		}()

		slug := hookNameForInstance(i)
		cmd := exec.Command(b.Hooks.AttachCmd, slug)
		if err := runHookAttachWithDetach(cmd, os.Stdin, os.Stdout, os.Stderr); err != nil {
			log.ErrorLog.Printf("attach_cmd error: %v", err)
		}
	}()
	return done, nil
}

// AttachTerminal gives interactive terminal access to the remote workspace by
// running terminal_cmd behind a local PTY, with the same detach-key handling
// as Attach. It powers the Terminal tab for remote sessions (#843): where
// attach_cmd connects to the AGENT's session, terminal_cmd opens a plain
// shell on the remote machine (typically `ssh <box>` into the workspace).
//
// Unlike Attach, the background preview process is left running: it captures
// the agent's attach_cmd stream over its own pipe, while terminal_cmd talks to
// a separate shell over its own PTY, so the two cannot compete for output.
//
// tabIdx is ignored: a remote session's tabs are fixed by hook config, so the
// Terminal tab always maps to the single terminal_cmd shell (#843). The
// parameter exists to satisfy the uniform Backend.AttachTerminal signature
// (#1592 Phase 1 PR5), which the local runtime uses to pick a shell tab.
func (b *HookBackend) AttachTerminal(i *Instance, _ int) (chan struct{}, error) {
	if !b.HasTerminalCmd() {
		return nil, fmt.Errorf("remote terminal is not configured: add a terminal_cmd to remote_hooks to enable the Terminal tab for remote sessions")
	}

	i.mu.RLock()
	s := i.started
	i.mu.RUnlock()
	if !s {
		return nil, fmt.Errorf("cannot open remote terminal for instance that has not been started")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		slug := hookNameForInstance(i)
		cmd := exec.Command(b.Hooks.TerminalCmd, slug)
		if err := runHookAttachWithDetach(cmd, os.Stdin, os.Stdout, os.Stderr); err != nil {
			log.ErrorLog.Printf("terminal_cmd error: %v", err)
		}
	}()
	return done, nil
}

// hookAttachDrainTimeout bounds how long we wait for the PTY copy goroutine to
// drain after the child exits. On the natural-exit path the master returns the
// child's remaining buffered output and then EIO once the slave is gone, so
// io.Copy terminates on its own — but a grandchild that inherited and kept the
// slave open would keep the master from ever EOFing. 2s is generous for a
// genuine drain while still bounding that pathological case (#912).
const hookAttachDrainTimeout = 2 * time.Second

func runHookAttachWithDetach(cmd *exec.Cmd, stdin io.Reader, stdout, stderr io.Writer) error {
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return err
	}
	// The remote hook attach runs the child under a PTY master and proxies it to
	// the local terminal — its PTYStream is exactly that master (#1592 Phase 1
	// PR5). driveHookPTYStream owns closing the stream on every exit path.
	return driveHookPTYStream(ptyFileStream{ptmx}, cmd, stdin, stdout, stderr)
}

// driveHookPTYStream copies a hook attach PTYStream to/from the local terminal,
// forwarding the detach key and emitting the #845 terminal restore on exit. It
// is the shared driver behind both HookBackend.Attach (attach_cmd, the agent
// session) and HookBackend.AttachTerminal (terminal_cmd, the Terminal tab): both
// open a PTYStream over their command and hand it here, so the detach/#912-drain/
// #845-restore behavior lives in one place regardless of which hook ran.
func driveHookPTYStream(stream PTYStream, cmd *exec.Cmd, stdin io.Reader, stdout, stderr io.Writer) error {
	defer stream.Close()

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	copyDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(stdout, stream)
		close(copyDone)
	}()

	detached := make(chan struct{})
	var detachOnce sync.Once
	detach := func() {
		detachOnce.Do(func() {
			close(detached)
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = stream.Close()
		})
	}

	go func() {
		buf := make([]byte, 32)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				// stdin.Read can batch the detach key with preceding bytes in a
				// single read (buffered terminal input), so check the last byte
				// rather than requiring it to be the sole byte — otherwise
				// Ctrl-W is forwarded to the PTY and the detach is silently
				// missed (#975). Forward any preceding bytes first so they still
				// reach the remote session, preserving the surrounding
				// write-error handling.
				if buf[n-1] == tmux.DetachKeyByte {
					if n > 1 {
						if _, writeErr := stream.Write(buf[:n-1]); writeErr != nil {
							return
						}
					}
					detach()
					return
				}
				if _, writeErr := stream.Write(buf[:n]); writeErr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	err := <-waitDone

	// Drain before closing. Now that the child (slave) has exited, io.Copy
	// reads the master's remaining buffered output and then sees EIO, so it
	// terminates on its own. Closing the stream here — before copyDone — would
	// race that final read and truncate the remote's last bytes (#912). The
	// detach path already closed the stream in detach(), so copyDone closes
	// promptly there and this drain is a harmless no-op. Bound the wait in case a
	// grandchild inherited the slave and is holding the master open: on timeout,
	// force the close to unblock io.Copy, then still wait for copyDone.
	select {
	case <-copyDone:
	case <-time.After(hookAttachDrainTimeout):
		_ = stream.Close() // grandchild holding slave open; stop waiting
		<-copyDone
	}
	_ = stream.Close() // idempotent; no-op if already closed

	// Every byte of the remote stream has been flushed (copyDone), so this
	// lands strictly after any modes the remote set. Emitted on every exit
	// path — detach kill, graceful exit, and error exit — because the remote
	// may have touched the terminal in all three (#845). Best-effort: if
	// stdout is gone there is no terminal left to restore.
	_, _ = io.WriteString(stdout, tmux.NeutralTerminalRestore)

	select {
	case <-detached:
		return nil
	default:
	}
	if err != nil {
		fmt.Fprintf(stderr, "remote attach exited: %v\n", err)
		return err
	}
	return nil
}
