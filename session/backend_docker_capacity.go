// The docker data-root capacity evidence a failed provision joins onto its
// error (#5295, extended by #5382). docker's combined output loses the
// engine-side errno as text the lost-restore classifier deliberately never
// reads, so the verdict is re-derived from the engine's data-root filesystem
// instead: a statfs needs no write access to a root-owned dir, and "no
// allocatable space" is exactly the ENOSPC dockerd's own write hit. A joined
// PathError carries the real errno for the classifier's errors.Is chain, with
// provenance text naming where it came from.
//
// The verdict only counts when it is PROVEN about the filesystem the engine
// actually writes (#5382):
//
//   - The endpoint must live on this host (dockerEndpointOnThisHost) — a
//     tcp:// endpoint, even on loopback, can be an `ssh -L` forward to a
//     remote engine whose data root is another machine's filesystem, and a
//     remote endpoint is refused before provisioning anyway.
//   - The daemon's own hostname must match af's os.Hostname() — a Unix socket
//     proves the daemon answers on this machine, not that it shares this
//     kernel: a forwarded or proxied socket (ssh -U, Docker Desktop's host
//     socket artifact) answers with the REMOTE daemon's info, whose
//     DockerRootDir is a path in another filesystem entirely.
//   - The daemon answering af's socket must share af's mount namespace —
//     hostname equality alone cannot prove it (an af container on
//     --uts=host sees the host's name but keeps its own mounts). The
//     namespace proof is SO_PEERCRED: dialing the endpoint yields the
//     answering process's pid, and /proc/<pid>/ns/mnt either equals
//     /proc/self/ns/mnt or does not — borrowed sockets, forwarded
//     sockets, and foreign-namespace daemons all fail closed (#5382
//     review).
//   - The privilege mode decides WHICH block count answers the fullness
//     question, and the class of the failed step decides whose privilege
//     matters (dockerOpClass): a rootful dockerd allocates reserved blocks
//     (Bfree), a rootless one writes as an ordinary user and only reaches
//     Bavail (docker info reports `name=rootless` in SecurityOptions), and a
//     `docker exec` running as a non-root container user is Bavail-bounded
//     on a rootful engine only where its write can bypass the overlay —
//     the overlayfs upper is accessed with the daemon's mounter
//     credentials, so the verdict needs a bind or volume mount on the
//     probed filesystem to exist (or `name=userns` uid remapping to make
//     even the in-container uid an ordinary host user — still requiring
//     the bind path). Both the exec user and the userns opt-out are read
//     off the RUNNING CONTAINER — `docker run --user` overrides the
//     image's USER and `--userns=host` overrides the daemon-wide remap,
//     so neither is answered from image config alone (#5382 review).
//   - The statfs must succeed and actually report exhaustion — blocks or
//     inodes. Every unresolved or unproven step returns nil instead: unknown
//     is not-proven, never a "full" verdict.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
)

// statfsFull is the test seam over the bounded filesystem fullness answer —
// a statfs needs no write access, so it is the only probe that works on a
// root-owned engine data root. statfsFullUnprivileged is the same answer for
// an engine whose writes cannot reach the root-reserved blocks (rootless
// dockerd): it reads Bavail, the blocks the unprivileged daemon can actually
// allocate (#5382).
var statfsFull = fsprobe.Full
var statfsFullUnprivileged = fsprobe.FullUnprivileged

// dockerEngineInfoFormat pulls the whole `docker info` snapshot once — the
// evidence path needs three fields from one consistent engine view (root dir,
// hostname, privilege mode), not three sequential reads.
const dockerEngineInfoFormat = "{{json .}}"

// dockerOpClass identifies WHICH provisioning operation failed, so the
// capacity evidence can ask the right fullness question — and, just as
// important, refuse to answer for failures no filesystem state could cause
// (#5382 review): an unparsable `docker port` reply or an account
// mount-boundary refusal is not a data-root write even when the data root
// happens to be full, and borrowing the fs verdict there would park a
// structural failure on the environment's unbounded retry budget.
type dockerOpClass int

