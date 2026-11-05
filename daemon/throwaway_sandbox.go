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
// Membership is proven per process and NEVER from environment variables:
// environments propagate — tmux copies a creator's environment into a
// shared server's global environment, so an inherited AGENT_FACTORY_HOME
// can sit inside a production pane. A pid is a member only when one of
// two positive proofs holds:
//
//   - it runs UNDER the sandbox's own private tmux server — a server the
//     kernel itself shows listening on a socket inside a directory the
//     marker records AND the root sentinel proves this sandbox created;
//     or
//   - its (pid, start id) pair was recorded in the home when the sandbox
//     started it — the daemon records itself, so a reparented daemon is
//     still reachable.
//
// Where socket evidence cannot be read (non-Linux), the first proof can
// never hold and only recorded pids are members. Every signal still goes
// through proctree's identity-checked paths, and anything the code cannot
// prove is left running — including shared servers, their panes, and
// anything merely carrying sandbox environment.

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

// The startup sweep runs inside daemon startup, so the EXPENSIVE half is
// bounded: throwawaySweepMaxHomes caps how many teardowns one start will
// run, and throwawaySweepBudget caps the wall clock. Every entry is still
// EXAMINED — scanning is cheap (a marker read plus a liveness check) —
// because capping the scan itself would let a retained prefix starve the
// entries behind it forever. Remainders are left for the next marked
// start — orphan reaping is convergence, not a gate on readiness.
const throwawaySweepMaxHomes = 64

var throwawaySweepBudget = 30 * time.Second

