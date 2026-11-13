package execgroup

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// Output runs cmd and returns its standard output, like (*exec.Cmd).Output.
// CombinedOutput is the same for (*exec.Cmd).CombinedOutput. Together they are
// the normal-completion half of the bounded-command contract: a deadline still
// kills the whole group through KillOnCancel, and these decide what happens to
// the group when the command finished on its own (#7121).
//
// WHY not os/exec plus Reap. Reap SIGKILLs every member of the group, and on
// a command that finished on its own the group can hold more than leaks.
// Under ControlMaster/ControlPersist, OpenSSH moves the master into its own
// session with daemon() but leaves the master's ProxyCommand, or ProxyJump's
// `ssh -W`, in the caller's group as the master's transport. From OpenSSH 8.0
// that proxy's stderr is /dev/null, so it holds none of the command's pipes
// and the command returns success. A group kill then SIGKILLs the live
// transport and the master dies with it: every later connection
// re-authenticates, a concurrent connection multiplexed over the master fails
// with exit 255, and forwards attached to it die (#7115). A member that holds
// none of the command's pipes is not evidence of a leak.
//
// What is evidence is a member still holding one of the command's own pipes
// WaitDelay after the leader exited: that is the straggler a bounded command
// exists to end (#769, #7108). os/exec cannot say which process that is,
// because it owns the pipes. These functions own them instead, so they can
// ask the kernel per process. When a pipe is still open WaitDelay after the
// leader exited, they SIGKILL each process that is a member of the group cmd's
// leader leads AND holds that pipe, re-checking each instance at signal time
// (reapPipeHolders), and signal nothing else. Then they close the pipes, as
// os/exec does at WaitDelay.
//
// Everything else follows os/exec:
//   - cmd.Stdout must be unset, and for CombinedOutput cmd.Stderr too. When
//     Output finds cmd.Stderr unset, it captures stderr and attaches the
//     first and last 32 KiB to an *exec.ExitError's Stderr. A cmd.Stderr the
//     caller set is left to os/exec, so a holder of that pipe is not looked
//     for.
//   - A cmd.Stdin that is not an *os.File is copied in through a pipe these
//     functions own, so a member that holds it unread past WaitDelay is found
//     the same way. An *os.File reaches the child directly, as in os/exec.
//   - The error is cmd.Wait's. When the leader exited 0 but a pipe was still
//     open at WaitDelay, it is exec.ErrWaitDelay: the output is complete. A
//     failed copy is reported only when nothing else failed.
//   - WaitDelay is counted from the leader's exit. Under KillOnCancel a
//     deadline kills the leader at once, so that is also, to within the kill,
//     os/exec's "WaitDelay after the context is done". A zero WaitDelay waits
//     for the pipes indefinitely and never reaps, as os/exec waits for them.
//
// cmd must lead its own process group (Lead, or Setsid), which is how a
// member is recognised. Without one there is no group to reap from, and the
// pipes are only closed. cmd must come from exec.CommandContext if it uses
// Lead or KillOnCancel (see the package doc).
//
// Known limit: before OpenSSH 8.0 a ControlPersist proxy kept the caller's
// stderr. It is then a member holding the command's pipe past WaitDelay, the
// same evidence a wedged helper gives, and it is reaped.
func Output(cmd *exec.Cmd) ([]byte, error) {
	return capture(cmd, false)
}

// CombinedOutput is Output with stderr sent to the same pipe as stdout, like
// (*exec.Cmd).CombinedOutput.
func CombinedOutput(cmd *exec.Cmd) ([]byte, error) {
	return capture(cmd, true)
}

// stderrKeep is how much of each end of stderr Output attaches to an
// *exec.ExitError, the same 32 KiB os/exec keeps.
const stderrKeep = 32 << 10