const (
	// dockerOpLocal is a failure produced on af's side — endpoint or
	// hostname validation, output parsing, command construction, a poll
	// deadline — or a read-only docker round-trip (info, inspect, port).
	// No data-root write is involved, so the fs verdict cannot apply.
	dockerOpLocal dockerOpClass = iota
	// dockerOpDaemonWrite is a dockerd-side data-root write — `docker run`,
	// `docker cp`, the exec state the daemon records — made with the
	// daemon's own privilege: rootful dockerd reaches the reserved blocks
	// (Bfree); a rootless one is held to Bavail like any other user.
	dockerOpDaemonWrite
	// dockerOpRootExec is a `docker exec` run as in-container uid 0 —
	// privileged inside the container's own user namespace, which still
	// means an ordinary user when the engine remaps uids (name=userns).
	// It is the class the prepare steps (prepareAccountUser,
	// prepareNonAccountUser) carry: their mkdir+chown writes the session
	// home/account mounts and /workspace.
	dockerOpRootExec
	// dockerOpChmod is the root exec copyAfBinary uses to chmod the copied
	// `af` binary at /usr/local/bin/af. It is a root exec like
	// dockerOpRootExec, but its single write root is /usr/local/bin/af —
	// not the home/workspace/account paths the prepare steps write — so a
	// chmod failure's capacity verdict may only be borrowed by a bind
	// covering the binary path, never by an unrelated bind at the home or
	// workspace that the chmod never touched (#5424 review).
	dockerOpChmod
	// dockerOpSessionExec is a `docker exec` run as the session's user —
	// the account owner's uid:gid when a session mounts an account, else
	// whatever USER the image declares. A non-root container user cannot
	// allocate root-reserved blocks, so on a rootful engine its writes are
	// bounded by Bavail even though the daemon's own writes are not.
	dockerOpSessionExec
)

// dockerStepError records which provisioning operation a failure came from
// so the evidence boundary can tell "the engine could not write" from "af
// could not parse" — only the former may borrow the filesystem verdict.
type dockerStepError struct {
	op  dockerOpClass
	err error
}

func (e *dockerStepError) Error() string { return e.err.Error() }
func (e *dockerStepError) Unwrap() error { return e.err }

// dockerStepErr classifies a step failure for the evidence boundary. A
// dockerStepError already inside wins — a step that classified a nested
// failure itself (a local construction error inside an exec step, say)
// keeps its own answer instead of inheriting the callsite's.
func dockerStepErr(err error, op dockerOpClass) error {
	var se *dockerStepError
	if errors.As(err, &se) {
		return err
	}
	return &dockerStepError{op: op, err: err}
}

// dockerEngineInfo is the slice of `docker info` the capacity probe consults:
// where the engine writes, which host it believes it runs on, and whether it
// runs rootless or remaps container uids. A field the engine did not report
// stays empty and keeps the corresponding proof from being made.
type dockerEngineInfo struct {
	root            string
	name            string
	driver          string
	containerdStore bool
	rootless        bool
	userns          bool
}

// parseDockerEngineInfo decodes the `docker info` snapshot. The dockerExec
// seam captures COMBINED output, and `docker info` prints daemon warnings —
// "Running in rootless-mode", missing cgroup support — to stderr, which lands
// in the buffer around the JSON the --format requested. Decode the first JSON
// value out of the blob rather than unmarshalling it whole; a blob with no
// JSON object at all is unproven, not malformed evidence. Rootless engines
// report "name=rootless" in SecurityOptions — the documented signal dockerd
// appends itself — and nothing else is trusted as privilege evidence.
func parseDockerEngineInfo(out []byte) (dockerEngineInfo, bool) {
	var raw struct {
		DockerRootDir   string     `json:"DockerRootDir"`
		Name            string     `json:"Name"`
		Driver          string     `json:"Driver"`
		DriverStatus    [][]string `json:"DriverStatus"`
		SecurityOptions []string   `json:"SecurityOptions"`
	}
	start := strings.IndexByte(string(out), '{')
	if start < 0 {
		return dockerEngineInfo{}, false
	}
	if err := json.NewDecoder(strings.NewReader(string(out)[start:])).Decode(&raw); err != nil {
		return dockerEngineInfo{}, false
	}
	info := dockerEngineInfo{
		root:   strings.TrimSpace(raw.DockerRootDir),
		name:   strings.TrimSpace(raw.Name),
		driver: strings.TrimSpace(raw.Driver),
	}
	for _, pair := range raw.DriverStatus {
		// Engine 29's default image store reports the containerd snapshotter
		// through DriverStatus — snapshots and the metadata db then live
		// under the containerd root, possibly a different filesystem than
		// DockerRootDir (#5382 review).
		if len(pair) == 2 && pair[0] == "driver-type" && strings.Contains(pair[1], "containerd") {
			info.containerdStore = true
		}
	}
	for _, opt := range raw.SecurityOptions {
		switch opt {
		case "name=rootless":
			info.rootless = true
		case "name=userns":
			info.userns = true
		}
	}
	return info, true
}

