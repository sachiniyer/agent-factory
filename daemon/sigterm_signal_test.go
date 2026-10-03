package daemon

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestSignalClassifiedDaemon_KillsSameInstance pins the identity-checked
// signal's happy path: a PID locateDaemonPID proved serves this home is
// captured as a proctree.Process at the classification decision (here,
// captureDaemonIdentity) and signaled through proctree.Signal, which
// revalidates the instance immediately before the kill. The same-instance
// daemon is SIGTERM'd and exits, the outcome signalAndWait already produced —
// the identity check is the added safety, not a behaviour change for the
// genuine target (#4793 review).
func TestSignalClassifiedDaemon_KillsSameInstance(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("proctree identity needs /proc")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	ours := spawnFakeDaemonWithHome(t, home)
	proc := captureDaemonIdentity(ours)
	if proc.PID == 0 {
		t.Fatalf("captureDaemonIdentity(ours=%d) returned a zero Process; the live daemon's identity must be capturable to bind the signal", ours)
	}

	if err := signalClassifiedDaemon(ours, proc); err != nil {
		t.Fatalf("signalClassifiedDaemon(ours=%d, proc): %v", ours, err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if !pidLooksAlive(ours) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pidLooksAlive(ours) {
		t.Fatalf("this home's daemon pid=%d did not exit within 8s after signalClassifiedDaemon; the "+
			"identity-checked signal must terminate the same-instance target", ours)
	}
}

// TestSignalClassifiedDaemon_UsesCapturedIdentity pins that signalClassifiedDaemon
// signals through the identity captured at the classification decision (proc),
// not a fresh proctree.Lookup of the PID — so a PID the kernel recycled between
// classification and the signal (the captured StartID no longer matches the
// live process) is refused rather than terminated. signalClassifiedDaemon must
// not re-Lookup the PID (a fresh Lookup would snapshot the recycled replacement
// and signal it), and must not fall back to a PID-only signal when the snapshot
// is unavailable (#4793 review).
func TestSignalClassifiedDaemon_UsesCapturedIdentity(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("proctree identity needs /proc")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	ours := spawnFakeDaemonWithHome(t, home)

	// Capture the live identity, then corrupt its StartID to model a PID the
	// kernel recycled between the classification decision and the signal: the
	// PID is still alive (still this home's daemon) but the captured identity no
	// longer matches the live process, exactly the instance-check failure a
	// recycle produces. signalClassifiedDaemon must refuse the recycled PID
	// (errSignalTargetChanged), not signal it.
	real, err := proctree.Lookup(ours)
	if err != nil {
		t.Fatalf("proctree.Lookup(ours=%d): %v", ours, err)
	}
	stale := real
	stale.StartID = real.StartID + 1

	if err := signalClassifiedDaemon(ours, stale); !errors.Is(err, errSignalTargetChanged) {
		t.Fatalf("signalClassifiedDaemon(ours=%d, stale) err=%v; want errSignalTargetChanged — "+
			"a PID whose captured identity no longer matches the live process must not be signaled, "+
			"even though the PID is still this home's daemon (the snapshot was captured at the "+
			"classification decision and binds the signal to that instance, not a fresh Lookup)", ours, err)
	}
	if !pidLooksAlive(ours) {
		t.Fatalf("this home's daemon pid=%d was killed by signalClassifiedDaemon against a stale identity; "+
			"the identity-checked signal must not terminate a PID whose captured instance no longer matches", ours)
	}
}

// TestSignalProcessChecked_RecycledPIDNotSignaled pins the recycled-PID
// refusal the PID-based signalAndWait could not enforce: a PID locateDaemonPID
// classified as this home's daemon exits and its number is recycled onto
// another process between classification and the signal. signalProcessChecked
// captures a proctree.Process at classification time; proctree.Signal
// revalidates the instance immediately before the kill and refuses
// (ErrIdentityChanged) when the StartID the snapshot captured no longer
// matches the live process, so the recycled PID is not terminated. If the PID
// is still alive (a different process now owns the number, likely another
// home's daemon) signalProcessChecked returns errSignalTargetChanged rather
// than signaling it — the same #4793 boundary the home binding enforces. The
// genuine daemon stays alive (the recycled PID was not its instance).
func TestSignalProcessChecked_RecycledPIDNotSignaled(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("proctree identity needs /proc")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	ours := spawnFakeDaemonWithHome(t, home)

	// Capture the live daemon's real identity, then corrupt its StartID to
	// model a PID the kernel recycled: the same number now names a different
	// process instance than the one classification proved ours.
	real, err := proctree.Lookup(ours)
	if err != nil {
		t.Fatalf("proctree.Lookup(ours=%d): %v", ours, err)
	}
	stale := real
	stale.StartID = real.StartID + 1

	if err := signalProcessChecked(stale); !errors.Is(err, errSignalTargetChanged) {
		t.Fatalf("signalProcessChecked(recycled pid=%d) err=%v; want errSignalTargetChanged — a PID "+
			"whose instance changed between classification and the signal must not be signaled", ours, err)
	}
	if !pidLooksAlive(ours) {
		t.Fatalf("this home's daemon pid=%d was killed by signalProcessChecked against a stale identity; "+
			"the identity-checked signal must not terminate a recycled PID that is no longer the "+
			"instance classification proved ours", ours)
	}
}

// TestSignalProcessChecked_ExitedTargetReturnsNil pins the clean-exit half
// of the identity-change branch: the classified daemon exited on its own
// between classification and the signal (the PID is now gone or a reaped
// zombie). proctree.Signal refuses with ErrIdentityChanged and the PID is no
// longer alive, so signalProcessChecked returns nil — the daemon we proved is
// stopped, the desired outcome — rather than treating the gone PID as a
// recycled target to refuse.
func TestSignalProcessChecked_ExitedTargetReturnsNil(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("proctree identity needs /proc")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	ours := spawnFakeDaemonWithHome(t, home)

	// Capture the live identity, then stop and reap the daemon so the PID no
	// longer names an instance — the shape of a classified daemon that exited
	// before the signal landed.
	real, err := proctree.Lookup(ours)
	if err != nil {
		t.Fatalf("proctree.Lookup(ours=%d): %v", ours, err)
	}
	if err := syscall.Kill(-ours, syscall.SIGKILL); err != nil {
		t.Fatalf("kill daemon: %v", err)
	}
	// Wait for the process group to clear so the PID is gone, not a zombie.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !pidLooksAlive(ours) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pidLooksAlive(ours) {
		t.Fatalf("daemon pid=%d did not exit after SIGKILL; cannot exercise the exited-target branch", ours)
	}

	if err := signalProcessChecked(real); err != nil {
		t.Fatalf("signalProcessChecked(exited pid=%d) err=%v; want nil — a classified daemon that "+
			"exited before the signal is already stopped, so the identity-change refusal returns nil "+
			"rather than treating a gone PID as a recycled target", ours, err)
	}
}
