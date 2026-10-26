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
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"golang.org/x/sys/unix"
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
	dockerOpRootExec
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
	root     string
	name     string
	rootless bool
	userns   bool
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
		DockerRootDir   string   `json:"DockerRootDir"`
		Name            string   `json:"Name"`
		SecurityOptions []string `json:"SecurityOptions"`
	}
	start := strings.IndexByte(string(out), '{')
	if start < 0 {
		return dockerEngineInfo{}, false
	}
	if err := json.NewDecoder(strings.NewReader(string(out)[start:])).Decode(&raw); err != nil {
		return dockerEngineInfo{}, false
	}
	info := dockerEngineInfo{
		root: strings.TrimSpace(raw.DockerRootDir),
		name: strings.TrimSpace(raw.Name),
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
	if !daemonSharesDataRootFS(endpoint, info.root) {
		return nil
	}
	// The failed step's writer picks the block counter: a rootless engine's
	// every write is Bavail-bounded, and so is any container-side writer that
	// cannot reach the reserved blocks on a rootful engine. Evaluated once —
	// resolving an image's USER costs a docker call.
	unpriv := info.rootless || p.dockerStepWriterUnprivileged(info, op)
	var full bool
	var probeErr error
	switch {
	case info.rootless:
		full, probeErr = statfsFullUnprivileged(info.root)
	case unpriv:
		// The writer cannot reach the reserved blocks, so EITHER counter at
		// zero is its ENOSPC: Bfree for the daemon-side writes the step still
		// makes, Bavail for the unprivileged process's own.
		if full, probeErr = statfsFull(info.root); !full && probeErr == nil {
			full, probeErr = statfsFullUnprivileged(info.root)
		}
	default:
		full, probeErr = statfsFull(info.root)
	}
	if probeErr != nil {
		return fmt.Errorf("could not statfs docker data root %s: %w", info.root, probeErr)
	}
	if !full {
		return nil
	}
	who := "the rootful engine"
	switch {
	case info.rootless:
		who = "the rootless engine"
	case unpriv:
		who = "unprivileged writers on the rootful engine"
	}
	return fmt.Errorf("statfs on docker data root %s reports no allocatable space for %s (blocks or inodes) — writes there would fail ENOSPC: %w",
		info.root, who, &os.PathError{Op: "statfs", Path: info.root, Err: syscall.ENOSPC})
}

// peerMountinfo reads the answering daemon's own mount table —
// /proc/<pid>/mountinfo. Same-uid peers (rootful af-as-root, rootless
// af-as-that-user) can always read it; a peer af cannot see is unproven.
var peerMountinfo = func(pid int) ([]byte, error) {
	return os.ReadFile("/proc/" + strconv.Itoa(pid) + "/mountinfo")
}

// daemonSharesDataRootFS is the mount-identity proof hostname equality
// cannot give (#5382 review). SO_PEERCRED names the process answering af's
// socket; its mount table then shows which device really backs the data
// root in the DAEMON's view. When that device equals af's stat of the same
// path, af's statfs verdict is the daemon's verdict — even when the daemon
// lives in a different mount namespace, as every rootless engine does, or
// sits under a tmpfs /run bind-visible mounts could never describe. A
// socket bind-mounted into a container fails here: the answering daemon's
// mountinfo shows a different device than af's container-rootfs stat. Any
// step that cannot name the peer or read its view is not proven.
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
	peer, err := socketPeerPid(conn)
	if err != nil || peer <= 0 {
		return false
	}
	dev, ok := statDev(rootDir)
	if !ok {
		return false
	}
	info, err := peerMountinfo(peer)
	if err != nil {
		return false
	}
	peerDev, ok := coveringMountDev(info, rootDir)
	return ok && peerDev == dev
}

// coveringMountDev finds the mount entry covering path in a mountinfo
// table — the longest mount-point prefix — and returns its st_dev. The
// mount the daemon reports under its data root is the filesystem it
// writes to; matching device numbers across namespaces name the same
// filesystem object. Unparseable or uncovered paths are unproven.
func coveringMountDev(info []byte, path string) (uint64, bool) {
	if !strings.HasPrefix(path, "/") {
		return 0, false
	}
	bestLen, bestDev := -1, uint64(0)
	for _, line := range strings.Split(string(info), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mp := unescapeMountinfoPath(fields[4])
		if mp != "/" && !strings.HasPrefix(path, mp+"/") {
			continue
		}
		if len(mp) <= bestLen {
			continue
		}
		var maj, min uint64
		if _, err := fmt.Sscanf(fields[2], "%d:%d", &maj, &min); err != nil {
			continue
		}
		bestLen, bestDev = len(mp), unix.Mkdev(uint32(maj), uint32(min))
	}
	return bestDev, bestLen >= 0
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
	case dockerOpSessionExec, dockerOpRootExec:
	default:
		return false
	}
	// An exec's write only hits the probed filesystem with the exec's own
	// credentials when a mount bypasses the overlay: writes into the
	// container's writable layer go through overlayfs, which accesses its
	// upper with the mounter's credentials — the daemon's, so rootful
	// dockerd reaches reserved blocks no matter which uid the exec ran as.
	// A bind or volume whose source lives on the data-root filesystem is
	// the path that proves the writer's own fsuid applies (#5382 review).
	user, usernsHost, boundDataRoot := p.containerExecIdentity(info.root)
	if !boundDataRoot {
		return false
	}
	if op == dockerOpRootExec {
		// uid 0's own credentials are privileged unless this container is
		// still under the daemon's userns remap.
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

// containerExecIdentity reads the running container's resolved exec user,
// its per-container userns mode, and whether any of its mounts give an
// in-container writer a path onto the probed data-root filesystem with the
// writer's own credentials — what `docker run` actually merged, not what
// the image declared: a `--user` in docker.run_args replaces the image's
// USER for every unpinned exec, `--userns=host` opts this container out of
// the daemon-wide remap, and only a bind or volume mount bypasses the
// overlay layer's mounter credentials (#5382 review). An unreadable answer
// fails closed every way: the empty user is unproven (privileged — no
// invented Bavail verdict), an unknown UsernsMode falls back to the
// engine's proven default, and no proven mount means the exec's writes
// ride the daemon's credentials anyway.
func (p *dockerProvisioner) containerExecIdentity(dataRoot string) (user string, usernsHost bool, boundDataRoot bool) {
	if p.containerID == "" {
		return "", false, false
	}
	out, err := p.docker(dockerShortStepTimeout, "inspect", "--format", "{{json .}}", p.containerID)
	if err != nil {
		return "", false, false
	}
	start := strings.IndexByte(string(out), '{')
	if start < 0 {
		return "", false, false
	}
	var c struct {
		Config struct {
			User string `json:"User"`
		} `json:"Config"`
		HostConfig struct {
			UsernsMode string `json:"UsernsMode"`
		} `json:"HostConfig"`
		Mounts []struct {
			Type   string `json:"Type"`
			Source string `json:"Source"`
		} `json:"Mounts"`
	}
	if err := json.NewDecoder(strings.NewReader(string(out)[start:])).Decode(&c); err != nil {
		return "", false, false
	}
	for _, m := range c.Mounts {
		if (m.Type == "bind" || m.Type == "volume") && sameFilesystem(m.Source, dataRoot) {
			boundDataRoot = true
			break
		}
	}
	return strings.TrimSpace(c.Config.User), c.HostConfig.UsernsMode == "host", boundDataRoot
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