// dockerContainerdRoot is the conventional data root of the containerd
// image store: the docker API does not expose containerd's configured root,
// so the default install path is the only honest candidate. It is probed
// only when the engine reports a containerd-backed driver, and only through
// the same co-location proof the data root must pass — an absent or foreign
// path skips rather than fabricating a verdict.
const dockerContainerdRoot = "/var/lib/containerd"

// dockerDataRootCandidates lists the daemon-side write roots the capacity
// probe must answer for: the reported data root always, plus the containerd
// store root when the image store is containerd-backed — a full containerd
// filesystem fails the same `docker run` while the data root still has room
// (#5382 review).
func dockerDataRootCandidates(info dockerEngineInfo) []string {
	roots := []string{info.root}
	if info.containerdStore && dockerContainerdRoot != info.root {
		roots = append(roots, dockerContainerdRoot)
	}
	return roots
}

// dockerRootFSFullEvidence derives the resource verdict docker's combined
// output cannot carry: any provisioning step's stderr loses the engine-side
// errno into text, but a co-located engine's data-root filesystem answers it
// from THIS process — statfs needs no write access to the root-owned dir, and
// zero allocatable blocks (or inodes) is exactly the ENOSPC dockerd's own
// write hit. Every gate that cannot prove the verdict is about the engine's
// own filesystem returns nil instead — no evidence rather than evidence about
// the wrong filesystem (#5382 review).
func (p *dockerProvisioner) dockerRootFSFullEvidence(op dockerOpClass) error {
	endpoint, _, err := resolveDockerEngineEndpoint(p.dockerEnvironment())
	if err != nil || !dockerEndpointOnThisHost(endpoint) {
		return nil
	}
	// The engine must sit in af's filesystem namespace for a local statfs to
	// mean anything. The daemon's self-reported name is its UTS hostname —
	// matching os.Hostname() is the co-location evidence; a Docker Desktop VM
	// or a container with a mounted socket reports its own.
	host, herr := os.Hostname()
	if herr != nil || host == "" {
		return nil
	}
	out, err := p.docker(dockerShortStepTimeout, "info", "--format", dockerEngineInfoFormat)
	if err != nil {
		return nil
	}
	info, ok := parseDockerEngineInfo(out)
	if !ok || info.root == "" || info.name == "" || info.name != host {
		return nil
	}
	// Mount-identity evidence, not hostname equality: the filesystem the
	// daemon's own mount table shows under its reported data root must be
	// the filesystem af's stat of that path sees (#5382 review). Asking the
	// daemon's namespace directly is what keeps genuine co-location working
	// — a tmpfs /run, a rootlesskit mount namespace — while a socket the
	// daemon never wrote into af's view (a bind under --uts=host, Docker
	// Desktop's artifact, an ssh forward) still cannot prove the root is
	// af's. Anything unreadable or unprovable is not proven.
	// The failed step's writer picks the block counter: a rootless engine's
	// every write is Bavail-bounded, and so is any container-side writer that
	// cannot reach the reserved blocks on a rootful engine. Evaluated once —
	// resolving an image's USER costs a docker call.
	unpriv := info.rootless || p.dockerStepWriterUnprivileged(info, op)
	// Every daemon-side write target is a candidate: the reported data root
	// always, plus the containerd store root when the image store is
	// containerd-backed. Each candidate needs its own co-location proof
	// before its statfs means anything about the engine; a root af cannot
	// prove is skipped, not treated as healthy.
	for _, root := range dockerDataRootCandidates(info) {
		if !daemonSharesDataRootFS(endpoint, root) {
			continue
		}
		var full bool
		var probeErr error
		switch {
		case info.rootless:
			full, probeErr = statfsFullUnprivileged(root)
		case unpriv:
			// The writer cannot reach the reserved blocks, so EITHER
			// counter at zero is its ENOSPC: Bfree for the daemon-side
			// writes the step still makes, Bavail for the unprivileged
			// process's own.
			if full, probeErr = statfsFull(root); !full && probeErr == nil {
				full, probeErr = statfsFullUnprivileged(root)
			}
		default:
			full, probeErr = statfsFull(root)
		}
		if probeErr != nil {
			return fmt.Errorf("could not statfs docker data root %s: %w", root, probeErr)
		}
		if !full {
			continue
		}
		who := "the rootful engine"
		switch {
		case info.rootless:
			who = "the rootless engine"
		case unpriv:
			who = "unprivileged writers on the rootful engine"
		}
		return fmt.Errorf("statfs on docker data root %s reports no allocatable space for %s (blocks or inodes) — writes there would fail ENOSPC: %w",
			root, who, &os.PathError{Op: "statfs", Path: root, Err: syscall.ENOSPC})
	}
	return nil
}

