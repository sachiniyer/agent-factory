package daemon

// Throwaway sandbox lifecycle enforcement (#6844).
//
// A throwaway sandbox — a play-test or audit AF home a scaffold created
// under a scratch directory — declares its own lifetime contract in
// throwaway-sandbox.json at the top of its home. That marker is the only
// identity the enforcement recognizes: it is written by the tooling that
// created the sandbox, never inferred from a path prefix or a process name.
//
// The daemon serving a marked home carries two obligations:
//
//   - It polices its own contract. armThrowawayWatchdog fires when the
//     marker's deadline passes or, when the scaffold declared a creator, the
//     owner is provably dead. On the way out, the daemon tears the whole
//     sandbox down — every process still carrying an environment marker that
//     binds it to the home, plus their process trees — so the driver, the
//     agents, and the private tmux server die with it even after the daemon
//     has been reparented to init.
//   - It sweeps for its siblings. At startup a marked daemon walks the
//     per-user registry of marked homes and reaps each one that is expired
//     or proven orphaned — the "next start of the tooling that creates them"
//     safety net. The sweep is gated on this daemon's own marker: a real
//     install never sweeps, and teardown refuses the default home outright.
//
// Nothing here recognizes a sandbox by /tmp path or argv. Process selection
// is environment-bound (AGENT_FACTORY_HOME / AF_HOME resolving to the marked
// home, or TMUX state under its recorded private TMUX_TMPDIR), every signal
// goes through proctree's identity-checked paths, and anything the code
// cannot prove is left running.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/throwawaysandbox"
	"github.com/sachiniyer/agent-factory/log"
	sessiontmux "github.com/sachiniyer/agent-factory/session/tmux"
)

// throwawayCheckInterval is how often a marked daemon rechecks its declared
// owner's liveness; the deadline itself is armed as a timer.
var throwawayCheckInterval = envDurationOr("AF_THROWAWAY_CHECK_INTERVAL", 30*time.Second)

// The teardown's SIGTERM→SIGKILL escalation budget, small because a
// throwaway has nothing worth preserving.
var (
	throwawayReapGrace    = 500 * time.Millisecond
	throwawayReapTermWait = 3 * time.Second
)

// throwawayKillTimeout bounds each tmux kill-server during teardown so a
// wedged server cannot stall the unwind.
const throwawayKillTimeout = 3 * time.Second

// Seams so the wiring — sweep selection, env binding, owner identity —
// can be driven by fixtures rather than real processes.
var (
	throwawayNowFn         = time.Now
	throwawayRegistryDirFn = func() string { return throwawaysandbox.RegistryDir(os.TempDir(), os.Getuid()) }
	throwawaySnapshotFn    = proctree.Snapshot
	throwawayLookupFn      = proctree.Lookup
	throwawayLookupEnvFn   = proctree.LookupEnv
	throwawayUIDFn         = proctree.UID
	throwawayScopeIDsFn    = func() (boot, ns string) {
		b, _ := proctree.BootID()
		n, _ := proctree.PIDNamespaceID()
		return b, n
	}
	throwawayDefaultHomeFn = func() (string, error) { return config.ConfigDirFor("") }
	throwawayKillSocketFn  = killThrowawayTmuxSocket
	throwawayKillFn        = proctree.KillEscalating
	throwawayTeardownFn    = teardownThrowawaySandbox
)

// loadThrowawayMarker reads home's marker. (marker, true) is the ONLY
// outcome that means "this home is a throwaway sandbox". An unreadable or
// invalid marker is unproven, not a sandbox; a marker on the default AF home
// is refused outright — the real install is never a sandbox, whatever a file
// claims.
func loadThrowawayMarker(home string) (*throwawaysandbox.Marker, bool) {
	m, err := throwawaysandbox.LoadMarker(home)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.WarningLog.Printf("throwaway sandbox marker at %s is unreadable; treating the home as unmarked: %v", home, err)
		}
		return nil, false
	}
	if isDefaultThrowawayHome(home) {
		log.ErrorLog.Printf("refusing the throwaway sandbox marker on the default AF home %s; the real install is never a sandbox", home)
		return nil, false
	}
	return m, true
}

