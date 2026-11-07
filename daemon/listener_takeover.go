package daemon

import (
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/log"
	"golang.org/x/sys/unix"
)

// listener_takeover.go is the #6858 no-takeover gate for the restartable web
// listener: a daemon whose network.listen_addr collides with the built-in
// default must never take the web port away from another AGENT_FACTORY_HOME's
// daemon — the failure mode of #6386's retry loop, where a second home's
// daemon retrying a held port wins it the moment the rightful owner's restart
// releases it, and the user's proxy, bookmarks and tokens then point at the
// wrong daemon.
//
// The gate has two independent triggers, both evaluated only while this daemon
// holds no web listener:
//
//  1. CLAIM — before any bind attempt: if the DEFAULT af home claims the port,
//     a non-default-home daemon refuses without ever calling net.Listen. A
//     claim needs evidence the home's daemon is RUNNING or will COME BACK —
//     a live daemon.lock, or an enabled autostart unit serving that home —
//     never a config.toml merely existing: first-run materialization writes
//     listen_addr into every home's file, so a stray ~/.agent-factory would
//     otherwise strand a user whose real home is relocated (their own daemon
//     refused the port, with only a quiet TUI to say why). Even then the home
//     claims only THIS port: a config.toml resolving elsewhere narrows a
//     proven claim away. This is what closes the window where a second-home
//     daemon starts while the default daemon is down or mid-restart and
//     binds the port cleanly — there is no EADDRINUSE to react to, so the
//     claim itself must be enough.
//  2. HOLDER — after a failed EADDRINUSE bind: identify the process holding
//     the contested socket and refuse when it is a daemon of ANOTHER af home.
//     "Held by an af daemon" alone is not enough — our OWN home's previous
//     process holding the port late across our restart is the #6386 recovery
//     that must keep working — so the holder's home is established from kernel
//     ground truth, never anything the holder volunteers: /proc/net/tcp{,6}
//     gives the LISTEN socket's inode, /proc/<pid>/fd gives its owner, and the
//     owner's own daemon.lock fd gives its home as an already-resolved path —
//     no argv trust, no environment spelling, and it covers custom-named test
//     daemons the af/agent-factory argv check would miss. A holder with no
//     lock fd (a pre-lock-era daemon) falls back to AGENT_FACTORY_HOME from
//     its environ plus the af argv test; env-absent resolves the default home.
//
// Verdicts: foreign home → refuse (latched for this daemon's lifetime, so a
// later release gap can never hand us the port); our home → retry (the #6386
// recovery); a non-af holder → retry (a third-party squatter is what the retry
// exists for); anything UNPROVEN — procfs missing (non-Linux), an fd table or
// environ we may not read, a holder that died mid-lookup — → retry: an
// unverifiable holder is never treated as foreign, and the default-home
// exemption keeps the main daemon's behaviour exactly as it was.
//
// The refusal is not an error: like the #5137 posture refusal it is a stable
// non-serving state recorded on the lifecycle (TCPRefusalReason — `af daemon
// status` shows it, `af doctor` rows it) and logged once, so the retry loop
// stops competing instead of lurking for the next release gap. The remedy it
// names is the same everywhere: set [network] listen_addr to another port.
//
// Nothing is asked of or exposed on the contested listener: no new
// unauthenticated surface anywhere. The other home's path appears only in
// this daemon's log and its own socket-served status, which only the home's
// owner can read — a refused daemon has no TCP listener to leak through.

// defaultWebListenAddr is what the gate measures "the default" against: the
// built-in network.listen_addr value. It is a package var because a daemon
// test may never bind the shipped port itself (#6625) — a regression test
// needs the gate to fire on a real kernel socket, so it re-points this var at
// a free loopback port for the duration of the test.
var defaultWebListenAddr = config.DefaultListenAddr()

// defaultHomeDir resolves the DEFAULT af home — the home the built-in default
// listen address belongs to — independently of this daemon's own
// AGENT_FACTORY_HOME. A var so tests can point it at a fixture.
var defaultHomeDir = func() (string, error) { return config.ConfigDirFor("") }

// procRoot is where socket tables and fd links are read — a var so the
// classifier's parsers can be exercised against fixture /proc trees.
var procRoot = "/proc"

// takeoverRemedy is the fix every refusal names: the port belongs to another
// home, so this daemon needs an address of its own.
const takeoverRemedy = "set [network] listen_addr to another port for this daemon " +
	"(af config set network.listen_addr 127.0.0.1:<port>)"