// peerMountinfo reads the answering daemon's own mount table —
// /proc/<pid>/mountinfo. Same-uid peers (rootful af-as-root, rootless
// af-as-that-user) can always read it; a peer af cannot see is unproven.
var peerMountinfo = func(pid int) ([]byte, error) {
	return os.ReadFile("/proc/" + strconv.Itoa(pid) + "/mountinfo")
}

// peerExe reads the answering process's binary identity — /proc/<pid>/exe.
// The link is PTRACE-gated where mountinfo is not: a non-root af gets
// EACCES on a root daemon's exe while the mount table stays readable, so
// a permission error means "identity unproven" — the same blind spot as
// an unreadable table — never a vanished peer. Callers distinguish that
// (serverDenied) from ENOENT/ESRCH, which is a vanished process.
var peerExe = func(pid int) (string, error) {
	return os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
}

// serverVerdict is socketServerPid's tri-state. serverProven names the
// serving dockerd; serverRejected is a proven non-daemon or a vanished
// process; serverDenied is a permission-blind identity read (EACCES/EPERM
// on exe or an fd table af cannot fully enumerate) — the caller treats it
// exactly like an unreadable mount table and takes the uid-0 fallback.
type serverVerdict int

const (
	serverRejected serverVerdict = iota
	serverProven
	serverDenied
)

// isDockerdExe identifies the daemon binary, tolerating the "(deleted)"
// suffix a replaced binary's exe link carries until restart.
func isDockerdExe(exe string) bool {
	return strings.TrimSuffix(filepath.Base(exe), " (deleted)") == "dockerd"
}

// socketServerPid names the process that actually SERVES the socket — the
// real dockerd — not merely the creator SO_PEERCRED reports. /proc/<pid>/exe
// settles the common case (creator is dockerd); the ptrace gate matches
// mountinfo's so the link is readable exactly when the table was. When the
// creator is not dockerd, the activated shape is proven instead: the
// listener's inode (from /proc/net/unix) names the holder set in /proc/*/fd,
// and exactly one holder besides the creator — a dockerd — is the
// socket-activation shape. Any other answer is unproven: a forwarder holds
// its own listener (holder set = itself, no dockerd), an extra inherited fd
// makes the set ambiguous, and an fd table af cannot fully read cannot prove
// who else holds it.
func socketServerPid(peer int, sockPath string) (int, serverVerdict) {
	exe, err := peerExe(peer)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			// af cannot even name the creator — EACCES here is the stock
			// non-root case (root daemon, unprivileged docker-group user).
			return 0, serverDenied
		}
		return 0, serverRejected // ENOENT/ESRCH: the process vanished
	}
	if isDockerdExe(exe) {
		return peer, serverProven
	}
	// The creator is provably not dockerd — fd-activation is the only
	// remaining positive proof: exactly one non-creator holder, a dockerd.
	holders, ok := listenerHolders(sockPath)
	if !ok {
		// The holder set cannot be fully enumerated (foreign-uid fd tables
		// are the same PTRACE gate as exe) — identity unproven.
		return 0, serverDenied
	}
	delete(holders, peer)
	if len(holders) != 1 {
		return 0, serverRejected
	}
	for pid := range holders {
		exe, err := peerExe(pid)
		switch {
		case err != nil && errors.Is(err, os.ErrPermission):
			return 0, serverDenied
		case err != nil || !isDockerdExe(exe):
			return 0, serverRejected
		}
		return pid, serverProven
	}
	return 0, serverRejected
}

// listenerHolders returns the pids holding sockPath's LISTENING unix socket
// open. The listener inode comes from /proc/net/unix (the row carrying
// __SO_ACCEPTCON at this path); holder pids come from scanning /proc/*/fd.
// Both match the ptrace policy mountinfo already lives under — a root af
// enumerates everything; a same-uid af sees what it can prove. An fd table
// af cannot fully enumerate cannot prove the holder set is complete, so the
// second return is false whenever any /proc/<pid>/fd is unreadable.
// listenerScanBudget bounds the /proc/*/fd walk the socket-activation proof
// makes. The scan runs inside the synchronous restore path before the failed
// container is reaped: a pid space of tens of thousands, or one process
// holding hundreds of thousands of descriptors, must yield unproven rather
// than stall the cleanup loop or balloon on a single ReadDir (#5382 review).
const listenerScanBudget = 1500 * time.Millisecond

