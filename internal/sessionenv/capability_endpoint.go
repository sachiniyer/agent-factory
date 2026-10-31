//go:build !windows

package sessionenv

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// SessionLiveness reports whether a tmux session still exists. known=false means
// tmux could not answer, which is never read as "gone".
type SessionLiveness func(ctx context.Context, session string) (alive, known bool)

const capabilitySweepInterval = 5 * time.Minute

var (
	launchOnce    sync.Once
	launchBroker  *CapabilityBroker
	launchSocket  string
	launchErr     error
	launchStarted atomic.Pointer[CapabilityBroker]
	launchStop    chan struct{}
	launchLn      net.Listener
	launchDir     string
)

// LaunchBrokerIfStarted returns the broker when this process has minted
// anything, and nil otherwise. A teardown that has nothing to revoke must not
// start a listener to find that out.
func LaunchBrokerIfStarted() *CapabilityBroker { return launchStarted.Load() }

// LaunchCapabilities returns this process's broker and the socket it serves
// claims on, starting both on first use. The process that mints is the process
// that serves, which is what keeps a record unforgeable by a same-user child.
// lookup and alive are consulted only on the first call.
func LaunchCapabilities(lookup PaneLookup, alive SessionLiveness) (*CapabilityBroker, string, error) {
	launchOnce.Do(func() {
		launchBroker, launchSocket, launchErr = startLaunchEndpoint(lookup, alive)
		if launchErr == nil {
			launchStarted.Store(launchBroker)
		}
	})
	return launchBroker, launchSocket, launchErr
}

func startLaunchEndpoint(lookup PaneLookup, alive SessionLiveness) (*CapabilityBroker, string, error) {
	dir, err := claimSocketDir()
	if err != nil {
		return nil, "", fmt.Errorf("start launch capability endpoint: %w", err)
	}
	socket := filepath.Join(dir, "claim.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return nil, "", fmt.Errorf("start launch capability endpoint: %w", err)
	}
	_ = os.Chmod(socket, 0o600)
	b := NewCapabilityBroker(lookup)
	stop := make(chan struct{})
	launchStop, launchLn, launchDir = stop, ln, dir
	go b.Serve(ln)
	go func() {
		ticker := time.NewTicker(capabilitySweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				keepClaimEndpointFresh(socket, dir, time.Now())
				if alive != nil {
					ctx, cancel := context.WithTimeout(context.Background(), claimIOTimeout)
					b.Sweep(ctx, alive)
					cancel()
				}
			}
		}
	}()
	return b, socket, nil
}

// keepClaimEndpointFresh marks the claim socket and its directory as recently used.
// They live under os.TempDir(), and on macOS that is /var/folders/xx/yyyy/T/,
// which the system prunes by age: nothing in this process touches the socket after
// bind and a connect does not update it, so a daemon that outlives the prune window
// (days, on a Mac that stays up) would have its endpoint unlinked while a session
// still holds a default-command naming it, and that session's new windows would
// exit 127. Errors are ignored: a path that is already gone is reported by the
// shim's own dial, with the restart hint.
func keepClaimEndpointFresh(socket, dir string, now time.Time) {
	_ = os.Chtimes(socket, now, now)
	_ = os.Chtimes(dir, now, now)
}

// ShutdownLaunchCapabilities stops the endpoint LaunchCapabilities started: it
// closes the claim listener, stops the sweep, and removes the socket directory.
// The endpoint otherwise lives as long as the process; this is for an orderly
// teardown (a test binary asserting it leaves no goroutine behind). Nothing may
// mint or claim afterwards.
func ShutdownLaunchCapabilities() {
	if launchStop == nil {
		return
	}
	close(launchStop)
	_ = launchLn.Close()
	_ = os.RemoveAll(launchDir)
	launchStarted.Store(nil)
	launchStop, launchLn = nil, nil
}

// claimSocketDir makes this process's private socket directory
// (<tmp>/af-claim-<uid>-<pid>, mode 0700) and removes the directories of dead
// processes. The listener is not a trust boundary by itself — the shim
// authenticates the process that answers — so the mode only keeps strangers from
// connecting.
func claimSocketDir() (string, error) {
	prefix := "af-claim-" + strconv.Itoa(os.Getuid()) + "-"
	tmp := os.TempDir()
	if entries, err := os.ReadDir(tmp); err == nil {
		for _, e := range entries {
			pid, ok := strings.CutPrefix(e.Name(), prefix)
			if !ok {
				continue
			}
			if n, err := strconv.Atoi(pid); err == nil && n != os.Getpid() && syscall.Kill(n, 0) == syscall.ESRCH {
				_ = os.RemoveAll(filepath.Join(tmp, e.Name()))
			}
		}
	}
	dir := filepath.Join(tmp, prefix+strconv.Itoa(os.Getpid()))
	// sun_path is 104 bytes on macOS and a long TMPDIR can overrun it, so a path
	// that would not bind falls back to /tmp.
	if len(filepath.Join(dir, "claim.sock")) > 100 {
		tmp = "/tmp"
		dir = filepath.Join(tmp, prefix+strconv.Itoa(os.Getpid()))
	}
	_ = os.RemoveAll(dir) // a reused pid's leftover
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// Sweep drops every active PerPane capability whose session tmux reports gone. It is
// the backstop behind Revoke: a session lost without af asking (the tmux server
// died, a user killed it) never reaches the teardown that revokes, and a PerPane
// record has no clock expiry. A session whose liveness is unknown keeps its record.
func (b *CapabilityBroker) Sweep(ctx context.Context, alive SessionLiveness) {
	b.mu.Lock()
	sessions := make(map[string]bool)
	for _, rec := range b.recs {
		if rec.cap.PerPane && !rec.pending {
			sessions[rec.cap.Session] = true
		}
	}
	b.mu.Unlock()
	for session := range sessions {
		if up, known := alive(ctx, session); known && !up {
			b.revokePerPane(session)
		}
	}
}

// revokePerPane is Revoke limited to PerPane records.
func (b *CapabilityBroker) revokePerPane(session string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for token, rec := range b.recs {
		if rec.cap.PerPane && !rec.pending && rec.cap.Session == session {
			delete(b.recs, token)
		}
	}
}

// Capabilities counts the live records bound to a session, and how many of those
// are PerPane, for tests and diagnostics.
func (b *CapabilityBroker) Capabilities(session string) (total, perPane int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, rec := range b.recs {
		if rec.cap.Session != session {
			continue
		}
		total++
		if rec.cap.PerPane {
			perPane++
		}
	}
	return total, perPane
}