// takeoverHolderRemedy adds the other half a HOLDER refusal can fix: a live
// daemon of the other home keeps its port, so besides moving this daemon the
// user can move that home's listener instead — then restart this daemon,
// which is what re-evaluates the refusal.
const takeoverHolderRemedy = takeoverRemedy + ", or move the other home's " +
	"listener to another port — then restart this daemon"

// takeoverClaimRemedy is the fuller fix a CLAIM refusal names: the default
// home is not currently serving the port but is provably alive or set to
// return, so freeing the port for it (moving or disabling its listener, or
// removing a stray default home outright) is a remedy alongside moving this
// daemon — then restart this daemon to re-evaluate.
const takeoverClaimRemedy = takeoverRemedy + ", or free the port for it: " +
	"move or disable the default home's listener (set its network.listen_addr " +
	"to another port or \"\"), or remove a stray default home — then restart " +
	"this daemon"

// takeoverGateApplies reports whether a bind of req is one the no-takeover
// gate polices: it would collide with a listener on the built-in default
// address (same port, overlapping IPs — the boundListenerBlocks test), and our
// own home is NOT the default home. The default home's own daemon is exempt:
// its restart/recovery behaviour stays exactly what #6386 made it, retrying a
// held default port until it wins it back.
func takeoverGateApplies(req *net.TCPAddr) bool {
	if req == nil || !boundListenerBlocks(defaultWebListenAddr, req) {
		return false
	}
	mine, err := config.GetConfigDir()
	if err != nil {
		return false // cannot prove our home → keep today's behaviour
	}
	def, err := defaultHomeDir()
	if err != nil {
		return false // cannot identify the default home → keep today's behaviour
	}
	return !sameAFHome(mine, def)
}

// defaultHomeClaimRefusal returns the refusal reason when the DEFAULT af home
// claims the port req wants, or "" when it claims nothing. A claim needs
// evidence the home's daemon is RUNNING or will COME BACK — a config.toml
// merely existing is not evidence: first-run materialization writes
// listen_addr into every home's file, so a leftover ~/.agent-factory would
// otherwise strand a user whose real home is relocated — their relocated
// daemon would refuse the default port on every reconcile, over a home that
// is never coming back (#6858 review: no regression on the default home's
// users). The evidence that counts:
//
//   - a live flock on the default home's daemon.lock, read from /proc/locks —
//     kernel ground truth that never takes the lock itself (a flock probe,
//     even a microsecond LOCK_EX hold, would make that home's own singleton
//     acquire fail "already running" in its start window), or
//   - an installed AND enabled autostart unit whose baked AGENT_FACTORY_HOME
//     serves the default home — the OS-level "it will be back" contract.
//
// Even a proven home claims only THIS port: its config.toml resolving to a
// non-colliding listen_addr — or to "" — narrows the claim away, because a
// daemon that would never bind this port has nothing to lose. An absent
// config leaves the built-in default, and a proven-but-unreadable one stays
// claimed rather than guessed away.
func defaultHomeClaimRefusal(req *net.TCPAddr) string {
	def, err := defaultHomeDir()
	if err != nil {
		return ""
	}
	evidence := ""
	if pid, held := daemonLockHeld(def); held {
		evidence = "its daemon is running"
		if pid > 0 {
			evidence = fmt.Sprintf("its daemon is running (daemon.lock held by pid %d)", pid)
		}
	} else if autostartWillReturnHome(def) {
		evidence = "an enabled autostart unit is set to restart its daemon"
	}
	if evidence == "" {
		return "" // a stray or never-used home claims nothing
	}
	if eff, found, cfgErr := config.ListenAddrInHome(def); cfgErr == nil && found &&
		!boundListenerBlocks(eff, req) {
		return "" // proven live/returning, but not for THIS port
	}
	return fmt.Sprintf("refusing to bind %s: the default AF home (%s) claims this port — %s; "+
		"binding would leave that home's daemon fighting for its own address — %s",
		req.String(), def, evidence, takeoverClaimRemedy)
}

