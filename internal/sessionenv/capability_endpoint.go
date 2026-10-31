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

// capabilityEnsureInterval is how often the endpoint checks that its socket path
// still exists. It is a bare Lstat, so it can be frequent: a window opened between
// a system temp cleanup and the next check fails, so the gap is kept short.
var capabilityEnsureInterval = 10 * time.Second

var (
	launchOnce    sync.Once
	launchBroker  *CapabilityBroker
	launchSocket  string
	launchErr     error
	launchStarted atomic.Pointer[CapabilityBroker]
	launchStop    chan struct{}
	launchEP      atomic.Pointer[claimEndpoint]
)

// claimEndpoint is the listener behind the claim socket. It owns re-creating the
// socket when something outside the process removes it.
type claimEndpoint struct {
	mu     sync.Mutex
	b      *CapabilityBroker
	socket string
	dir    string
	ln     net.Listener
}

// ensure re-creates the socket, and its directory, at the SAME path when either has
// been removed. The path is baked into every account session's default-command, so
// it must keep answering for the life of the process. macOS prunes /var/folders/*/T
// by age, and a Mac that slept through the prune window runs that cleanup on wake,
// before any keep-alive tick can touch the socket (#6384).
func (e *claimEndpoint) ensure() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ln == nil {
		return
	}
	if _, err := os.Lstat(e.socket); err == nil {
		return
	}
	// The old listener is bound to an unlinked inode. Close it without unlinking:
	// the path may by now name something else, and it is about to be ours again.
	if ul, ok := e.ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	_ = e.ln.Close()
	// The directory's mode is the only thing keeping other users from the tokens in
	// default-command, and the path is predictable (/tmp/af-claim-<uid>-<pid> when the
	// temp dir is long): after a cleanup another user may have planted something
	// there. Verify before EVERY bind, and fail closed. Leaving the closed listener
	// in place makes the next tick retry from here.
	if err := prepareClaimDir(e.dir); err != nil {
		return
	}
	ln, err := net.Listen("unix", e.socket)
	if err != nil {
		return
	}
	_ = os.Chmod(e.socket, 0o600)
	e.ln = ln
	go e.b.Serve(ln)
}

// prepareClaimDir creates dir (mode 0700) or accepts one that already exists, and
// either way requires what it will bind in to be ours: a real directory (not a
// symlink), owned by this uid, with mode exactly 0700. os.Mkdir, never MkdirAll, so
// an existing path is inspected rather than adopted; an existing directory's mode is
// never "fixed", because a directory someone else made is not ours to repair.
func prepareClaimDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err == nil {
		// Mkdir applies the umask; a restrictive one must not leave 0500 behind.
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	} else if !os.IsExist(err) {
		return err
	}
	return verifyClaimDir(dir)
}

func verifyClaimDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("claim directory %s is not a plain directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("claim directory %s is not owned by this user", dir)
	}
	if fi.Mode().Perm() != 0o700 || fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Errorf("claim directory %s has mode %v, want 0700", dir, fi.Mode())
	}
	return nil
}

func (e *claimEndpoint) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ln != nil {
		_ = e.ln.Close()
		e.ln = nil
	}
	_ = os.RemoveAll(e.dir)
}

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
	if ep := launchEP.Load(); ep != nil {
		ep.ensure()
	}
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
	ep := &claimEndpoint{b: b, socket: socket, dir: dir, ln: ln}
	launchStop = stop
	launchEP.Store(ep)
	go b.Serve(ln)
	ensure := time.NewTicker(capabilityEnsureInterval) // read here, not in the goroutine
	go func() {
		ticker := time.NewTicker(capabilitySweepInterval)
		defer ticker.Stop()
		defer ensure.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ensure.C:
				ep.ensure()
			case <-ticker.C:
				ep.ensure()
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
	// Chtimes follows symlinks; only touch a directory that is still ours.
	if verifyClaimDir(dir) != nil {
		return
	}
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
	if ep := launchEP.Swap(nil); ep != nil {
		ep.close()
	}
	launchStarted.Store(nil)
	launchStop = nil
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
	if err := prepareClaimDir(dir); err != nil {
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
