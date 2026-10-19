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
//     mount namespace: Docker Desktop's VM and an af container bind-mounted
//     the host socket both answer locally while their DockerRootDir lives in
//     a filesystem af cannot see. Same hostname is the observable seam of a
//     shared namespace.
//   - The privilege mode decides WHICH block count answers the fullness
//     question, and the class of the failed step decides whose privilege
//     matters (dockerOpClass): a rootful dockerd allocates reserved blocks
//     (Bfree), a rootless one writes as an ordinary user and only reaches
//     Bavail (docker info reports `name=rootless` in SecurityOptions), and a
//     `docker exec` running as a non-root container user is Bavail-bounded
//     on a rootful engine too whenever its write lands outside the overlay
//     layer's mounter credentials (a bind mount, or any write at all under
//     `name=userns` uid remapping).
//   - The statfs must succeed and actually report exhaustion — blocks or
//     inodes. Every unresolved or unproven step returns nil instead: unknown
//     is not-proven, never a "full" verdict.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

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

// dockerStepWriterUnprivileged answers whether the FAILED operation's writer
// cannot reach the filesystem's root-reserved blocks — the question Bavail
// answers (#5382 review). Only the process that failed matters: a rootful
// dockerd's own writes (docker run, docker cp, the exec state it records)
// are privileged, while a `docker exec` process writing outside the overlay
// layer's mounter credentials — a bind mount, or anything at all under
// userns remapping — is bounded by its own fsuid: the account owner's
// uid:gid when one is mounted, else the image's USER.
func (p *dockerProvisioner) dockerStepWriterUnprivileged(info dockerEngineInfo, op dockerOpClass) bool {
	switch op {
	case dockerOpSessionExec:
		if info.userns {
			return true
		}
		if p.containerUser != "" {
			uid, _, _ := strings.Cut(p.containerUser, ":")
			return uid != "0"
		}
		return p.imageUserUnprivileged()
	case dockerOpRootExec:
		return info.userns
	default:
		return false
	}
}

// imageUserUnprivileged resolves the USER the image runs under — the uid a
// plain `docker exec` inherits when af did not pin one. Empty, "0" and
// "root" are privileged; anything else — a nonzero uid or a non-root name —
// is bounded by Bavail. An unreadable answer is unproven, which fails closed
// to the privileged metric rather than inventing a reserved-block verdict.
func (p *dockerProvisioner) imageUserUnprivileged() bool {
	out, err := p.docker(dockerShortStepTimeout, "image", "inspect", "--format", "{{.Config.User}}", p.image)
	if err != nil {
		return false
	}
	user := strings.TrimSpace(string(out))
	if user == "" || user == "root" {
		return false
	}
	uid, _, _ := strings.Cut(user, ":")
	if n, err := strconv.ParseUint(uid, 10, 32); err == nil {
		return n != 0
	}
	return true
}