// daemonLockHeld reports whether a process provably holds an exclusive flock
// on dir's daemon.lock — the live-daemon evidence that costs the probed home
// nothing. It reads /proc/locks, never the lock itself: even a one-instant
// LOCK_EX probe would collide with that home's own singleton acquire and fail
// its daemon start as "already running" (the probe must never BE the
// contention this gate exists to stop). The second return is the holder's pid
// for the refusal message; 0 when the kernel did not name one.
func daemonLockHeld(dir string) (int, bool) {
	info, err := os.Stat(daemonLockPathIn(dir))
	if err != nil {
		return 0, false // no lock file exists for anyone to hold
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if ok {
		if data, rerr := os.ReadFile(filepath.Join(procRoot, "locks")); rerr == nil {
			return procFlockHolderPID(data, uint64(st.Dev), st.Ino)
		}
	}
	// /proc/locks or the stat was unreadable — fall back to the identity the
	// daemon publishes: a daemon.pid naming a pid whose own fd table proves
	// it holds THIS home's lock. Weaker (misses the lock-held-before-pid-write
	// window), but still honest evidence with no lock taken.
	data, ok := readManagedFileNoFollow(filepath.Join(dir, "daemon.pid"), daemonPIDFileMaxBytes)
	if !ok {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if home, ok := lockFDHome(pid); ok && home != "" && sameAFHome(home, dir) {
		return pid, true
	}
	return 0, false
}

// procFlockHolderPID parses /proc/locks for an exclusive FLOCK on dev:ino and
// returns its holder's pid. Line shape:
//
//	1: FLOCK  ADVISORY  WRITE 2435 08:01:44042672 0 EOF
//
// Only FLOCK rows count — af's singleton lock is flock(2), and POSIX/OFD
// fcntl locks live in a different namespace on Linux: they would not block
// the daemon's own acquire, so they are not evidence. The dev field is
// major:minor in hex and the inode decimal; both are parsed numerically so
// zero-padding never decides the match. held is true whenever a matching row
// exists; pid is 0 when the kernel showed a holder we cannot name.
func procFlockHolderPID(data []byte, dev uint64, ino uint64) (pid int, held bool) {
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 6 || f[1] != "FLOCK" || f[3] != "WRITE" {
			continue
		}
		devIno := strings.Split(f[5], ":")
		if len(devIno) != 3 {
			continue
		}
		maj, e1 := strconv.ParseUint(devIno[0], 16, 32)
		mn, e2 := strconv.ParseUint(devIno[1], 16, 32)
		inode, e3 := strconv.ParseUint(devIno[2], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil {
			continue
		}
		if maj != uint64(unix.Major(dev)) || mn != uint64(unix.Minor(dev)) || inode != ino {
			continue
		}
		if n, err := strconv.Atoi(f[4]); err == nil && n > 0 {
			return n, true
		}
		return 0, true
	}
	return 0, false
}

// autostartWillReturnHome reports whether the installed autostart unit both
// SERVES dir and is ENABLED by the service manager — the only "will be back"
// evidence a down-or-mid-restart daemon can offer. An installed unit that is
// disabled, or one serving another home, claims nothing: the first will not
// restart anything, the second restarts someone else.
func autostartWillReturnHome(dir string) bool {
	serves, installed, err := AutostartUnitServesHome(dir)
	if err != nil || !installed || !serves {
		return false
	}
	enabled := false
	yes := func() { enabled = true }
	noop := func() {}
	unknown := func(error) {}
	switch autostartGOOS {
	case "linux":
		ans, _ := systemdAsk(systemdIsEnabled)
		ans.Match(yes, noop, noop, unknown)
	case "darwin":
		launchdEnabled().Match(yes, noop, noop, unknown)
	}
	return enabled
}

// portHolderVerdict is who holds the contested port, for the give-up decision.
type portHolderVerdict int

const (
	// holderUnproven covers every "could not identify" outcome: non-Linux
	// procfs, unreadable fd tables/environ/argv, no matching socket found, a
	// holder that exited mid-lookup. Unproven NEVER resolves to refuse —
	// refusing on a guess could strand a same-home predecessor's recovery.
	holderUnproven  portHolderVerdict = iota
	holderForeign                     // a live af daemon of ANOTHER home holds it
	holderOwn                         // a live af daemon of OUR home holds it
	holderNonDaemon                   // the holder is provably not an af daemon
)

// foreignHolderRefusal returns the refusal reason when the port req failed
// EADDRINUSE on is held by a live af daemon of another af home, or "" for
// every other verdict — same-home predecessor, third-party squatter, and
// everything unprovable all keep today's retry.
func foreignHolderRefusal(req *net.TCPAddr) string {
	if !takeoverGateApplies(req) {
		return ""
	}
	verdict, home := classifyPortHolder(req)
	if verdict != holderForeign {
		return ""
	}
	return fmt.Sprintf("refusing to bind %s: the address is held by a daemon of another AF home (%s); "+
		"taking it the moment that daemon's restart releases it is the #6858 takeover — %s",
		req.String(), home, takeoverHolderRemedy)
}

// classifyPortHolder resolves the holder of the contested port: the contested
// LISTEN socket's inode from /proc/net/tcp{,6}, then the pid(s) holding that
// inode, then each holder's af home from its own daemon.lock fd (with an
// environ+argv fallback for lock-less daemons).
func classifyPortHolder(req *net.TCPAddr) (portHolderVerdict, string) {
	inodes, complete := procListenInodes(req)
	if len(inodes) == 0 {
		// No matching socket in the tables we read — whether because none
		// exists, it lives in a table we could not read, or it died between
		// the failed bind and our lookup. Unidentified is not foreign.
		return holderUnproven, ""
	}
	pids := pidsHoldingSockets(inodes)
	if len(pids) == 0 {
		return holderUnproven, ""
	}
	mine, err := config.GetConfigDir()
	if err != nil {
		return holderUnproven, ""
	}
	// Score every holder pid before deciding: the verdict ranking is
	// foreign > unproven > own > non-daemon — a PROVEN foreign daemon holds
	// the port whatever else shares it, anything unidentifiable must not
	// resolve to foreign OR to safe, an own-home holder is the recovery the
	// retry protects, and a plain squatter is what the retry exists for.
	var foreignHome string
	foreign, unproven, own, nonDaemon := false, !complete, false, false
	for pid := range pids {
		home, isDaemon, provable := holderDaemonHome(pid)
		switch {
		case !provable:
			unproven = true
		case !isDaemon:
			nonDaemon = true
		case sameAFHome(home, mine):
			own = true
		default:
			foreign, foreignHome = true, home
		}
	}
	switch {
	case foreign:
		return holderForeign, foreignHome
	case unproven:
		return holderUnproven, ""
	case own:
		return holderOwn, mine
	case nonDaemon:
		return holderNonDaemon, ""
	}
	return holderUnproven, ""
}

// holderDaemonHome identifies a socket-holding process: (home, isDaemon,
// provable). provable=false means we could not establish either daemon-hood or
// home — the caller must not treat the holder as foreign OR as safely ours.
func holderDaemonHome(pid int) (string, bool, bool) {
	// Every path we might read from this process's fd table resolves in ITS
	// mount namespace, not ours — a holder in a container or chroot whose
	// "/x/daemon.lock" names a different directory would hand us a home that
	// is not its home. Unverifiable, never a guess in our frame.
	if !sameProcessRoot(pid) {
		return "", false, false
	}
	// A live af daemon holds its per-home daemon.lock flock for its entire
	// life, so an fd whose link target is */daemon.lock proves daemon-hood AND
	// names the home in one read — the strongest evidence there is.
	if home, ok := lockFDHome(pid); ok {
		if home != "" {
			return home, true, true
		}
		// fd table read, no lock fd: not proven a daemon — fall through to the
		// argv/env check for a lock-less (pre-flock or test) daemon.
	}
	args := daemonArgs(pid)
	if len(args) == 0 {
		return "", false, false // argv unreadable → nothing provable
	}
	if !isAgentFactoryDaemon(pid) {
		return "", false, true
	}
	env, ok := daemonHomeEnv(pid)
	if !ok {
		return "", true, false // a daemon whose environ we may not read → unproven
	}
	// The env spelling means what it meant in the HOLDER's frame: a relative
	// value names a dir under ITS cwd, an absent or tilde value derives from
	// ITS $HOME, and an absolute one lives in ITS mount namespace. Resolving
	// it here instead would mislabel a same-home predecessor foreign (the
	// refusal then latches for this daemon's life) — and mislabel a foreign
	// holder ours. An unresolvable frame is unproven, never a guess in ours.
	home, ok := daemonHomeInProcessFrame(pid, env)
	if !ok {
		return "", true, false
	}
	return home, true, true
}

// procListenInodes returns the socket inodes LISTENing on addresses that would
// collide with a bind of req, from procRoot/net/tcp{,6} — always BOTH tables:
// a specific v4 request still contends with a dual-stack [::] socket (the
// common Go wildcard bind owns v4 space too), and that socket appears only in
// tcp6; symmetrically a v4 wildcard socket blocks a dual-stack [::] request.
// parseProcNetTCP's listenIPsOverlap already rejects the non-colliding
// cross-family rows, so the read is unconditional. complete is false when a
// table that could carry the request was unreadable — a zero-inode result
// then means "could not look", not "no holder".
func procListenInodes(req *net.TCPAddr) (inodes map[uint64]bool, complete bool) {
	inodes = map[uint64]bool{}
	complete = true
	for _, table := range []string{"tcp", "tcp6"} {
		data, err := os.ReadFile(filepath.Join(procRoot, "net", table))
		if err != nil {
			complete = false
			continue
		}
		parseProcNetTCP(data, req, inodes)
	}
	return inodes, complete
}

// parseProcNetTCP collects the inodes of LISTEN sockets whose local address
// would block a bind of req: same port, overlapping IP (the
// boundListenerBlocks half of the collision test). Line shape:
//
//	sl local_address rem_address st tx_queue:rx_queue tr:tm retr uid timeout inode
//	 0 0100007F:1F90 00000000:0000 0A        ...                       12345
func parseProcNetTCP(data []byte, req *net.TCPAddr, out map[uint64]bool) {
	for i, line := range strings.Split(string(data), "\n") {
		if i == 0 {
			continue // header
		}
		f := strings.Fields(line)
		if len(f) < 10 || f[3] != "0A" { // 0A = TCP_LISTEN
			continue
		}
		local := strings.Split(f[1], ":")
		if len(local) != 2 {
			continue
		}
		port, err := strconv.ParseUint(local[1], 16, 16)
		if err != nil || int(port) != req.Port {
			continue
		}
		ip, ok := procHexIP(local[0])
		if !ok || !listenIPsOverlap(ip, req.IP) {
			continue
		}
		if ino, err := strconv.ParseUint(f[9], 10, 64); err == nil && ino != 0 {
			out[ino] = true
		}
	}
}

// procHexIP decodes the kernel's hex address column: v4 is one little-endian
// word, v6 is four. Returns false for anything else rather than guessing.
func procHexIP(s string) (net.IP, bool) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, false
	}
	switch len(b) {
	case 4:
		return net.IPv4(b[3], b[2], b[1], b[0]), true
	case 16:
		ip := make(net.IP, 16)
		for w := 0; w < 4; w++ {
			ip[w*4], ip[w*4+1], ip[w*4+2], ip[w*4+3] = b[w*4+3], b[w*4+2], b[w*4+1], b[w*4]
		}
		return ip, true
	}
	return nil, false
}