// isDefaultThrowawayHome reports whether home is the default install's home.
// When the comparison cannot be made it answers true — an unresolvable
// default must never be treated as a sandbox, the same direction every
// other unproven case here takes.
func isDefaultThrowawayHome(home string) bool {
	def, err := throwawayDefaultHomeFn()
	if err != nil {
		return true
	}
	canonHome, err1 := canonicalDir(home)
	canonDef, err2 := canonicalDir(def)
	if err1 != nil || err2 != nil {
		return true
	}
	return canonHome == canonDef
}

// registerThrowawaySandbox lists home in the per-user registry so the next
// marked daemon start can find it. Best-effort: a home that cannot be
// registered is simply never swept — its own daemon still enforces the
// contract.
func registerThrowawaySandbox(home string) {
	canon, err := canonicalDir(home)
	if err != nil {
		log.WarningLog.Printf("throwaway sandbox home %s could not be canonicalized for registration: %v", home, err)
		return
	}
	if err := throwawaysandbox.WriteRegistryEntry(throwawayRegistryDirFn(), canon); err != nil {
		log.WarningLog.Printf("throwaway sandbox home %s could not be registered; a later marked start cannot sweep it: %v", home, err)
	}
}

// armThrowawayWatchdog starts the sandbox's self-enforcement: fire closes
// when the marker's deadline passes or, when an owner was declared, the
// owner is proven dead. The returned func stops the watcher without firing.
// An already-expired marker fires immediately — a sandbox discovered past
// its deadline does not get a second start.
func armThrowawayWatchdog(m *throwawaysandbox.Marker, fire chan<- struct{}) func() {
	stop := make(chan struct{})
	var once sync.Once
	expire := func(reason string) {
		once.Do(func() {
			log.InfoLog.Printf("throwaway sandbox (kind %s): %s; ending the daemon and tearing the sandbox down", m.Kind, reason)
			close(fire)
		})
	}
	go func() {
		wait := m.Deadline().Sub(throwawayNowFn())
		if wait <= 0 {
			expire(fmt.Sprintf("creation deadline passed %s ago", (-wait).Round(time.Second)))
			return
		}
		deadline := time.NewTimer(wait)
		defer deadline.Stop()
		var ownerC <-chan time.Time
		var ownerTick *time.Ticker
		if m.OwnerPID > 0 {
			ownerTick = time.NewTicker(throwawayCheckInterval)
			defer ownerTick.Stop()
			ownerC = ownerTick.C
		}
		for {
			select {
			case <-stop:
				return
			case <-deadline.C:
				expire(fmt.Sprintf("reached its %ds lifetime", m.MaxLifetimeSeconds))
				return
			case <-ownerC:
				if throwawayOwnerDead(m) {
					expire(fmt.Sprintf("declared owner pid %d is gone", m.OwnerPID))
					return
				}
			}
		}
	}()
	return func() { close(stop) }
}

