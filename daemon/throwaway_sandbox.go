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
//     sandbox down — every process that independently proves membership in
//     this sandbox instance — so the driver, the agents, and the private
//     tmux server die with it even after the daemon has been reparented to
//     init.
//   - It sweeps for its siblings. At startup a marked daemon walks the
//     per-user registry of marked homes and reaps each one that is expired
//     or proven orphaned — the "next start of the tooling that creates them"
//     safety net. The sweep is gated on this daemon's own marker: a real
//     install never sweeps, and teardown refuses the default home outright.
//
// Membership is proven per process, never inherited: a pid is a member only
// when its own environment binds it to the marked home (or to the marker's
// proven-private tmux dir), it started no earlier than the marker it is
// being killed under, it shares this process's filesystem root, and it is
// not shared infrastructure — a tmux server whose socket is not the
// sandbox's private dir, or any process holding a named socket outside the
// sandbox's own storage. Descent from a member is not proof, and neither is
// an environment a shared server merely inherited. Every signal still goes
// through proctree's identity-checked paths, and anything the code cannot
// prove is left running.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// The startup sweep runs inside daemon startup, so it is bounded in both
// directions: throwawaySweepMaxHomes caps how many registry entries one
// start will reap, and throwawaySweepBudget caps the wall clock. Remainders
// are left for the next marked start — orphan reaping is convergence, not a
// gate on readiness.
const throwawaySweepMaxHomes = 64

var throwawaySweepBudget = 30 * time.Second