// pidsHoldingSockets scans procRoot/<pid>/fd for socket fds pointing at the
// contested inodes. Pids whose fd table cannot be read are skipped: they could
// be holding the socket, but so could anyone — an unidentified holder is
// already unproven through the empty result, and punishing a found holder for
// an unrelated unreadable table would misclassify the common case.
func pidsHoldingSockets(inodes map[uint64]bool) map[int]bool {
	pids := map[int]bool{}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return pids
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || !e.IsDir() {
			continue
		}
		fdDir := filepath.Join(procRoot, e.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue // the fd closed between readdir and readlink — churn, not a holder
			}
			if ino, ok := socketFDInode(target); ok && inodes[ino] {
				pids[pid] = true
				break
			}
		}
	}
	return pids
}

// socketFDInode parses an /proc/<pid>/fd link target of the form
// "socket:[12345]" into the inode number.
func socketFDInode(target string) (uint64, bool) {
	rest, ok := strings.CutPrefix(target, "socket:[")
	if !ok || !strings.HasSuffix(rest, "]") {
		return 0, false
	}
	ino, err := strconv.ParseUint(strings.TrimSuffix(rest, "]"), 10, 64)
	return ino, err == nil && ino != 0
}

// lockFDHome returns the af home of the process whose fd table holds a
// daemon.lock, as the lock file's already-resolved absolute dir. A basename
// alone is not proof — any process can open a file named daemon.lock — so the
// fd counts only when the sibling daemon.pid names THIS process: the identity
// af publishes into its own home alongside the lock it holds for life.
// ("", true) means the fd table was readable and held no proven lock fd;
// ok=false means it could not be read.
func lockFDHome(pid int) (home string, ok bool) {
	fdDir := filepath.Join(procRoot, strconv.Itoa(pid), "fd")
	fds, err := os.ReadDir(fdDir)
	if err != nil {
		return "", false
	}
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
		if err != nil {
			continue
		}
		if filepath.Base(target) == daemonLockFileName {
			if home = filepath.Dir(target); daemonPIDFileNames(home, pid) {
				return home, true
			}
		}
	}
	return "", true
}