// Seams so the wiring — sweep selection, socket proofs, owner identity —
// can be driven by fixtures rather than real processes.
var (
	throwawayNowFn         = time.Now
	throwawayRegistryDirFn = func() string { return throwawaysandbox.RegistryDir(os.TempDir(), os.Getuid()) }
	throwawaySnapshotFn    = proctree.Snapshot
	throwawayLookupFn      = proctree.Lookup
	throwawayUIDFn         = proctree.UID
	throwawaySameRootFn    = sameProcessRoot
	// Server-identification seams: a tmux server is proven private by the
	// unix sockets the kernel reports it holding — the only socket
	// evidence that survives argv retitling and absent environment.
	throwawayIsTmuxServerFn    = proctree.IsTmuxServer
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

// recordThrowawayDaemon lists this daemon's own process instance in the
// marked home's recorded-pid file. It is the (b) membership proof: the
// daemon is reparented to init, so it is never "under" the private tmux
// server — only a recorded (pid, start id) lets a teardown still reach
// it. Best-effort: a home whose file cannot be written keeps the deadline
// watchdog either way.
func recordThrowawayDaemon(home string) {
	cur, err := throwawayLookupFn(os.Getpid())
	if err != nil {
		log.WarningLog.Printf("throwaway sandbox daemon could not read its own start id; teardown will not reach it by pid: %v", err)
		return
	}
	if err := throwawaysandbox.WriteRecordedPID(home, cur.PID, cur.StartID); err != nil {
		log.WarningLog.Printf("throwaway sandbox daemon could not record its pid in %s: %v", home, err)
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
	reaped := 0
sweep:
	for _, registered := range homes {
		if time.Since(started) > throwawaySweepBudget {
			log.WarningLog.Printf("throwaway sandbox sweep budget exhausted; some entries remain for the next marked start")
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
		if reaped >= throwawaySweepMaxHomes {
			log.WarningLog.Printf("throwaway sandbox teardown bound reached at %d; %s remains for the next marked start", reaped, home)
			break sweep
		}
		log.InfoLog.Printf("reaping throwaway sandbox %s (kind %s): %s", home, m.Kind, reason)
		throwawayTeardownFn(home, m)
		reaped++
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
	procs := throwawayMemberProcesses(canonHome, m)
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
}

// throwawayPrivateTmuxDir returns the marker's recorded TMUX_TMPDIR when
// the sandbox itself provably created it — POSITIVE proof, not a denylist.
// Three conditions must all hold:
//
//   - the recorded sandbox root carries the creation sentinel the
//     scaffolding plants, and the sentinel's content is THIS marker's
//     created_unix — a root the sandbox made at mark time proves every
//     dir inside it is sandbox-created too;
//   - the marked home sits inside that proven root, so a marker pointing
//     at someone else's tree gains nothing;
//   - the tmux dir sits strictly inside the proven root and, when it
//     exists, is a real directory owned by this uid.
//
// There is no shared-shape list: $HOME/.cache, /tmp, or any other path is
// refused for exactly one reason — no sentinel proves this sandbox made
// it. The proof also ignores the reaper's own environment entirely.
// Absent metadata means "nothing recorded": the empty first return with
// an empty reason is a no-op, not a refusal. The second return is the
// refusal reason for logging; empty means the dir is proven private.
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
	if !sandboxRootCreatedByMarker(root, m.CreatedUnix) {
		return "", fmt.Sprintf("recorded sandbox root %s carries no creation sentinel bound to this marker", root)
	}
	if canonHome != root && !strings.HasPrefix(canonHome, root+sep) {
		return "", fmt.Sprintf("marked home %s is not inside recorded sandbox root %s", canonHome, root)
	}
	if tmuxDir == root || !strings.HasPrefix(tmuxDir, root+sep) {
		return "", fmt.Sprintf("recorded tmux dir %s is not inside sandbox root %s", tmuxDir, root)
	}
	if fi, err := os.Lstat(tmuxDir); err == nil {
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Sprintf("recorded tmux dir %s is not a real directory", tmuxDir)
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
			return "", fmt.Sprintf("recorded tmux dir %s is owned by uid %d", tmuxDir, st.Uid)
		}
	}
	return tmuxDir, ""
}

// sandboxRootCreatedByMarker reports whether the recorded sandbox root
// carries the scaffolding's creation sentinel bound to THIS marker: a
// trusted, uid-owned, regular file whose content is the marker's own
// created_unix. The sentinel is the only way a directory proves the
// sandbox made it — a path that merely exists proves nothing.
func sandboxRootCreatedByMarker(root string, createdUnix int64) bool {
	data, err := throwawaysandbox.TrustedOwnedFile(filepath.Join(root, throwawaysandbox.RootSentinelName))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == strconv.FormatInt(createdUnix, 10)
}

// killThrowawayTmuxSockets runs kill-server on every unix socket under the
// marker's private TMUX_TMPDIR. Privacy is proven by
// throwawayPrivateTmuxDir first; a refused dir means no socket kills at
// all — whatever is on the other end is not the sandbox's server.
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

// throwawayMemberProcesses returns the processes proven to belong to this
// sandbox instance. Membership never reads environment variables — they
// propagate through shared tmux servers into production panes. A pid is a
// member only when:
//
//   - (a) it is, or runs under, a tmux server the KERNEL shows listening
//     on a socket inside the marker's proven-private tmux dir — the
//     server's whole tree plus every process sharing a member's session
//     id (pane strays reparented to init); or
//   - (b) its (pid, start id) was recorded in the home's recorded-pid
//     file, and the current instance at that pid still matches.
//
// Where socket evidence cannot be read at all, (a) can never hold; a pid
// recycled after the snapshot is skipped, not killed. The reaper's own
// pid and pid 1 are always excluded.
func throwawayMemberProcesses(canonHome string, m *throwawaysandbox.Marker) []proctree.Process {
	snap, err := throwawaySnapshotFn()
	if err != nil {
		log.WarningLog.Printf("throwaway teardown cannot snapshot the process table; leaving %s in place: %v", canonHome, err)
		return nil
	}
	self := os.Getpid()
	members := make(map[int]proctree.Process)
	add := func(p proctree.Process) {
		if p.PID > 1 && p.PID != self {
			members[p.PID] = p
		}
	}
	// (a) The private tmux dir binds the server only when the root
	// sentinel proves the sandbox made it — a dir the sandbox did not
	// create can host other users' servers.
	if canonTmux, _ := throwawayPrivateTmuxDir(canonHome, m); canonTmux != "" {
		for pid := range snap {
			if !tmuxServerSocketPrivate(pid, canonTmux) {
				continue
			}
			tree := proctree.TreeOf(snap, pid)
			for _, q := range tree {
				add(q)
				// A pane process reparented to init keeps its pane's
				// session id — SessionMembers reaches the strays TreeOf
				// cannot. A non-positive SID matches nothing by contract.
				for _, r := range proctree.SessionMembers(snap, q.SID) {
					add(r)
				}
			}
		}
	}
	// (b) Process instances the sandbox recorded at spawn time.
	for _, member := range throwawayRecordedMembers(canonHome) {
		add(member)
	}
	out := make([]proctree.Process, 0, len(members))
	for _, p := range members {
		out = append(out, p)
	}
	return out
}

// throwawayRecordedMembers returns the live processes named by the home's
// recorded-pid file whose current instance still matches the recorded
// (pid, start id) — proof the record names THIS process, not a recycled
// slot. Each must also still be same-uid and in this filesystem root: a
// record is a teardown target, so every check it could plausibly fail is
// checked. An unreadable or untrusted file contributes nothing.
func throwawayRecordedMembers(canonHome string) []proctree.Process {
	recs, err := throwawaysandbox.ReadRecordedPIDs(canonHome)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.WarningLog.Printf("throwaway teardown could not read recorded pids in %s; skipping recorded membership: %v", canonHome, err)
		}
		return nil
	}
	var members []proctree.Process
	for _, rec := range recs {
		cur, err := throwawayLookupFn(rec.PID)
		if err != nil || cur.StartID != rec.StartID {
			continue // dead or recycled — the record names a process that no longer exists
		}
		if uid, ok := throwawayUIDFn(rec.PID); !ok || uid != os.Getuid() {
			continue
		}
		if !throwawaySameRootFn(rec.PID) {
			continue
		}
		members = append(members, cur)
	}
	return members
}

// tmuxServerSocketPrivate proves a tmux server is the sandbox's own by
// socket evidence ALONE: the kernel must report it holding at least one
// named unix socket, and every reported socket must resolve inside the
// proven-private tmux dir. ok=false (no socket evidence — non-Linux, or
// an uninspectable process) means the server can never be proven, so it
// is never a member: that fails closed exactly where the alternative
// would aim signals at a shared server.
func tmuxServerSocketPrivate(pid int, canonTmux string) bool {
	if canonTmux == "" || !throwawayIsTmuxServerFn(pid) || !throwawaySameRootFn(pid) {
		return false
	}
	if uid, ok := throwawayUIDFn(pid); !ok || uid != os.Getuid() {
		return false
	}
	paths, ok := throwawayUnixSocketPathsFn(pid)
	if !ok || len(paths) == 0 {
		return false
	}
	sep := string(os.PathSeparator)
	for _, p := range paths {
		c, err := canonicalDir(p)
		if err != nil || (c != canonTmux && !strings.HasPrefix(c, canonTmux+sep)) {
			return false
		}
	}
	return true
}