// throwawayOwnerDead reports whether the marker's declared owner is PROVEN
// gone. The verdicts mirror the orphan-sweep stamp logic: a process the code
// cannot look at, a pid in another namespace, or an unreadable scope is
// "unknown", and unknown is never dead.
func throwawayOwnerDead(m *throwawaysandbox.Marker) bool {
	if m.OwnerPID <= 0 {
		return false
	}
	curBoot, curNs := throwawayScopeIDsFn()
	// A stamp from another pid namespace names a number space this process
	// cannot read — the owner may be alive there.
	if m.OwnerNsID != "" && curNs != "" && m.OwnerNsID != curNs {
		return false
	}
	// Different boots are proof of death only when both ids are the kernel's
	// strong UUID; the pidns fallback can disagree across a shared namespace.
	if m.OwnerBootID != "" && curBoot != "" && m.OwnerBootID != curBoot &&
		!proctree.BootIDIsFallback(m.OwnerBootID) && !proctree.BootIDIsFallback(curBoot) {
		return true
	}
	cur, err := throwawayLookupFn(m.OwnerPID)
	switch {
	case errors.Is(err, proctree.ErrProcessExited), errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ESRCH):
		return true
	case err != nil:
		return false // could not look is not dead
	case m.OwnerStartID != 0 && cur.StartID != m.OwnerStartID:
		return true
	default:
		return false
	}
}

// sweepThrowawaySandboxes reaps registered sandbox homes that are expired or
// proven orphaned. It runs at daemon startup, only in a marked daemon — the
// marker gate is what makes "the next start of the tooling that creates
// them" rather than "any af start" the reaper.
func sweepThrowawaySandboxes(selfHome string) {
	if _, marked := loadThrowawayMarker(selfHome); !marked {
		return
	}
	dir := throwawayRegistryDirFn()
	homes, malformed, err := throwawaysandbox.ReadRegistryHomes(dir)
	if err != nil {
		log.WarningLog.Printf("throwaway sandbox registry %s could not be read; sweep skipped: %v", dir, err)
		return
	}
	if malformed > 0 {
		log.WarningLog.Printf("throwaway sandbox registry holds %d unreadable entries; skipping them", malformed)
	}
	selfCanonical, err := canonicalDir(selfHome)
	if err != nil {
		log.WarningLog.Printf("cannot canonicalize own home %s; throwaway sandbox sweep skipped: %v", selfHome, err)
		return
	}
	now := throwawayNowFn()
	for _, registered := range homes {
		home, err := canonicalDir(registered)
		if err != nil {
			log.WarningLog.Printf("throwaway sandbox registry entry %s is unresolvable; dropping it: %v", registered, err)
			_ = throwawaysandbox.RemoveRegistryEntry(dir, registered)
			continue
		}
		if home == selfCanonical {
			continue
		}
		m, err := throwawaysandbox.LoadMarker(home)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// The contract itself is gone — the home or its marker.
				// Dropping the registration is bookkeeping only.
				_ = throwawaysandbox.RemoveRegistryEntry(dir, registered)
			} else {
				log.WarningLog.Printf("throwaway sandbox %s has an unreadable marker; leaving it in place: %v", home, err)
			}
			continue
		}
		var reason string
		switch {
		case m.Expired(now):
			reason = fmt.Sprintf("exceeded its %ds lifetime", m.MaxLifetimeSeconds)
		case throwawayOwnerDead(m):
			reason = fmt.Sprintf("declared owner pid %d is gone", m.OwnerPID)
		default:
			continue
		}
		log.InfoLog.Printf("reaping throwaway sandbox %s (kind %s): %s", home, m.Kind, reason)
		throwawayTeardownFn(home, m)
		if err := throwawaysandbox.RemoveRegistryEntry(dir, registered); err != nil {
			log.WarningLog.Printf("throwaway sandbox %s could not be deregistered after teardown: %v", home, err)
		}
	}
}

