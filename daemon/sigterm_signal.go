package daemon

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/log"
)

// captureDaemonIdentity snapshots the process-instance identity proctree uses to
// revalidate a PID before signaling it, at the point classifyDaemonHome (or
// pidBelongsToThisHome) just proved the PID serves this home. The snapshot is
// bound to the instance whose home was checked so a PID the kernel recycled
// between this classification and the signal is not terminated: proctree.Signal
// revalidates the StartID and refuses (ErrIdentityChanged) when the PID no
// longer names that instance. A PID that exited before the snapshot (or one the
// backend could not read) returns the zero Process, so signalClassifiedDaemon
// does not fall back to a PID-only signal that could hit the recycled PID
// (#4793).
func captureDaemonIdentity(pid int) proctree.Process {
	proc, err := proctree.Lookup(pid)
	if err != nil {
		return proctree.Process{}
	}
	return proc
}

// errSignalTargetChanged is returned by signalClassifiedDaemon when the PID
// locateDaemonPID classified as this home's daemon exited and its number was
// recycled onto a live process before the signal — proctree.Signal refused to
// signal the replacement, so a recycled PID serving another home is not
// killed. sigtermFallback surfaces it as a scoped failure that does NOT
// recommend the blanket pkill (which would kill the recycled PID).
var errSignalTargetChanged = errors.New("sigterm fallback: classified PID exited and its number was recycled onto a different process before the signal; not signalling a recycled PID")

// signalClassifiedDaemon delivers the SIGTERM→SIGKILL escalation to the PID
// locateDaemonPID proved serves this home, using the proctree.Process identity
// captured AT the classification decision (proc) so the signal is bound to the
// instance whose home classifyDaemonHome proved ours, not a PID the kernel
// recycled between classification and the signal. The previous shape called
// proctree.Lookup here, AFTER locateDaemonPID had classified the numeric PID;
// if the proven same-home daemon exited and that PID was recycled onto a
// foreign daemon before the Lookup, the snapshot observed the replacement and
// signalProcessChecked validated and signaled it successfully — the exact
// #4793 boundary the home binding is there to enforce. Capturing the identity
// at the classification decision closes that window: proctree.Signal
// revalidates the captured StartID immediately before the kill and refuses
// (ErrIdentityChanged) when the PID no longer names the snapshot, so a
// recycled PID serving another home is not terminated.
//
// proc is the snapshot captureDaemonIdentity returned at the classification
// decision. A zero proc (the PID exited before the snapshot, or the backend
// could not read its identity) is NOT signaled by PID: a PID-only signal is
// the unsafe fallback the captured-identity path replaces, so the caller
// surfaces "nothing to signal" (the daemon is already gone) rather than
// signaling whatever the kernel recycled the number onto. signalClassifiedDaemon
// never falls back to signalAndWait; the captured identity is the whole of the
// signal's safety.
func signalClassifiedDaemon(pid int, proc proctree.Process) error {
	if proc.PID == 0 {
		// The classified daemon exited before the identity snapshot, or the
		// backend could not read it. Nothing to signal — and never a PID-only
		// fallback, which would signal a recycled PID (#4793). A dead PID is
		// the desired end state; an unreadable one is fail-closed.
		if !pidLooksAlive(pid) {
			return nil
		}
		// The PID is still alive but its instance could not be bound at
		// classification time. Rather than signal an unbound PID (the recycled-
		// PID hazard), surface the same scoped refusal a recycled PID raises:
		// the caller reports it left untouched and does NOT recommend the
		// blanket pkill.
		return errSignalTargetChanged
	}
	return signalProcessChecked(proc)
}

// signalProcessChecked is the identity-checked SIGTERM→SIGKILL escalation for a
// proctree.Process captured at classification time. proctree.Signal revalidates
// the instance immediately before the kill; on ErrIdentityChanged the PID no
// longer names the snapshot (it exited, or was recycled onto another process),
// and a recycled PID serving another home is not signaled. If the PID is now
// dead the daemon we proved is stopped; if it is alive it is a different
// process this binding did not classify, so it is left untouched.
func signalProcessChecked(proc proctree.Process) error {
	if err := proctree.Signal(proc, syscall.SIGTERM); err != nil {
		if errors.Is(err, proctree.ErrIdentityChanged) {
			if !pidLooksAlive(proc.PID) {
				// The classified daemon exited on its own; it is gone, which
				// is the desired outcome and not a recycled-PID signal.
				return nil
			}
			return errSignalTargetChanged
		}
		return fmt.Errorf("SIGTERM: %w", err)
	}

	deadline := time.Now().Add(sigtermFallbackGrace)
	for time.Now().Before(deadline) {
		if !proctree.AliveSame(proc) {
			return nil
		}
		time.Sleep(sigtermFallbackPoll)
	}

	log.WarningLog.Printf("sigterm fallback: pid %d did not exit within %s; escalating to SIGKILL", proc.PID, sigtermFallbackGrace)
	if err := proctree.Signal(proc, syscall.SIGKILL); err != nil {
		if errors.Is(err, proctree.ErrIdentityChanged) {
			// Exited between the grace poll and SIGKILL — the daemon is gone,
			// which is the desired outcome; a recycled PID is not signaled.
			return nil
		}
		return fmt.Errorf("SIGKILL: %w", err)
	}
	return nil
}

// signalAndWait sends SIGTERM to pid, polls for exit up to
// sigtermFallbackGrace, and escalates to SIGKILL if it has not exited.
func signalAndWait(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("FindProcess: %w", err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		if errIsProcessGone(err) {
			return nil
		}
		return fmt.Errorf("SIGTERM: %w", err)
	}

	deadline := time.Now().Add(sigtermFallbackGrace)
	for time.Now().Before(deadline) {
		if !pidLooksAlive(pid) {
			return nil
		}
		time.Sleep(sigtermFallbackPoll)
	}

	log.WarningLog.Printf("sigterm fallback: pid %d did not exit within %s; escalating to SIGKILL", pid, sigtermFallbackGrace)
	if err := proc.Signal(syscall.SIGKILL); err != nil && !errIsProcessGone(err) {
		return fmt.Errorf("SIGKILL: %w", err)
	}
	return nil
}

// errIsProcessGone reports whether err from Signal indicates the target is
// already gone. POSIX returns ESRCH; os.Process surfaces this as "os: process
// already finished".
func errIsProcessGone(err error) bool {
	if err == nil {
		return false
	}
	if err == os.ErrProcessDone {
		return true
	}
	return strings.Contains(err.Error(), "process already finished") ||
		strings.Contains(err.Error(), "no such process")
}