func capture(cmd *exec.Cmd, combined bool) ([]byte, error) {
	if cmd.Stdout != nil {
		return nil, errors.New("exec: Stdout already set")
	}
	if combined && cmd.Stderr != nil {
		return nil, errors.New("exec: Stderr already set")
	}
	var pipes []*ownedPipe
	defer func() {
		for _, p := range pipes {
			p.closeBoth()
		}
	}()

	var stdout bytes.Buffer
	out, err := newOutputPipe(&stdout)
	if err != nil {
		return nil, err
	}
	pipes = append(pipes, out)
	cmd.Stdout = out.theirs

	var stderr *headTail
	switch {
	case combined:
		cmd.Stderr = out.theirs
	case cmd.Stderr == nil:
		stderr = &headTail{n: stderrKeep}
		errPipe, err := newOutputPipe(stderr)
		if err != nil {
			return nil, err
		}
		pipes = append(pipes, errPipe)
		cmd.Stderr = errPipe.theirs
	}

	if src := cmd.Stdin; src != nil {
		if _, isFile := src.(*os.File); !isFile {
			in, err := newInputPipe(src)
			if err != nil {
				return nil, err
			}
			pipes = append(pipes, in)
			cmd.Stdin = in.theirs
		}
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// Only the child may hold the child's ends from here on, or a pipe the
	// child has finished with would never reach EOF.
	for _, p := range pipes {
		_ = p.theirs.Close()
		go p.run()
	}

	err = cmd.Wait()
	overran := settle(cmd, pipes)
	if err == nil {
		for _, p := range pipes {
			if p.err != nil {
				err = p.err
				break
			}
		}
	}
	if err == nil && overran {
		err = exec.ErrWaitDelay
	}
	if ee, ok := err.(*exec.ExitError); ok && stderr != nil {
		ee.Stderr = stderr.Bytes()
	}
	return stdout.Bytes(), err
}

// settle waits, once the leader has exited, for every pipe to finish: EOF on
// an output pipe, the last byte written on stdin. It reports whether any was
// still open cmd.WaitDelay later. Those are reaped and then closed.
func settle(cmd *exec.Cmd, pipes []*ownedPipe) bool {
	if cmd.WaitDelay <= 0 {
		for _, p := range pipes {
			<-p.done
		}
		return false
	}
	timer := time.NewTimer(cmd.WaitDelay)
	defer timer.Stop()
	var open []*ownedPipe
	for _, p := range pipes {
		if open != nil {
			if !p.finished() {
				open = append(open, p)
			}
			continue
		}
		select {
		case <-p.done:
		case <-timer.C:
			open = []*ownedPipe{p}
		}
	}
	if open == nil {
		return false
	}
	if leadsOwnGroup(cmd) {
		ends := make([]proctree.PipeEnd, 0, len(open))
		for _, p := range open {
			ends = append(ends, p.end)
		}
		// Before abandon: each pipe's own end must still be open here while its
		// holders are looked for (see proctree.PipeEnd).
		reapPipeHolders(cmd.Process.Pid, ends)
	}
	for _, p := range open {
		p.abandon()
	}
	for _, p := range pipes {
		<-p.done
	}
	return true
}

// leadsOwnGroup reports whether cmd's child was started as the leader of its
// own process group, so that its pid names that group.
func leadsOwnGroup(cmd *exec.Cmd) bool {
	a := cmd.SysProcAttr
	return cmd.Process != nil && a != nil && (a.Setpgid && a.Pgid == 0 || a.Setsid)
}

// ownedPipe is one pipe between this process and the child: ours is the end
// kept here, theirs the end the child inherits, closed here once it has
// started. end identifies the pair, so that a process still holding theirs
// can be found while ours is open.
type ownedPipe struct {
	ours, theirs *os.File
	end          proctree.PipeEnd
	copy         func() error
	done         chan struct{} // closed when copy has returned
	err          error         // copy's result, valid once done is closed
	abandoned    atomic.Bool   // set before ours is closed under the copy
}

func newOutputPipe(dst io.Writer) (*ownedPipe, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	p := &ownedPipe{ours: r, theirs: w, done: make(chan struct{})}
	p.copy = func() error {
		_, err := io.Copy(dst, r)
		return err
	}
	return p, p.identify()
}

func newInputPipe(src io.Reader) (*ownedPipe, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	p := &ownedPipe{ours: w, theirs: r, done: make(chan struct{})}
	p.copy = func() error {
		_, err := io.Copy(w, src)
		// A child that exits without reading all of its input is not a
		// failure, as in os/exec (golang.org/issue/9173).
		if errors.Is(err, syscall.EPIPE) {
			err = nil
		}
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		return err
	}
	return p, p.identify()
}

// identify records the identity of the pipe. A pipe whose holders cannot be
// named would leave a straggler on it unreapable, so failing here fails the
// command before it starts rather than degrading silently.
func (p *ownedPipe) identify() error {
	end, err := proctree.PipeEndOf(p.theirs, p.ours)
	if err != nil {
		p.closeBoth()
		return err
	}
	p.end = end
	return nil
}

func (p *ownedPipe) run() {
	defer close(p.done)
	err := p.copy()
	if p.abandoned.Load() {
		// Closing ours under the copy is how a pipe is given up at
		// WaitDelay; the error that produces is not the command's.
		err = nil
	}
	p.err = err
}

func (p *ownedPipe) finished() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// abandon gives up on a pipe still open at WaitDelay. Closing our end wakes
// the copy, which is blocked on it.
func (p *ownedPipe) abandon() {
	p.abandoned.Store(true)
	_ = p.ours.Close()
}

func (p *ownedPipe) closeBoth() {
	_ = p.ours.Close()
	_ = p.theirs.Close()
}

// headTail keeps the first and last n bytes written to it, as os/exec's
// prefixSuffixSaver does for the stderr Output attaches to an ExitError.
type headTail struct {
	n       int
	head    []byte
	tail    []byte
	skipped int64
}

func (w *headTail) Write(b []byte) (int, error) {
	written := len(b)
	if room := w.n - len(w.head); room > 0 {
		k := min(room, len(b))
		w.head = append(w.head, b[:k]...)
		b = b[k:]
	}
	w.tail = append(w.tail, b...)
	if over := len(w.tail) - w.n; over > 0 {
		w.skipped += int64(over)
		w.tail = append([]byte(nil), w.tail[over:]...)
	}
	return written, nil
}

func (w *headTail) Bytes() []byte {
	out := append([]byte(nil), w.head...)
	if w.skipped > 0 {
		out = append(out, "\n... omitting "+strconv.FormatInt(w.skipped, 10)+" bytes ...\n"...)
	}
	return append(out, w.tail...)
}
