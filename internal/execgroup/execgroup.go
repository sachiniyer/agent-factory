// Package execgroup holds af's one implementation of the bounded-command
// process-group contract: a context-bound exec.Cmd runs as the leader of its
// own process group, and cancellation SIGKILLs that whole group — the child
// plus anything it spawned that still holds an inherited pipe — rather than
// only the direct process.
//
// WHY this exists. exec.CommandContext's default Cancel kills only the direct
// process, and every bounded command here exists because a real stall proved
// that insufficient: a wedged child holding the capture pipe keeps
// Output()/Wait() blocked on pipe EOF past the deadline, and a shell script's
// backgrounded grandchildren outlive the kill entirely (#610, #769, #856).
// The contract used to be copy-pasted across a dozen call sites — each
// restating the negative-pid kill and the ESRCH mapping in its own words —
// which is how subtly divergent variants crept in beside it. It is stated
// once here instead.
//
// The WaitDelay half of a bound deliberately stays with each caller: the
// right delay is per-command, and os/exec documents its semantics at the
// field.
package execgroup

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Lead puts cmd in its own process group (SysProcAttr.Setpgid) and installs
// KillOnCancel, so a context deadline tears down the whole tree the group
// holds rather than orphaning a spawned child that still holds the pipes.
func Lead(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	KillOnCancel(cmd)
}

// KillOnCancel installs on cmd the Cancel that SIGKILLs the process GROUP led
// by the child — the negative pid reaches every member, not just the direct
// process exec.CommandContext kills by default. A group already gone (ESRCH)
// maps to os.ErrProcessDone, which Wait ignores rather than reporting as a
// command failure. A nil Process — a command that never started — is a
// no-op.
//
// Callers whose SysProcAttr differs from Lead's Setpgid still share this
// Cancel: a Setsid child leads its own group too, so the negative pid names
// the same tree.
func KillOnCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
}

// Reap SIGKILLs any group member that outlived cmd's leader — a backgrounded
// grandchild still holding an inherited pipe — on every exit path, normal
// completion included. The group is led by the already-exited child, so the
// signal is ESRCH (ignored) in the common case; a nil Process means the
// command never started and there is nothing to reap.
func Reap(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