var listenerHolders = func(sockPath string) (map[int]struct{}, bool) {
	inodes, ok := listenerInodes(sockPath)
	if !ok || len(inodes) == 0 {
		return nil, false
	}
	pids, err := os.ReadDir("/proc")
	if err != nil {
		return nil, false
	}
	deadline := time.Now().Add(listenerScanBudget)
	holders := map[int]struct{}{}
	for _, p := range pids {
		if time.Now().After(deadline) {
			return nil, false // scan budget spent: holder set unproven
		}
		pid, perr := strconv.Atoi(p.Name())
		if perr != nil || pid <= 0 {
			continue
		}
		fdDir, ferr := os.Open("/proc/" + p.Name() + "/fd")
		if ferr != nil {
			if errors.Is(ferr, os.ErrNotExist) {
				continue // the process raced away — not a blind spot
			}
			return nil, false // unreadable fd table: holder set unproven
		}
		hit, complete := false, true
		for !hit {
			if time.Now().After(deadline) {
				complete = false
				break
			}
			// Chunked reads bound a huge descriptor table's footprint and
			// let the deadline bite mid-table; entries returned alongside
			// io.EOF are still valid.
			fds, rerr := fdDir.ReadDir(256)
			for _, fd := range fds {
				link, lerr := os.Readlink("/proc/" + p.Name() + "/fd/" + fd.Name())
				if lerr != nil {
					continue // the fd raced away — not a blind spot
				}
				if ino, found := strings.CutPrefix(link, "socket:["); found {
					if _, isHolder := inodes[strings.TrimSuffix(ino, "]")]; isHolder {
						holders[pid] = struct{}{}
						hit = true
						break
					}
				}
			}
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				complete = false // mid-table error: holder set unproven
				break
			}
		}
		fdDir.Close()
		if !complete {
			return nil, false
		}
	}
	return holders, true
}

// listenerInodes parses /proc/net/unix for the inode of every socket bound
// at sockPath in the accept state (__SO_ACCEPTCON). Columns are
// Num RefCount Protocol Flags Type St Inode Path. The bound path is matched
// both literally and symlink-resolved (/var/run vs /run spellings).
func listenerInodes(sockPath string) (map[string]struct{}, bool) {
	raw, err := os.ReadFile("/proc/net/unix")
	if err != nil {
		return nil, false
	}
	resolved, rerr := filepath.EvalSymlinks(sockPath)
	if rerr != nil {
		resolved = ""
	}
	inodes := map[string]struct{}{}
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) < 8 {
			continue
		}
		flags, ferr := strconv.ParseUint(f[3], 16, 32)
		if ferr != nil || flags&0x00010000 == 0 {
			continue
		}
		if f[7] != sockPath && f[7] != resolved {
			continue
		}
		inodes[f[6]] = struct{}{}
	}
	return inodes, true
}