// Seams so the wiring — sweep selection, env binding, owner identity —
// can be driven by fixtures rather than real processes.
var (
	throwawayNowFn         = time.Now
	throwawayRegistryDirFn = func() string { return throwawaysandbox.RegistryDir(os.TempDir(), os.Getuid()) }
	throwawaySnapshotFn    = proctree.Snapshot
	throwawayLookupFn      = proctree.Lookup
	throwawayLookupEnvFn   = proctree.LookupEnv
	throwawayUIDFn         = proctree.UID
	throwawayWorkingDirFn  = proctree.WorkingDir
	throwawaySameRootFn    = sameProcessRoot
	// Server-identification seams: a process that IS a shared server is
	// never a sandbox member no matter what its environment says, and the
	// tmux-server shape needs its own socket-path proof because argv can be
	// retitled (tmux: server) or flag-free (-L only, BSD rc.d).
	throwawayIsTmuxServerFn    = proctree.IsTmuxServer
	throwawayArgvFn            = proctree.Argv
	throwawayUnixSocketPathsFn = unixSocketPathsOf
	throwawayScopeIDsFn        = func() (boot, ns string) {
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
	started := time.Now()
	for i, registered := range homes {
		if i >= throwawaySweepMaxHomes || time.Since(started) > throwawaySweepBudget {
			log.WarningLog.Printf("throwaway sandbox sweep bound reached at %d entries; %d remain for the next marked start", i, len(homes)-i)
			break
		}
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
// order matters: the member set is captured FIRST, before the recorded
// private tmux servers die — a member session or pane must be proven while
// the socket evidence still exists, and the tmux kill that follows gives
// the pane trees SIGHUP through the normal channel (the same lesson
// session/tmux/reap.go encodes). Then every captured process — including
// the driver that spawned the sandbox and the daemon serving it when the
// sweep runs — is signalled by identity. Members the SIGHUP already reaped
// are skipped by the identity check, never re-killed.
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
	procs, unresolved := throwawayBoundProcesses(canonHome, m)
	killThrowawayTmuxSockets(canonHome, m)
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
// proves private to the sandbox — an ALLOWLIST, not a denylist. The dir is
// trusted only when the recorded shape is the one the scaffolding actually
// creates: a sandbox root that contains the marked home itself, and the tmux
// dir strictly inside that root. Neither the root nor the dir may be — or
// contain — any directory other users' tmux servers (or this install's real
// one) could be serving from: the real tmux socket directories under every
// plausible base, the shared tmp bases themselves, and the user's home.
// That refusal is computed from the box's fixed shapes, never from the
// reaper's own TMPDIR — a daemon started with TMPDIR=/var/tmp must refuse
// /tmp exactly as hard as one started under /tmp. Absent or untrusted
// metadata means NO tmux teardown: the empty first return with an empty
// reason is simply "nothing recorded". The second return is the refusal
// reason for logging; empty means the dir is proven private.
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
	if refuse := sharedDirCollision(root, "sandbox root"); refuse != "" {
		return "", refuse
	}
	if canonHome != root && !strings.HasPrefix(canonHome, root+sep) {
		return "", fmt.Sprintf("marked home %s is not inside recorded sandbox root %s", canonHome, root)
	}
	if refuse := sharedDirCollision(tmuxDir, "recorded tmux dir"); refuse != "" {
		return "", refuse
	}
	if tmuxDir == root || !strings.HasPrefix(tmuxDir, root+sep) {
		return "", fmt.Sprintf("recorded tmux dir %s is not inside sandbox root %s", tmuxDir, root)
	}
	return tmuxDir, ""
}

// sharedDirCollision refuses when dir IS a shared directory or an ANCESTOR
// of one: sockets under an ancestor's subtree include the shared dir itself,
// so walking/killing through it is the same hazard as naming it.
func sharedDirCollision(dir, what string) string {
	sep := string(os.PathSeparator)
	for _, shared := range throwawaySharedDirs() {
		if dir == shared || strings.HasPrefix(shared, dir+sep) {
			return fmt.Sprintf("%s %s is or contains shared directory %s", what, dir, shared)
		}
	}
	return ""
}

// throwawaySharedDirs enumerates every directory a legitimate shared or
// real tmux socket can live under, canonicalized: the fixed tmp bases (not
// just os.TempDir — the answer must not depend on the reaper's TMPDIR), the
// per-user tmux-<uid> socket dir under each of them plus the reaper's own
// TMUX_TMPDIR, and the user's home with its conventional ~/.tmux dir. A
// base that cannot be canonicalized is skipped — an unresolvable base cannot
// host a socket the marker could reach anyway.
func throwawaySharedDirs() []string {
	uid := strconv.Itoa(os.Getuid())
	bases := []string{"/", "/tmp", "/var/tmp", os.TempDir()}
	if env := os.Getenv("TMUX_TMPDIR"); env != "" {
		bases = append(bases, env)
	}
	var dirs []string
	add := func(p string) {
		if c, err := canonicalDir(p); err == nil {
			dirs = append(dirs, c)
		}
	}
	for _, b := range bases {
		add(b)
		add(filepath.Join(b, "tmux-"+uid))
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(home)
		add(filepath.Join(home, ".tmux"))
		add(filepath.Join(home, ".tmux", "tmux-"+uid))
	}
	return dirs
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

// throwawayBoundProcesses returns the processes that independently prove
// membership in this sandbox instance — no trees, no descent. Each pid must
// satisfy processMemberOfSandbox on its own evidence, and the instance the
// snapshot described must still own the pid when it is selected (a pid
// recycled between snapshot and selection is skipped, not killed). The
// daemon calling this always excludes itself by pid: a daemon serving the
// marked home IS bound to it and must not signal itself mid-teardown, and
// when the sweep runs its own home never binds to the target anyway.
// unresolved counts processes whose evidence could not be read at all —
// "could not look" is never a kill.
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
	for pid, p := range snap {
		if pid == self {
			continue
		}
		member, proven := processMemberOfSandbox(pid, p, canonHome, canonTmux, m.CreatedUnix)
		if !proven {
			unresolved++
			continue
		}
		if !member {
			continue
		}
		// The environment was read AFTER the snapshot: re-lookup the pid and
		// require the same instance. A pid that exited or recycled between
		// the two reads names a different process than the one whose
		// environment was just proven — it is skipped, never signalled.
		cur, err := throwawayLookupFn(pid)
		if err != nil || cur.StartID != p.StartID {
			continue
		}
		bound = append(bound, p)
	}
	return bound, unresolved
}

// processMemberOfSandbox decides whether pid is a member of THIS sandbox
// instance. Membership is per-process proof, not lineage:
//
//   - the process must share this process's filesystem root — an env path
//     read under a different root/mount namespace resolves against a
//     different filesystem, so nothing about it can be proven;
//   - its own environment must bind it to the marked home or the proven
//     private tmux dir — descent from a bound parent, or a binding another
//     process happened to inherit, is not proof of anything;
//   - it must not have started before the marker was written — the marker
//     names an instance, not a path, and a process that predates it cannot
//     belong to the sandbox it declares;
//   - and it must not be shared infrastructure — a tmux server whose socket
//     is not inside the sandbox's private dir, or any process holding a
//     named unix socket outside the sandbox's own storage. A server the
//     sandbox environment spawned but did not create inside its own private
//     tmux directory is never a member: killing it would take the real
//     install's panes — or another user's — with it.
//
// proven=false means the process could not be evaluated at all (foreign
// uid, foreign root, unreadable environment) — unknown is never a member
// and never a kill.
func processMemberOfSandbox(pid int, p proctree.Process, canonHome, canonTmux string, createdUnix int64) (member, proven bool) {
	if !throwawaySameRootFn(pid) {
		return false, false
	}
	envBound, proven := processBoundToThrowawayHome(pid, canonHome, canonTmux)
	if !proven || !envBound {
		return false, proven
	}
	if p.StartedAt.IsZero() || p.StartedAt.Unix() < createdUnix {
		return false, true
	}
	if throwawaySharedInfraProcess(pid, canonHome, canonTmux) {
		return false, true
	}
	return true, true
}

// throwawaySharedInfraProcess reports whether pid is shared infrastructure
// the sandbox must never claim, regardless of what its environment binds it
// to. Two shapes qualify: a tmux server whose socket is not proven inside
// the sandbox's private dir (the production server spawned from a sandbox
// shell is the canonical hazard), and any process holding a named unix
// socket outside the sandbox's own storage — an ssh ControlMaster, an
// agent's control socket, anything another client can find. A process whose
// sockets cannot be inspected shows no shared evidence; the tmux-server
// shape keeps its own stricter proof because its argv is visible on every
// platform.
func throwawaySharedInfraProcess(pid int, canonHome, canonTmux string) bool {
	if throwawayIsTmuxServerFn(pid) {
		return !tmuxServerSocketPrivate(pid, canonTmux)
	}
	paths, ok := throwawayUnixSocketPathsFn(pid)
	if !ok {
		return false
	}
	for _, p := range paths {
		if c, ok := resolveEnvPathInProcFrame(pid, p); !ok || !insideSandboxStorage(c, canonHome, canonTmux) {
			return true
		}
	}
	return false
}

// insideSandboxStorage reports whether a canonical path lives under the
// sandbox's own storage — the marked home or its proven-private tmux dir.
// Sockets there serve this sandbox; sockets anywhere else serve someone
// else.
func insideSandboxStorage(c, canonHome, canonTmux string) bool {
	sep := string(os.PathSeparator)
	if c == canonHome || strings.HasPrefix(c, canonHome+sep) {
		return true
	}
	return canonTmux != "" && (c == canonTmux || strings.HasPrefix(c, canonTmux+sep))
}

// tmuxServerSocketPrivate proves a tmux server's socket lives inside the
// sandbox's private dir. Every discovered candidate — argv -S/--socket, the
// BSD retitle argv0, a -L socket name resolved under the server's own
// TMUX_TMPDIR, and any socket the kernel reports — must resolve inside
// canonTmux, and at least one must exist: an uninspectable server is not
// proven private, so it is never a member.
func tmuxServerSocketPrivate(pid int, canonTmux string) bool {
	if canonTmux == "" {
		return false
	}
	candidates := tmuxServerSocketCandidates(pid)
	if len(candidates) == 0 {
		return false
	}
	sep := string(os.PathSeparator)
	for _, cand := range candidates {
		c, ok := resolveEnvPathInProcFrame(pid, cand)
		if !ok || (c != canonTmux && !strings.HasPrefix(c, canonTmux+sep)) {
			return false
		}
	}
	return true
}

// tmuxServerSocketCandidates collects every socket path a tmux server might
// be serving: the -S/--socket flag value, the BSD-retitled argv0 form
// "tmux: server (<sock>)", the -L/--socket-name form resolved under the
// server's own TMUX_TMPDIR (or the /tmp default), and the unix sockets the
// kernel reports it holding. Duplicates collapse; nothing here decides
// trust — tmuxServerSocketPrivate does.
func tmuxServerSocketCandidates(pid int) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	argv := throwawayArgvFn(pid)
	if len(argv) > 0 {
		if rest, ok := strings.CutPrefix(argv[0], "tmux: server ("); ok {
			add(strings.TrimSuffix(rest, ")"))
		}
	}
	for i, a := range argv {
		switch {
		case (a == "-S" || a == "--socket") && i+1 < len(argv):
			add(argv[i+1])
		case strings.HasPrefix(a, "-S="):
			add(strings.TrimPrefix(a, "-S="))
		case strings.HasPrefix(a, "--socket="):
			add(strings.TrimPrefix(a, "--socket="))
		case (a == "-L" || a == "--socket-name") && i+1 < len(argv):
			base := "/tmp"
			if v, st := throwawayLookupEnvFn(pid, "TMUX_TMPDIR"); st == proctree.EnvFound && v != "" {
				base = v
			}
			add(filepath.Join(base, "tmux-"+strconv.Itoa(os.Getuid()), argv[i+1]))
		}
	}
	if paths, ok := throwawayUnixSocketPathsFn(pid); ok {
		for _, p := range paths {
			add(p)
		}
	}
	return out
}

// processBoundToThrowawayHome reports whether pid's environment proves it
// belongs to the marked home: AGENT_FACTORY_HOME or AF_HOME resolving to it,
// or — only when the marker recorded a private TMUX_TMPDIR — TMUX_TMPDIR
// resolving to it or a TMUX socket under it. proven=false means every
// environment read failed, which is "could not look", never a kill.
// Environment binding is the membership gate, not membership: the caller
// still owes the instance-start and shared-infrastructure checks in
// processMemberOfSandbox.
func processBoundToThrowawayHome(pid int, canonHome, canonTmux string) (bound bool, proven bool) {
	if uid, ok := throwawayUIDFn(pid); !ok || uid != os.Getuid() {
		return false, false
	}
	if v, st := throwawayLookupEnvFn(pid, "AGENT_FACTORY_HOME"); st != proctree.EnvUnknown {
		proven = true
		if st == proctree.EnvFound {
			if c, ok := resolveEnvPathInProcFrame(pid, v); ok && c == canonHome {
				return true, true
			}
		}
	}
	if v, st := throwawayLookupEnvFn(pid, sessiontmux.EnvMarkerHome); st != proctree.EnvUnknown {
		proven = true
		if st == proctree.EnvFound {
			if c, ok := resolveEnvPathInProcFrame(pid, v); ok && c == canonHome {
				return true, true
			}
		}
	}
	if canonTmux != "" {
		sep := string(os.PathSeparator)
		if v, st := throwawayLookupEnvFn(pid, "TMUX_TMPDIR"); st != proctree.EnvUnknown {
			proven = true
			if st == proctree.EnvFound {
				if c, ok := resolveEnvPathInProcFrame(pid, v); ok && c == canonTmux {
					return true, true
				}
			}
		}
		if v, st := throwawayLookupEnvFn(pid, "TMUX"); st != proctree.EnvUnknown {
			proven = true
			if st == proctree.EnvFound {
				if sock, _, _ := strings.Cut(v, ","); sock != "" {
					if c, ok := resolveEnvPathInProcFrame(pid, sock); ok && strings.HasPrefix(c, canonTmux+sep) {
						return true, true
					}
				}
			}
		}
	}
	return false, proven
}

// resolveEnvPathInProcFrame absolutizes an environment-derived path the way
// the process carrying it resolves it, and canonicalizes the result. A
// relative or tilde spelling means something only in THAT process's frame —
// the reaping daemon's own cwd is the marked home after chdirToNeutralHome,
// so joining "." to it would bind every same-user process that happens to
// carry a relative AGENT_FACTORY_HOME (the same hazard classifyDaemonHome
// resolves through resolveHomeInDaemonFrame). Absolute spellings pass
// through; "~"/"~/" expand against the target's own HOME; "~user" forms are
// unresolvable, matching config.ConfigDirFor's rejection; and anything else
// joins /proc/<pid>/cwd. ok=false means the frame cannot be proven — the
// caller leaves the process alone rather than guessing in ours.
func resolveEnvPathInProcFrame(pid int, p string) (string, bool) {
	var joined string
	switch {
	case p == "":
		return "", false
	case filepath.IsAbs(p):
		joined = p
	case p == "~" || strings.HasPrefix(p, "~/"):
		home, st := throwawayLookupEnvFn(pid, "HOME")
		if st != proctree.EnvFound || home == "" {
			return "", false
		}
		joined = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	case strings.HasPrefix(p, "~"):
		return "", false
	default:
		cwd, ok := throwawayWorkingDirFn(pid)
		if !ok || cwd == "" {
			return "", false
		}
		joined = filepath.Join(cwd, p)
	}
	if !filepath.IsAbs(joined) {
		return "", false
	}
	// Reuse the sigterm-fallback guards rather than re-deriving them: a
	// spelling that IS a process-relative procfs path — or whose symlink
	// chain enters one — resolves in OUR frame, not the target's. Such a
	// value is unresolvable as evidence, the same verdict those guards give
	// classifyDaemonHome.
	if isProcessRelativeProcfsHome(joined) || homeSymlinkEntersProcessRelativeProcfs(joined) {
		return "", false
	}
	c, err := canonicalDir(joined)
	if err != nil {
		return "", false
	}
	return c, true
}