// daemonPIDFileNames reports whether dir's daemon.pid names pid — the
// liveness identity af publishes for its daemon in the same home dir as the
// lock. A daemon serving a contested socket published its pid long ago; a
// missing or mismatched file just means the lock fd proves nothing. The read
// is the same no-follow/nonblocking one every daemon.pid reader uses: this
// runs on a path learned from an unverified process's fd while wl.mu is held,
// so a FIFO or device swapped in for the file must not hang reconciliation.
func daemonPIDFileNames(dir string, pid int) bool {
	data, ok := readManagedFileNoFollow(filepath.Join(dir, "daemon.pid"), daemonPIDFileMaxBytes)
	if !ok {
		return false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	return err == nil && n == pid
}

// sameAFHome reports whether two home paths identify the same install:
// os.SameFile when both stat (case-insensitive volumes and spelling variants
// compare by filesystem identity), the normalized path when either side is
// unstatable.
func sameAFHome(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ai, aerr := os.Stat(a)
	bi, berr := os.Stat(b)
	if aerr == nil && berr == nil {
		return os.SameFile(ai, bi)
	}
	return normalizeHomePath(a) == normalizeHomePath(b)
}

// normalizeHomePath canonicalizes a home path string for comparison: tilde
// expansion, absolutization, symlink resolution where components exist.
func normalizeHomePath(p string) string {
	p = config.ExpandTilde(p)
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return pathutil.ResolveForCompare(p)
}

// foreignGateRefusalLocked returns the #6858 refusal for attempting addr: a
// latched refusal first (it never un-refuses), then the default home's claim
// when the gate polices this address at all. req is the resolved request.
// Caller holds wl.mu.
func (wl *webListeners) foreignGateRefusalLocked(addr string, req *net.TCPAddr) string {
	if refusal, latched := wl.foreignRefusals[addr]; latched {
		return refusal
	}
	if !takeoverGateApplies(req) {
		return ""
	}
	return defaultHomeClaimRefusal(req)
}

// refuseOrKeepLocked applies a #6858 refusal to the current listener state.
// With no listener the refusal becomes the stable non-serving state
// refuseWebForeignLocked records. With a live listener — a rebind onto a
// refused or foreign-held address — the refusal latches for every later
// attempt, the bind is refused as an ordinary apply failure naming the
// still-serving address, and the healthy listener is never released into a
// takeover. The ERROR logs once per refusal text; a repeated reconcile error
// is visible without spamming the log.
// Caller holds wl.mu.
func (wl *webListeners) refuseOrKeepLocked(addr, refusal string) error {
	if wl.webHandle == nil {
		wl.refuseWebForeignLocked(addr, refusal)
		return nil
	}
	if wl.foreignRefusals[addr] != refusal {
		if wl.foreignRefusals == nil {
			wl.foreignRefusals = map[string]string{}
		}
		wl.foreignRefusals[addr] = refusal
		log.ErrorLog.Printf("%s", refusal)
	}
	return fmt.Errorf("apply network.listen_addr %q: %s — daemon still serving on %s",
		addr, refusal, servingOn(wl.webConfigAddr))
}

// refuseWebForeignLocked records a foreign-takeover refusal: the listener is a
// stable non-serving state — latched in foreignRefusals for this daemon's
// lifetime so no later release gap can hand us the port — and surfaced on the
// lifecycle exactly like the #5137 posture refusal (TCPRefusalReason → `af
// daemon status`, `af doctor`). It mirrors refuseWebLocked's bookkeeping minus
// the parts that only exist for a serving socket (retire, tracker sever,
// posture pin): by definition we reach here with webHandle==nil. Idempotent:
// the same refusal recurring across reconciles neither re-logs nor re-bumps.
// Caller holds wl.mu.
func (wl *webListeners) refuseWebForeignLocked(addr, refusal string) {
	if wl.foreignRefusals == nil {
		wl.foreignRefusals = map[string]string{}
	}
	wl.foreignRefusals[addr] = refusal
	if wl.webRefusal == refusal {
		return // already in force — do not re-log on every reconcile
	}
	wl.webConfigAddr = ""
	wl.webBoundAddr = ""
	wl.webGen++
	wl.webRefusal = refusal
	if wl.manager.lifecycle != nil {
		wl.manager.lifecycle.setTCPRefused(addr, refusal)
	}
	log.ErrorLog.Printf("%s", refusal)
}