// daemonSharesDataRootFS is the mount-identity proof hostname equality
// cannot give (#5382 review). SO_PEERCRED names the process answering af's
// socket; mount tables then show which mounted filesystem really backs the
// data root in each side's view. When both tables cover the reported path
// with the same (device, fs-internal root) pair, af's statfs verdict is the
// daemon's verdict — even when the daemon lives in a different mount
// namespace, as every rootless engine does. The compare is table-vs-table,
// never table-vs-stat: btrfs hands each subvolume its own anonymous st_dev,
// so stat() never reports the dev field a mountinfo line shows there; only
// two mount entries naming the same device AND the same subtree identify
// the view. A socket bind-mounted into a container fails here: af's own
// table covers the path with a different (dev, root) than the daemon's.
// Any step that cannot name the peer or read its view is not proven.
func daemonSharesDataRootFS(endpoint, rootDir string) bool {
	sockPath := strings.TrimPrefix(endpoint, "unix://")
	if !strings.HasPrefix(sockPath, "/") || rootDir == "" {
		return false
	}
	conn, err := net.DialTimeout("unix", sockPath, dockerShortStepTimeout)
	if err != nil {
		return false
	}
	defer conn.Close()
	peer, peerUID, err := socketPeerCred(conn)
	if err != nil || peer <= 0 {
		return false
	}
	if _, ok := statDev(rootDir); !ok {
		return false // af cannot stat the reported path — unproven
	}
	// af's own mount table — always readable — is the comparison base for
	// both the proven path and the denied-route fallback.
	self, err := peerMountinfo(os.Getpid())
	if err != nil {
		return false
	}
	peerInfo, err := peerMountinfo(peer)
	if err == nil {
		// Strong proof — but ONLY once the process SERVING the socket is
		// identified as the daemon itself. SO_PEERCRED names the listener's
		// CREATOR: a same-uid forwarder (socat, ssh -L) is a creator whose
		// readable mountinfo describes AF's filesystem, not the remote
		// engine's; and under socket activation (dockerd -H fd:// behind
		// docker.socket) the creator is PID 1 while dockerd serves via an
		// inherited fd. The mount table used for the compare must be the
		// serving dockerd's (#5382 review).
		srv, verdict := socketServerPid(peer, sockPath)
		switch verdict {
		case serverRejected:
			return false
		case serverProven:
			info := peerInfo
			if srv != peer {
				var serr error
				if info, serr = peerMountinfo(srv); serr != nil {
					info = nil // serving table unreadable — denied route below
				}
			}
			if info != nil {
				return sameCoveringMount(info, self, rootDir)
			}
		}
		// serverDenied (and the unreadable serving table above) fall
		// through to the uid-0 fallback below: a permission-blind identity
		// read is the same blind spot as an unreadable table.
	}
	// Two blind spots land here: /proc/<peer>/mountinfo unreadable (the
	// ptrace gate a hardened system applies), and the identity read above
	// being permission-denied (serverDenied — the stock non-root case,
	// where mountinfo is world-readable but exe/fd need PTRACE_MODE_READ).
	// That deployment is the common case, so fall back to weaker but
	// cross-process evidence (#5382 review): the answering process must
	// be privileged (uid 0 — the real dockerd, never the same-user proxy
	// or ssh forwarder that is the foreign-socket vector), AND the mount
	// tables must agree on what backs the data root.
	if peerUID != 0 {
		return false
	}
	if peerInfo != nil {
		// A readable peer table may veto even though it cannot prove, and
		// may corroborate even though it cannot identify: agreeing with
		// af's own table on the same (dev, root) covering the data root —
		// including a mounted subvolume whose fs-internal root is "/@" —
		// is the positive witness; disagreement is the foreign shape.
		return sameCoveringMount(peerInfo, self, rootDir)
	}
	// With no peer table to cross-check, only a whole-filesystem covering
	// mount in af's own table is proof enough: a non-"/" root could be a
	// foreign partial bind and cannot be told from a legitimate subvolume.
	root, ok := coveringMountRoot(self, rootDir)
	return ok && root == "/"
}

// sameCoveringMount answers whether two mount tables cover path with the
// same mounted filesystem view: identical major:minor device field AND
// identical fs-internal root. The pair, not the device alone, carries the
// compare — a subtree bind can share a device while describing a different
// view, and on btrfs stat() reports the subvolume's own anonymous st_dev,
// never the mountinfo dev field.
func sameCoveringMount(a, b []byte, path string) bool {
	fa, oka := coveringMountEntry(a, path)
	fb, okb := coveringMountEntry(b, path)
	if !oka || !okb {
		return false
	}
	return fa[2] == fb[2] &&
		unescapeMountinfoPath(fa[3]) == unescapeMountinfoPath(fb[3])
}

// coveringMountEntry finds the mount entry covering path in a mountinfo
// table — the longest mount-point prefix — and returns its fields. Mounts
// stack at the same mountpoint (a tmpfs over a bind): later lines shadow
// earlier ones, so the LAST entry at the longest prefix is what a stat
// actually sees. Unparseable or uncovered paths are unproven.
func coveringMountEntry(info []byte, path string) ([]string, bool) {
	if !strings.HasPrefix(path, "/") {
		return nil, false
	}
	var best []string
	bestLen := -1
	for _, line := range strings.Split(string(info), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mp := unescapeMountinfoPath(fields[4])
		if mp != "/" && mp != path && !strings.HasPrefix(path, mp+"/") {
			continue
		}
		if len(mp) >= bestLen {
			bestLen, best = len(mp), fields
		}
	}
	return best, best != nil
}

// coveringMountRoot is the covering mount's root field — "/" for a whole
// filesystem or the rootfs, a subtree path for a partial bind.
func coveringMountRoot(info []byte, path string) (string, bool) {
	fields, ok := coveringMountEntry(info, path)
	if !ok {
		return "", false
	}
	return unescapeMountinfoPath(fields[3]), true
}

