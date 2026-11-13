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
// A deadline kills the whole group. What a command that finished on its own
// leaves behind is decided by how its caller ran it: Reap kills every member
// left, and Output and CombinedOutput (capture.go) kill only the members still
// holding one of the command's pipes WaitDelay after the leader exited.
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
	"time"
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
// process exec.CommandContext kills by default. On darwin it also re-signals
// until no member is alive, which reaches a child the group was forking when
// the kill landed (see killGroup). A group with nothing left to kill maps to
// os.ErrProcessDone, which Wait ignores rather than reporting as a command
// failure: one already gone (ESRCH) and, on darwin, one whose members are all
// zombies or exiting, which killpg answers with EPERM. Like kill(2), a nil
// return means the kill reached at least one member. A nil Process — a
// command that never started — is a no-op.
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
		return killGroup(cmd.Process.Pid)
	}
}

// Reap SIGKILLs any group member that outlived cmd's leader — a backgrounded
// grandchild still holding an inherited pipe — on every exit path, normal
// completion included. The group is led by the already-exited child, so the
// signal is ESRCH (ignored) in the common case; a nil Process means the
// command never started and there is nothing to reap. Like KillOnCancel, it
// reaches only a group that cmd's child leads, and on darwin it re-signals
// the same way.
//
// It reaches every member, including one that holds none of the command's
// pipes and is meant to outlive it. A command whose child can open an ssh
// connection must not be reaped this way: under ControlPersist, OpenSSH
// leaves the master's ProxyCommand in the caller's group as the master's
// transport, and this kills it (#7115). Such a command captures its output
// through Output or CombinedOutput instead, which reap only the members proven
// to hold one of the command's pipes past WaitDelay (#7121).
func Reap(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = killGroup(cmd.Process.Pid)
	}
}

// killGroup SIGKILLs every process in the group that pgid leads, including
// a child the group is forking at that instant. A group already gone (ESRCH)
// maps to os.ErrProcessDone.
//
// One kill(-pgid) is enough on Linux, whose fork is atomic against a group
// signal: "Process group and session signals need to be delivered to just
// the parent before the fork or both the parent and the child after the
// fork" (kernel/fork.c, copy_process). XNU gives no such guarantee. A child
// joins its parent's group while it is still being created, and killpg's
// walk of the group skips a process that is still being created
// (pgrp_iterate's proc_find refuses one still marked P_REF_NEW). On
// macos-15, a kill(-pgid, SIGKILL) that landed while a member was forking
// missed the new child in about half the trials, and never on Linux
// (#7066). The parent died on its way out of fork, and the child ran on as
// an orphan, still in the group and still holding every pipe it inherited.
//
// So, where readGroup is defined, killGroup goes on to sweep the group
// (resignal.go). Where readGroup is nil, the kernel gives the guarantee
// itself, and the first kill is all there is.
//
// darwin's killpg also answers EPERM, not ESRCH, when it found no member it
// could signal: its walk skips zombies, and proc_find refuses a member that
// is exiting. killpg1 returns "posix ? EPERM : ESRCH" for that, and
// libsyscall's kill passes posix=1. So a group holding only its exited
// leader, as when a deadline races a command that finished on its own,
// used to make Cancel fail with "operation not permitted" (#7072).
// afterEPERM tells that case apart from a member that refused the signal.
// Linux's kill of a group of zombies succeeds, so there EPERM is always a
// refusal and is returned as it is.
//
// Everything after the first kill finishes within resignalBound, whatever a
// membership read does.
func killGroup(pgid int) error {
	return killGroupWith(pgid, syscall.Kill, readGroup, resignalBound)
}

// killGroupWith is killGroup with its kill, membership read and bound
// injected, so every platform's CI can test the darwin path.
func killGroupWith(pgid int, kill signaller, read groupReader, bound time.Duration) error {
	deadline := time.Now().Add(bound)
	err := kill(-pgid, syscall.SIGKILL)
	switch {
	case errors.Is(err, syscall.ESRCH):
		return os.ErrProcessDone
	case read == nil:
		return err
	case errors.Is(err, syscall.EPERM):
		return afterEPERM(pgid, kill, read, deadline, err)
	case err != nil:
		return err
	}
	sweep(pgid, read, kill, deadline)
	return nil
}
