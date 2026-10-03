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

// errSignalTargetChanged is returned by signalClassifiedDaemon when the PID
// locateDaemonPID classified as this home's daemon exited and its number was
// recycled onto a live process before the signal — proctree.Signal refused to
// signal the replacement, so a recycled PID serving another home is not
// killed. sigtermFallback surfaces it as a scoped failure that does NOT
// recommend the blanket pkill (which would kill the recycled PID).
var errSignalTargetChanged = errors.New("sigterm fallback: classified PID exited and its number was recycled onto a different process before the signal; not signalling a recycled PID")

// signalClassifiedDaemon delivers the SIGTERM→SIGKILL escalation to the PID
// locateDaemonPID proved serves this home, capturing a proctree.Process
// identity at classification time so the signal lands on the same instance
// classifyDaemonHome proved ours, not a recycled PID a different home's daemon
// acquired between classification and the signal. signalAndWait (used by
// stopDaemon's other callers) signals by PID alone, which a recycled PID
// survives: the old os.Process.Signal has no instance check, so a PID the
// kernel recycled onto another home's daemon is terminated and the fallback
// reports success against the wrong home — the same #4793 boundary the home
// binding is there to enforce. proctree.Signal revalidates the identity
// immediately before the kill and refuses (ErrIdentityChanged) when the PID no
// longer names the snapshot; a recycled PID is left untouched.
//
// On a platform where proctree identity is unavailable (an unsupported
// backend, or a transient /proc read failure), the PID-based signalAndWait
// path is the fallback: on such a platform locateDaemonPID could not have
// classified the PID ours to begin with (classifyDaemonHome reads /proc), so
// reaching this fallback is rare and degrades to the existing behaviour.
func signalClassifiedDaemon(pid int) error {
	proc, err := proctree.Lookup(pid)
	if err != nil {
		if errors.Is(err, proctree.ErrProcessExited) {
			// The daemon exited between classification and here; nothing to
			// signal — the same outcome signalAndWait gives for ESRCH, and a
			// recycled PID is not signaled.
			return nil
		}
		log.WarningLog.Printf("sigterm fallback: could not capture process identity for pid %d (%v); signaling by PID", pid, err)
		return signalAndWait(pid)
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
