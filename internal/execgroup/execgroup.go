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
// the kill landed (see killGroup). A group already gone (ESRCH)
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
func Reap(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = killGroup(cmd.Process.Pid)
	}
}

// resignalBound caps how long killGroup keeps re-signalling a group that
// still has a live member. A SIGKILLed process is normally a zombie within
// milliseconds, so the bound only matters for a member stuck in an
// uninterruptible wait, which no signal ends: without it, Cancel would hold
// os/exec's Wait for as long as that wait lasts.
const resignalBound = 2 * time.Second

// resignalPoll is the pause between two reads of the group's membership.
const resignalPoll = time.Millisecond

// killGroup SIGKILLs every process in the group that pgid leads, including
// a child the group is forking at that instant. A group already gone (ESRCH)
// maps to os.ErrProcessDone.
//
// One kill(-pgid) is enough on Linux, whose fork is atomic against a group
// signal: "Process group and session signals need to be delivered to just
// the parent before the fork or both the parent and the child after the
// fork" (kernel/fork.c, copy_process). XNU gives no such guarantee. On
// macos-15, a kill(-pgid, SIGKILL) that landed while a member was forking
// missed the new child in about half the trials, and never on Linux (#7066).
// The parent died on its way out of fork, and the child ran on as an orphan,
// still in the group and still holding every pipe it inherited. A second
// kill of the group killed it every time.
//
// So, where groupHasLiveMember is defined, killGroup re-signals the group
// until no member of it is alive: every member left is a zombie, or there
// are none. That condition also means no fork is still in flight. A member
// that is forking is not a zombie, and by the time it becomes one, its fork
// has returned and the new child is listed in the group. Where
// groupHasLiveMember is nil, the kernel gives the guarantee itself and the
// first kill is all there is.
func killGroup(pgid int) error {
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	if groupHasLiveMember != nil {
		resignal(pgid, groupHasLiveMember, resignalBound)
	}
	return nil
}

// resignal re-sends SIGKILL to the group pgid leads for as long as live
// reports a live member in it, and for at most bound. A read that finds a
// live member does not pin the group. If that member exits and is reaped
// before the re-signal, and the leader has been reaped too (os/exec's Wait
// reaps it without waiting for Cancel), the pgid is free when the signal
// goes out. To hit another group, XNU's sequential pid allocator would then
// have to wrap round to this exact number, and the process given it would
// have to lead its own group, all in the microseconds between the read and
// the kill. Reap's single kill after Wait has always had that same window.
// darwin offers no handle that pins a group, which is what closing it would
// take. A failed read ends the loop: the first kill has already gone out,
// and that kill is everything a kernel with an atomic fork needs.
func resignal(pgid int, live func(pgid int) (bool, error), bound time.Duration) {
	deadline := time.Now().Add(bound)
	for {
		alive, err := live(pgid)
		if err != nil || !alive || !time.Now().Before(deadline) {
			return
		}
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		time.Sleep(resignalPoll)
	}
}