// teardownThrowawaySandbox ends everything the marked home can claim. The
// order matters: tmux servers on the recorded private sockets first, so the
// pane trees get SIGHUP through the normal channel, then every process still
// environment-bound to the home — including the driver that spawned it and
// the daemon serving it when the sweep runs — plus their trees.
func teardownThrowawaySandbox(home string, m *throwawaysandbox.Marker) {
	canonHome, err := canonicalDir(home)
	if err != nil {
		log.WarningLog.Printf("cannot canonicalize throwaway sandbox home %s for teardown: %v", home, err)
		return
	}
	if isDefaultThrowawayHome(canonHome) {
		log.ErrorLog.Printf("refusing to tear down the default AF home %s", canonHome)
		return
	}
	killThrowawayTmuxSockets(canonHome, m)
	procs, unresolved := throwawayBoundProcesses(canonHome, m)
	if len(procs) == 0 {
		return
	}
	remaining := throwawayKillFn(procs, throwawayReapGrace, throwawayReapTermWait, func(outcome proctree.ReapOutcome, format string, args ...any) {
		if outcome == proctree.ReapSignalled {
			log.InfoLog.Printf("throwaway teardown: "+format, args...)
		} else {
			log.WarningLog.Printf("throwaway teardown: "+format, args...)
		}
	})
	if len(remaining) > 0 {
		pids := make([]string, 0, len(remaining))
		for _, p := range remaining {
			pids = append(pids, fmt.Sprintf("%d", p.PID))
		}
		log.WarningLog.Printf("throwaway teardown of %s left %d processes after SIGKILL: %s", home, len(remaining), strings.Join(pids, ","))
	}
	if unresolved > 0 {
		log.InfoLog.Printf("throwaway teardown of %s left %d processes whose environment could not be read; unproven processes are left alone", home, unresolved)
	}
}

// throwawayPrivateTmuxDir returns the marker's recorded TMUX_TMPDIR when it
// proves private to the sandbox: canonicalized, strictly inside the recorded
// sandbox root, and with the root enclosing the marked home itself. Every
// consumer — socket teardown AND tmux-env process selection — must pass this
// proof before trusting the dir: a marker that recorded a shared dir such as
// /tmp would otherwise bind or kill other users' — and this install's real —
// tmux sockets, so those shapes are refused outright. The second return is
// the refusal reason for logging; empty means the dir is proven private.
func throwawayPrivateTmuxDir(canonHome string, m *throwawaysandbox.Marker) (string, string) {
	if m.TmuxTmpdir == "" || m.SandboxRoot == "" {
		return "", ""
	}
	root, err := canonicalDir(m.SandboxRoot)
	if err != nil {
		return "", ""
	}
	tmuxDir, err := canonicalDir(m.TmuxTmpdir)
	if err != nil {
		return "", ""
	}
	sep := string(os.PathSeparator)
	if sysTmp, err := canonicalDir(os.TempDir()); err == nil && (root == sysTmp || root == sep) {
		return "", fmt.Sprintf("sandbox root %s is a shared directory", root)
	}
	if canonHome != root && !strings.HasPrefix(canonHome, root+sep) {
		return "", fmt.Sprintf("marked home %s is not inside recorded sandbox root %s", canonHome, root)
	}
	if tmuxDir == root || !strings.HasPrefix(tmuxDir, root+sep) {
		return "", fmt.Sprintf("recorded tmux dir %s is not inside sandbox root %s", tmuxDir, root)
	}
	return tmuxDir, ""
}

// killThrowawayTmuxSockets runs kill-server on every unix socket under the
// marker's private TMUX_TMPDIR. Privacy is proven by
// throwawayPrivateTmuxDir first; a refused dir leaves the env-bound
// teardown to do the work instead.
func killThrowawayTmuxSockets(canonHome string, m *throwawaysandbox.Marker) {
	tmuxDir, refuse := throwawayPrivateTmuxDir(canonHome, m)
	if tmuxDir == "" {
		if refuse != "" {
			log.WarningLog.Printf("refusing tmux teardown: %s", refuse)
		}
		return
	}
	var sockets []string
	_ = filepath.WalkDir(tmuxDir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type()&os.ModeSocket != 0 {
			sockets = append(sockets, path)
		}
		return nil
	})
	for _, sock := range sockets {
		if err := throwawayKillSocketFn(sock); err != nil {
			log.WarningLog.Printf("throwaway teardown could not kill tmux server on %s: %v", sock, err)
		}
	}
}

