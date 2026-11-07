package daemon

import (
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/log"
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
//  1. CLAIM — before any bind attempt: if the DEFAULT af home claims the port
//     (a live daemon holds its daemon.lock, or its config.toml resolves to a
//     listen_addr overlapping the request), a non-default-home daemon refuses
//     without ever calling net.Listen. This is what closes the window where a
//     second-home daemon starts while the default daemon is down or mid-
//     restart and binds the port cleanly — there is no EADDRINUSE to react to,
//     so the claim itself must be enough.
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
// claims the port req wants — either a live daemon holds its daemon.lock, or
// its config.toml resolves to a listen_addr that would block this bind — or ""
// when the home claims nothing (never configured, listener disabled, or moved
// to a non-colliding address). The config is authoritative when it parses; the
// lock covers what config cannot see — a live daemon in the gap before its
// first-run materialization, or one whose config was deleted under it.
func defaultHomeClaimRefusal(req *net.TCPAddr) string {
	def, err := defaultHomeDir()
	if err != nil {
		return ""
	}
	if eff, found, cfgErr := config.ListenAddrInHome(def); cfgErr == nil && found {
		if boundListenerBlocks(eff, req) {
			return fmt.Sprintf("refusing to bind %s: the default AF home (%s) claims this port — "+
				"its config.toml sets a listen_addr that overlaps it; "+
				"binding would leave that home's daemon fighting for its own address — %s",
				req.String(), def, takeoverRemedy)
		}
		return ""
	}
	lockHeld := false
	ProbeHomeLock(def).Match(
		func() { lockHeld = true },
		func() {},
		func() {},
		func(error) {},
	)
	if lockHeld {
		return fmt.Sprintf("refusing to bind %s: the default AF home (%s) claims this port — "+
			"its daemon is running (daemon.lock held) and the default address belongs to it — %s",
			req.String(), def, takeoverRemedy)
	}
	return ""
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
		req.String(), home, takeoverRemedy)
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
	home, ok := daemonHomeEnv(pid)
	if !ok {
		return "", true, false // a daemon whose home we may not read → unproven
	}
	if home == "" {
		// AGENT_FACTORY_HOME absent in ITS environment → it resolved the
		// default home (daemonHomeEnv's documented EnvAbsent semantics).
		def, err := defaultHomeDir()
		if err != nil {
			return "", true, false
		}
		home = def
	}
	return home, true, true
}

// procListenInodes returns the socket inodes LISTENing on addresses that would
// collide with a bind of req, from procRoot/net/tcp{,6}. complete is false
// when a table that could carry the request was unreadable — a zero-inode
// result then means "could not look", not "no holder".
func procListenInodes(req *net.TCPAddr) (inodes map[uint64]bool, complete bool) {
	inodes = map[uint64]bool{}
	complete = true
	for _, table := range procTablesFor(req) {
		data, err := os.ReadFile(filepath.Join(procRoot, "net", table))
		if err != nil {
			complete = false
			continue
		}
		parseProcNetTCP(data, req, inodes)
	}
	return inodes, complete
}

// procTablesFor names the /proc/net tables that could carry a LISTEN socket
// colliding with req. A requested wildcard contends in both families (the
// kernel binds it dual-stack), a specific v4 address only tcp, a specific v6
// address only tcp6.
func procTablesFor(req *net.TCPAddr) []string {
	v4, v6 := requestIPFamilies(req.IP)
	var tables []string
	if v4 {
		tables = append(tables, "tcp")
	}
	if v6 {
		tables = append(tables, "tcp6")
	}
	return tables
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

// lockFDHome returns the af home of the process whose fd table contains a
// daemon.lock fd — the flock a live daemon holds for its whole life — as the
// lock file's already-resolved absolute dir. ("", true) means the fd table was
// readable and held no daemon.lock fd; ok=false means it could not be read.
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
			return filepath.Dir(target), true
		}
	}
	return "", true
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
