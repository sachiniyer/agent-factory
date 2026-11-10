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
//
// Callers must meet two requirements that the types do not enforce:
//
//   - The command must be built with exec.CommandContext, not exec.Command.
//     Lead and KillOnCancel install a Cancel, and os/exec's Start refuses a
//     Cancel on a command with no context: "exec: command with a non-nil
//     Cancel was not created with CommandContext".
//   - The child must lead its own process group. Lead arranges that with
//     Setpgid, and a Setsid child leads one too. KillOnCancel on a child that
//     stayed in its parent's group is a silent no-op: the negative pid names
//     no group, the kill answers ESRCH, Cancel reports os.ErrProcessDone, and
//     nothing is killed. That is safe, because it cannot reach any other
//     group, but the child outlives the deadline. Only WaitDelay's fallback
//     Process.Kill ends it, if the caller set a WaitDelay, and that never
//     reaches anything the child spawned. Reap has the same requirement.
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
// cmd must come from exec.CommandContext, or Start fails (see the package
// doc).
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
// the same tree. A child that leads no group of its own gets nothing from
// it: the kill answers ESRCH and Cancel silently kills nothing. cmd must also
// come from exec.CommandContext, or Start fails. The package doc states both
// requirements.
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
// command never started and there is nothing to reap. Like KillOnCancel, it
// reaches only a group that cmd's child leads.
func Reap(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