func unescapeMountinfoPath(s string) string {
	// mountinfo octal-escapes ' ', '\t', '\n', '\\' — decode so prefix
	// matching sees the real path the daemon reported.
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

// dockerStepWriterUnprivileged answers whether the FAILED operation's writer
// cannot reach the filesystem's root-reserved blocks — the question Bavail
// answers (#5382 review). Only the process that failed matters: a rootful
// dockerd's own writes (docker run, docker cp, the exec state it records)
// are privileged, while a `docker exec` process is bounded by its own fsuid
// ONLY where its write bypasses the overlay — the overlayfs upper is
// accessed with the daemon's mounter credentials, which stay privileged on
// a rootful engine no matter which uid the exec ran as. So an unprivileged
// exec user counts only when the container also has a bind or volume
// mounted from the probed filesystem. The exec user itself is the account
// owner's uid:gid when a session mounts an account, else the CONTAINER's
// resolved user (a `--user` in docker.run_args overrides the image's USER —
// read the running container, not the image). `name=userns` is the daemon's
// default; a container's own HostConfig.UsernsMode can refuse it with
// `--userns=host`, so remap is only counted when the container did not opt
// out.
func (p *dockerProvisioner) dockerStepWriterUnprivileged(info dockerEngineInfo, op dockerOpClass) bool {
	switch op {
	case dockerOpSessionExec, dockerOpRootExec, dockerOpChmod:
	default:
		return false
	}
	// An exec's write only hits a probed filesystem with the exec's own
	// credentials where a mount bypasses the overlay: writes into the
	// container's writable layer go through overlayfs, which accesses its
	// upper with the mounter's credentials — the daemon's, so rootful
	// dockerd reaches reserved blocks no matter which uid the exec ran as.
	// The exception is vfs: it has no overlay mounter, so every in-container
	// write carries the exec's own fsuid whether a bind exists or not. On
	// overlay drivers the bypass must cover a path THIS op writes — a bind
	// or volume whose source lives on a probed filesystem, mounted over a
	// write root of the failed step. An unrelated credential or cache bind
	// sharing that filesystem is not evidence the step's write went through
	// it (#5382 review).
	user, usernsHost, boundDests := p.containerExecIdentity(dockerDataRootCandidates(info))
	bypass := info.driver == "vfs"
	for _, dest := range boundDests {
		for _, w := range p.dockerOpWriteRoots(op) {
			if mountCoversPath(dest, w) {
				bypass = true
			}
		}
	}
	if !bypass {
		return false
	}
	if op == dockerOpRootExec || op == dockerOpChmod {
		// uid 0's own credentials are privileged unless this container is
		// still under the daemon's userns remap. The chmod of the copied
		// `af` binary runs as --user 0:0 too, so it shares this rule.
		return info.userns && !usernsHost
	}
	if info.userns && !usernsHost {
		return true // a remapped uid is an ordinary user on the host fs
	}
	if p.containerUser != "" {
		return dockerUserUnprivileged(p.containerUser)
	}
	return dockerUserUnprivileged(user)
}

// dockerUserUnprivileged classifies a docker user string — "1000",
// "1000:1000", "root", "root:root", a bare name — by whether it can
// allocate root-reserved blocks. Empty and any spelling of uid 0 ("0",
// "0:0", "root", "root:root") are privileged; a nonzero uid is not; a
// name that is not "root" is treated as unprivileged (a named user is an
// ordinary account by definition). The uid component alone decides — a
// root uid carrying a group is still uid 0 (#5382 review).
func dockerUserUnprivileged(user string) bool {
	user = strings.TrimSpace(user)
	uid, _, _ := strings.Cut(user, ":")
	if uid == "" || uid == "root" {
		return false
	}
	if n, err := strconv.ParseUint(uid, 10, 32); err == nil {
		return n != 0
	}
	return true
}

// dockerOpWriteRoots names the in-container roots an op class's writes land
// under — af's own managed mount destinations plus the workspace, the only
// places a failed provisioning step was trying to write. The Bavail verdict
// may only be borrowed when a data-root bind covers one of these (#5382
// review). A root exec touches the account mounts and /workspace for an
// account session (prepareAccountUser mkdir+chowns /af-home and /workspace)
// and the non-account session home (dockerContainerHome, /root) plus
// /workspace for a non-account session (prepareNonAccountUser mkdir+chowns
// /root and /workspace); the non-account home is selected only for a
// non-account session so an unrelated /root bind cannot make an account root
// exec borrow the Bavail window (#5424 review). Symmetrically, the account
// mounts are selected only for an account session — prepareNonAccountUser
// never writes /af-home or /af-account — so an unrelated bind or volume at
// either account path cannot make a non-account root exec borrow the Bavail
// window (#5424 review). /workspace stays a root-exec write root on both
// paths because each prepare step mkdir+chowns it as root. Session execs
// write the workspace, the account dirs a mounted session owns, and /tmp
// for the agent-server banner redirect.
func (p *dockerProvisioner) dockerOpWriteRoots(op dockerOpClass) []string {
	switch op {
	case dockerOpChmod:
		// copyAfBinary's root chmod writes a single path — the copied
		// `af` binary at /usr/local/bin/af — and nothing else, so that
		// path alone is the write root. A bind covering it (at
		// /usr/local/bin/af, /usr/local/bin, or /) may borrow the Bavail
		// window for a chmod failure; an unrelated bind at the home or
		// workspace the chmod never touched may not (#5424 review).
		return []string{dockerAfBinaryPath}
	case dockerOpRootExec:
		roots := []string{dockerWorkspaceDir}
		if p.spec.Account.Dir == "" {
			roots = append(roots, dockerContainerHome)
		} else {
			roots = append(roots, dockerAccountHome, dockerAccountRuntimeHome)
		}
		return roots
	case dockerOpSessionExec:
		return []string{dockerWorkspaceDir, dockerAccountHome, dockerAccountRuntimeHome, "/tmp"}
	}
	return nil
}

// mountCoversPath reports whether a container mount destination contains a
// write root — the mount AT the root or an ancestor of it (a `/` bind
// covers everything). A sibling or child bind says nothing about where the
// failed step wrote.
func mountCoversPath(dest, target string) bool {
	return dest == "/" || dest == target || strings.HasPrefix(target, dest+"/")
}

// containerExecIdentity reads the running container's resolved exec user,
// its per-container userns mode, and the destinations of any mounts that
// give an in-container writer a path onto a probed filesystem with the
// writer's own credentials — what `docker run` actually merged, not what
// the image declared: a `--user` in docker.run_args replaces the image's
// USER for every unpinned exec, `--userns=host` opts this container out of
// the daemon-wide remap, and only a bind or volume mount bypasses the
// overlay layer's mounter credentials (#5382 review). An unreadable answer
// fails closed every way: the empty user is unproven (privileged — no
// invented Bavail verdict), an unknown UsernsMode falls back to the
// engine's proven default, and no proven mount means the exec's writes
// ride the daemon's credentials anyway.
func (p *dockerProvisioner) containerExecIdentity(dataRoots []string) (user string, usernsHost bool, boundDests []string) {
	if p.containerID == "" {
		return "", false, nil
	}
	out, err := p.docker(dockerShortStepTimeout, "inspect", "--format", "{{json .}}", p.containerID)
	if err != nil {
		return "", false, nil
	}
	start := strings.IndexByte(string(out), '{')
	if start < 0 {
		return "", false, nil
	}
	var c struct {
		Config struct {
			User string `json:"User"`
		} `json:"Config"`
		HostConfig struct {
			UsernsMode string `json:"UsernsMode"`
		} `json:"HostConfig"`
		Mounts []struct {
			Type        string `json:"Type"`
			Source      string `json:"Source"`
			Destination string `json:"Destination"`
		} `json:"Mounts"`
	}
	if err := json.NewDecoder(strings.NewReader(string(out)[start:])).Decode(&c); err != nil {
		return "", false, nil
	}
	for _, m := range c.Mounts {
		if m.Type != "bind" && m.Type != "volume" {
			continue
		}
		for _, root := range dataRoots {
			if sameFilesystem(m.Source, root) {
				boundDests = append(boundDests, path.Clean(m.Destination))
				break
			}
		}
	}
	return strings.TrimSpace(c.Config.User), c.HostConfig.UsernsMode == "host", boundDests
}

// sameFilesystem is the st_dev comparison behind the overlay-bypass proof:
// a mount source and the data root share a filesystem only when the kernel
// reports the same device. The stats run through fsprobe.Stat's bounded
// flight — a mount source on an unresponsive NFS/FUSE filesystem must not
// wedge the synchronous restore loop; a timeout is unproven identity,
// never "same" (#5382 review). Seamable so tests need no real mounts.
var sameFilesystem = func(a, b string) bool {
	da, oka := statDev(a)
	db, okb := statDev(b)
	return oka && okb && da == db
}

var statDev = func(path string) (uint64, bool) {
	st, err := fsprobe.Stat(path)
	if err != nil {
		return 0, false
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(s.Dev), true
}