// killThrowawayTmuxSocket asks the server on sock to exit, bounded so a
// wedged server cannot stall the teardown.
func killThrowawayTmuxSocket(sock string) error {
	if _, err := exec.LookPath("tmux"); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), throwawayKillTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", "-S", sock, "kill-server")
	cmd.WaitDelay = throwawayKillTimeout
	return cmd.Run()
}

// throwawayBoundProcesses returns the live process set environment-bound to
// canonHome — every bound process plus its whole tree in the snapshot. The
// daemon calling this is always excluded (it excludes itself when running
// its own teardown; when the sweep runs, its own home never binds to the
// target home). unresolved counts processes whose environment could not be
// read at all — "could not look" is never a kill.
func throwawayBoundProcesses(canonHome string, m *throwawaysandbox.Marker) (bound []proctree.Process, unresolved int) {
	snap, err := throwawaySnapshotFn()
	if err != nil {
		log.WarningLog.Printf("throwaway teardown cannot snapshot the process table; leaving %s in place: %v", canonHome, err)
		return nil, 0
	}
	self := os.Getpid()
	// The recorded tmux dir binds processes only when it is proven private
	// to the sandbox — a shared dir like /tmp must never make unrelated
	// tmux sessions part of the kill set (#6844 review).
	canonTmux, _ := throwawayPrivateTmuxDir(canonHome, m)
	seen := make(map[int]bool)
	for pid := range snap {
		if pid == self {
			continue
		}
		ok, proven := processBoundToThrowawayHome(pid, canonHome, canonTmux)
		if !proven {
			unresolved++
			continue
		}
		if !ok {
			continue
		}
		for _, d := range proctree.TreeOf(snap, pid) {
			if d.PID == self || seen[d.PID] {
				continue
			}
			seen[d.PID] = true
			bound = append(bound, d)
		}
	}
	return bound, unresolved
}

// processBoundToThrowawayHome reports whether pid's environment proves it
// belongs to the marked home: AGENT_FACTORY_HOME or AF_HOME resolving to it,
// or — only when the marker recorded a private TMUX_TMPDIR — TMUX_TMPDIR
// resolving to it or a TMUX socket under it. proven=false means every
// environment read failed, which is "could not look", never a kill.
func processBoundToThrowawayHome(pid int, canonHome, canonTmux string) (bound bool, proven bool) {
	if uid, ok := throwawayUIDFn(pid); !ok || uid != os.Getuid() {
		return false, false
	}
	if v, st := throwawayLookupEnvFn(pid, "AGENT_FACTORY_HOME"); st != proctree.EnvUnknown {
		proven = true
		if st == proctree.EnvFound {
			if h, err := config.ConfigDirFor(v); err == nil {
				if c, err := canonicalDir(h); err == nil && c == canonHome {
					return true, true
				}
			}
		}
	}
	if v, st := throwawayLookupEnvFn(pid, sessiontmux.EnvMarkerHome); st != proctree.EnvUnknown {
		proven = true
		if st == proctree.EnvFound {
			if c, err := canonicalDir(v); err == nil && c == canonHome {
				return true, true
			}
		}
	}
	if canonTmux != "" {
		sep := string(os.PathSeparator)
		if v, st := throwawayLookupEnvFn(pid, "TMUX_TMPDIR"); st != proctree.EnvUnknown {
			proven = true
			if st == proctree.EnvFound {
				if c, err := canonicalDir(v); err == nil && c == canonTmux {
					return true, true
				}
			}
		}
		if v, st := throwawayLookupEnvFn(pid, "TMUX"); st != proctree.EnvUnknown {
			proven = true
			if st == proctree.EnvFound {
				if sock, _, _ := strings.Cut(v, ","); sock != "" {
					if c, err := canonicalDir(sock); err == nil && strings.HasPrefix(c, canonTmux+sep) {
						return true, true
					}
				}
			}
		}
	}
	return false, proven
}
