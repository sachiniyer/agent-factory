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
//     question: a rootful dockerd allocates reserved blocks (Bfree), a
//     rootless one writes as an ordinary user and only reaches Bavail
//     (docker info reports `name=rootless` in SecurityOptions).
//   - The statfs must succeed and actually report exhaustion — blocks or
//     inodes. Every unresolved or unproven step returns nil instead: unknown
//     is not-proven, never a "full" verdict.
package session

import (
	"encoding/json"
	"fmt"
	"os"
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

// dockerEngineInfo is the slice of `docker info` the capacity probe consults:
// where the engine writes, which host it believes it runs on, and whether it
// runs rootless. A field the engine did not report stays empty and keeps the
// corresponding proof from being made.
type dockerEngineInfo struct {
	root     string
	name     string
	rootless bool
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
		if opt == "name=rootless" {
			info.rootless = true
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
func (p *dockerProvisioner) dockerRootFSFullEvidence() error {
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
	probe := statfsFull
	if info.rootless {
		probe = statfsFullUnprivileged
	}
	full, err := probe(info.root)
	if err != nil {
		return fmt.Errorf("could not statfs docker data root %s: %w", info.root, err)
	}
	if !full {
		return nil
	}
	mode := "rootful"
	if info.rootless {
		mode = "rootless"
	}
	return fmt.Errorf("statfs on docker data root %s reports no allocatable space for the %s engine (blocks or inodes) — engine writes there would fail ENOSPC: %w",
		info.root, mode, &os.PathError{Op: "statfs", Path: info.root, Err: syscall.ENOSPC})
}
