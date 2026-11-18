// af's own payload delivery for docker sessions (#7577 item 2): the agent-server
// binary and the operator policy reach the container through af-owned read-only
// bind mounts appended LAST in the `docker run` argv — after docker.run_args —
// rather than through `docker cp` + root chmod. A repo-controlled run_args mount
// can then no longer redirect af's writes through a host bind at
// /usr/local/bin/af or /tmp/af-policy.json: there is no write, and a bind
// shadowing the af destination is a docker-level duplicate mount the provisioner
// turns into an actionable error.
//
// A VM-based engine's file sharing is the one place a bind can fail: on macOS
// every docker engine mounts through a sharing layer (Docker Desktop, colima,
// OrbStack, a podman machine), and a source path outside the shared roots
// cannot mount at all. The fallback is the old cp mechanism — but only after a
// mount-table check proves each write target sits on the container's own root
// filesystem (or a tmpfs), refusing any other enclosing mount.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// dockerStagingDirName is the af-owned subtree under the AF home that carries
// each session's staged payload. Living under the daemon's own home keeps the
// bind source readable by a rootless dockerd (which runs as the daemon user)
// and inside Docker Desktop's default file-sharing roots on macOS.
const dockerStagingDirName = "docker-staging"

// dockerStagingSharedRoots are the host paths macOS docker engines share with
// their VM by default — Docker Desktop's /Users + /Volumes + /private + /tmp
// and the same /Users+tmp convention colima/OrbStack/podman-machine use. A
// bind source outside them is refused by the engine's sharing layer; the cp
// fallback covers sessions whose AF home lives elsewhere. A package var (like
// dockerExec) so tests can pin the platform boundary.
var dockerStagingSharedRoots = []string{"/Users", "/Volumes", "/private", "/tmp"}

// dockerStagingGOOS reports the client platform for the shared-root check — a
// package var so tests can exercise the macOS boundary on any host.
var dockerStagingGOOS = runtime.GOOS

// preparePayloadDelivery stages the payload af owns into a per-session dir
// under the AF home and chooses the delivery mechanism: read-only binds
// appended last in `docker run` — no cp and no chmod — wherever a bind
// provably mounts, and the cp fallback otherwise. Binds are skipped only on a
// Docker Desktop engine whose file-sharing roots exclude the staging dir;
// there the staged files stay as the cp source (their modes already match
// what the container must see) and verifyWriteTargets gates the write.
//
// Staging is best-effort: a failed stage falls back to the un-staged cp+chmod
// path — the exact pre-#7577 mechanism, still behind the mount-table proof —
// rather than failing a session the old path would have delivered.
func (p *dockerProvisioner) preparePayloadDelivery() {
	var err error
	p.stagingDir, err = p.dockerStagingDir()
	if err == nil {
		err = p.stageContainerPayload(p.stagingDir)
	}
	if err != nil {
		log.WarningLog.Printf("docker runtime: cannot stage the container payload (%v); delivering it via `docker cp` after the mount-table check", err)
		p.stagedAfPath, p.stagedPolicyPath = "", ""
	}
	p.payloadViaBinds = p.stagedAfPath != "" && p.stagingShareable(p.stagingDir)
	if p.stagedAfPath != "" && !p.payloadViaBinds {
		log.InfoLog.Printf("docker runtime: staging dir %s is outside the engine's shared file roots; delivering af's payload via `docker cp` after the mount-table check", p.stagingDir)
	}
}

// runPayloadContainer runs `docker run` with the staged binds as the last
// mount arguments. A bind source the sharing layer still refuses — a Docker
// Desktop config the upfront check could not see, or a Desktop install
// reporting an unusual engine name — retries once without the binds rather
// than failing the session for a delivery mechanism.
func (p *dockerProvisioner) runPayloadContainer() error {
	err := p.runContainer()
	if err == nil || !p.payloadViaBinds || !dockerSharingFailure(err.Error()) {
		return err
	}
	log.WarningLog.Printf("docker runtime: `docker run` refused the staged binds; retrying once with `docker cp` delivery: %v", err)
	p.payloadViaBinds = false
	if p.containerID != "" {
		if _, rerr := p.docker(dockerShortStepTimeout, "rm", "-f", p.containerID); rerr != nil {
			log.WarningLog.Printf("docker runtime: cleanup of the sharing-refused container %s failed: %v", p.shortID(), rerr)
		}
	}
	p.resetForRunRetry()
	return p.runContainer()
}

// deliverPayload gets af's files into the container. On the bind path there is
// nothing to write — the mounts already serve them. The cp fallback is the
// only path that writes into the container at all, so it is the only path
// verifyWriteTargets must prove stays on the container's own filesystem (or a
// tmpfs) — a run_args mount at the destination would otherwise carry af's
// write onto a host path the repo chose (#7577 item 2's exact hole).
func (p *dockerProvisioner) deliverPayload() error {
	if !p.payloadViaBinds {
		if err := p.verifyWriteTargets(); err != nil {
			return err
		}
	}
	if err := p.copyAfBinary(); err != nil {
		return dockerStepErr(err, dockerOpDaemonWrite)
	}
	if err := p.copyPolicy(); err != nil {
		return dockerStepErr(err, dockerOpDaemonWrite)
	}
	return nil
}

// dockerStagingDir is the per-session staging root beneath the AF home: mode
// 0700, unique per provision, removed on reap. The unique suffix keeps two
// projects that share a session title from colliding, and makes a leftover
// directory identifiable rather than ambiguous.
func (p *dockerProvisioner) dockerStagingDir() (string, error) {
	afHome, err := config.GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("backend=docker: cannot resolve the AF home for the session staging dir: %w", err)
	}
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("backend=docker: cannot name the session staging dir: %w", err)
	}
	dir := filepath.Join(afHome, dockerStagingDirName, Slugify(p.spec.Title)+"-"+hex.EncodeToString(suffix[:]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("backend=docker: cannot create the session staging dir %s: %w", dir, err)
	}
	return dir, nil
}

// stageContainerPayload copies every file af owns into the staging dir with the
// exact mode the container must see: the af binary 0755 (exec'able by the
// session user, which may be non-root) and the operator policy 0644 (world-
// readable like the old chmod made it — it holds hook commands and program
// names, no credential). Both the bind path and the cp fallback copy from
// here, so the in-container mode never depends on the host file's own bits —
// `docker cp` preserves the source mode and a bind carries it verbatim, which
// is what lets the root `chmod` execs go away entirely.
func (p *dockerProvisioner) stageContainerPayload(dir string) error {
	afBytes, err := os.ReadFile(p.afBin)
	if err != nil {
		return fmt.Errorf("backend=docker: cannot read the af binary %s for container staging: %w", p.afBin, err)
	}
	afStaged := filepath.Join(dir, "af")
	if err := os.WriteFile(afStaged, afBytes, 0o755); err != nil {
		return fmt.Errorf("backend=docker: cannot stage the af binary into %s: %w", dir, err)
	}
	// Guard the file-bind contract: docker creates a DIRECTORY at a missing
	// source path, silently turning the mount into an empty dir over
	// /usr/local/bin/af. Staging wrote the file a line ago; a vanished source
	// here is a real defect, not a race to ride.
	if st, err := os.Stat(afStaged); err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("backend=docker: the staged af binary %s is not a regular file; refusing to bind-mount a missing source", afStaged)
	}
	p.stagedAfPath = afStaged
	if len(p.spec.AgentPolicy) > 0 {
		policyStaged := filepath.Join(dir, "af-policy.json")
		if err := os.WriteFile(policyStaged, p.spec.AgentPolicy, 0o644); err != nil {
			return fmt.Errorf("backend=docker: cannot stage the operator policy into %s: %w", dir, err)
		}
		if st, err := os.Stat(policyStaged); err != nil || !st.Mode().IsRegular() {
			return fmt.Errorf("backend=docker: the staged operator policy %s is not a regular file; refusing to bind-mount a missing source", policyStaged)
		}
		p.stagedPolicyPath = policyStaged
	}
	return nil
}

// stagingMounts returns the argv tail af appends LAST in `docker run`: one
// read-only bind per staged file, mode-selected by the same bindMountRelabel
// rule every af-installed bind uses (#3589). Appended after docker.run_args and
// every -e pin so nothing the repo supplies can reorder it; where a run_args
// mount claims the same destination outright, Docker rejects the pair as a
// duplicate mount point — runContainer maps that to an actionable error.
func (p *dockerProvisioner) stagingMounts() []string {
	if !p.payloadViaBinds || p.stagedAfPath == "" {
		return nil
	}
	mode := "ro"
	if p.bindMountRelabel() {
		mode = "ro,z"
	}
	args := []string{"-v", p.stagedAfPath + ":" + dockerAfBinaryPath + ":" + mode}
	if p.stagedPolicyPath != "" {
		args = append(args, "-v", p.stagedPolicyPath+":"+dockerPolicyPath+":"+mode)
	}
	return args
}

// stagingShareable reports whether dir can reach a container through a bind on
// this engine. The boundary is the client platform, not the engine name:
// dockerd cannot run on macOS natively, so EVERY engine reachable from a macOS
// client — Docker Desktop, colima, OrbStack, a podman machine — mounts through
// a VM file-sharing layer, and a source outside the shared roots is a hard
// failure rather than a mount. The shared-roots list covers the defaults all
// of them share (/Users, /Volumes, /private — /tmp itself resolves beneath
// /private, so the comparison walks symlink-resolved real paths); an engine
// configured with extra shares simply takes the cp fallback, which is always
// correct. On Linux a local bind works wherever dockerd can read the path,
// and the staging root is the daemon-owned AF home, which a rootless dockerd
// (the daemon's own user) reads by construction. A Windows daemon cannot
// produce an af binary a Linux container can exec either way, so the bind
// path is no worse than the cp it replaces there.
func (p *dockerProvisioner) stagingShareable(dir string) bool {
	if dockerStagingGOOS != "darwin" {
		return true
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		resolved = dir
	}
	resolved = filepath.Clean(resolved)
	for _, root := range dockerStagingSharedRoots {
		root = filepath.Clean(root)
		if resolved == root || strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// dockerSharingFailure reports whether a `docker run` failure is the file-
// sharing refusal Docker Desktop produces for a bind source outside its shared
// roots. The wording has moved across releases ("Mounts denied", "not shared
// from the host", guidance pointing at file-sharing settings); matching any of
// them is safe because a match only flips the run to the cp path, which is
// valid on every engine.
func dockerSharingFailure(out string) bool {
	out = strings.ToLower(out)
	return strings.Contains(out, "mounts denied") ||
		strings.Contains(out, "not shared from the host") ||
		strings.Contains(out, "file sharing") ||
		strings.Contains(out, "sharing the path")
}

// dockerDuplicateMountError rewrites Docker's duplicate-mount refusal into an
// actionable error: a run_args mount claimed one of the destinations af's
// staged payload installs last, and only one can win.
func dockerDuplicateMountError(out string, err error) error {
	if !strings.Contains(strings.ToLower(out), "duplicate mount point") {
		return nil
	}
	return fmt.Errorf("backend=docker: `docker run` failed for a duplicate mount destination: %s — a docker.run_args mount shadows a path af must install itself (%s%s); remove that run_args entry or move it to another destination",
		strings.TrimSpace(out), dockerAfBinaryPath, ", "+dockerPolicyPath)
}

// mountEntry is one mountinfo row's usable slice: the mount point and its
// filesystem type. parseMountPoints pre-dates the need for the type; the
// write-target check needs it to allow tmpfs while refusing host-backed
// mounts at the same destination.
type mountEntry struct {
	point  string
	fstype string
}

// parseMountEntries parses /proc/self/mountinfo like parseMountPoints but keeps
// each row's filesystem type: fields after the "-" separator begin with the
// fstype. Same fail-closed contract — an unparseable line or a table with no
// root mount is an error, never a partial answer.
func parseMountEntries(mountinfo string) ([]mountEntry, error) {
	var entries []mountEntry
	sawRoot := false
	seen := map[string]bool{}
	for _, line := range strings.Split(mountinfo, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 7 {
			return nil, fmt.Errorf("unparseable mountinfo line %q", line)
		}
		mp := path.Clean(unescapeMountinfo(fields[4]))
		if !strings.HasPrefix(mp, "/") {
			return nil, fmt.Errorf("unparseable mountinfo mount point %q", fields[4])
		}
		sep := -1
		for i := 5; i < len(fields); i++ {
			if fields[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+1 >= len(fields) {
			return nil, fmt.Errorf("unparseable mountinfo line %q", line)
		}
		if mp == "/" {
			sawRoot = true
		}
		if !seen[mp] {
			seen[mp] = true
			entries = append(entries, mountEntry{point: mp, fstype: fields[sep+1]})
		}
	}
	if !sawRoot {
		return nil, fmt.Errorf("mountinfo has no root mount")
	}
	return entries, nil
}

// enclosingMountEntry is enclosingMount with the mount's fstype attached: the
// longest mount point equal to p or a path prefix of it. "/" with no real
// mount entry is impossible on a parsed table (parseMountEntries requires the
// root mount), so a missing entry means p resolves under a mount point the
// table declared, or under "/" itself.
func enclosingMountEntry(entries []mountEntry, p string) mountEntry {
	p = path.Clean(p)
	best := mountEntry{point: "/", fstype: ""}
	for _, e := range entries {
		if (e.point == p || strings.HasPrefix(p, e.point+"/")) && len(e.point) > len(best.point) {
			best = e
		}
	}
	return best
}

// leafSymlinkMarker prefixes the lines the write-target probe prints for each
// destination whose final component is a symlink. A symlinked destination is
// refused outright: `docker cp` resolves it and writes wherever the target
// points, which the resolved-parent + mount-table check cannot bound.
const leafSymlinkMarker = "AF-LEAF-SYMLINK "

// verifyWriteTargets proves every path af is about to `docker cp` into lands on
// the container's own root filesystem — or on a tmpfs, whose contents never
// reach a host file — before the cp runs. A bind at the destination or an
// ancestor would otherwise let the write land on a host path the repo chose
// (#7577 item 2's exact hole). This runs only on the cp fallback: when the
// staging binds are used there is no write to guard.
//
// One exec resolves both destination parents and reads the container's own
// /proc/self/mountinfo — the same table shape the chown guard uses — then a
// `test -L` tail marks any symlinked destination leaf. Three cases get refused:
// a non-rootfs non-tmpfs mount enclosing the resolved destination, a non-tmpfs
// mount below it (cp writes into a destination that is a directory), and a
// symlinked leaf, whose write escapes the checked path entirely.
//
// The mount table is read through the container's /bin/sh — image code. That
// reader is the documented residual: an image can already run anything as
// root, so a forged table changes nothing about what it can do. A repo's
// docker.run_args mount, by contrast, cannot hide itself from the kernel's
// table — the mount the shadow lives on is itself listed.
func (p *dockerProvisioner) verifyWriteTargets() error {
	dirA := path.Dir(dockerAfBinaryPath)
	dirB, leafB := dirA, dockerAfBinaryPath
	if len(p.spec.AgentPolicy) > 0 {
		dirB = path.Dir(dockerPolicyPath)
		leafB = dockerPolicyPath
	}
	// The read needs no privilege — mountinfo, cd and test -L on the world-
	// readable destination parents are answerable by the image's default
	// user — so it runs without --user like every other session exec. An
	// image that makes those paths unsearchable refuses the write instead.
	out, err := p.docker(dockerShortStepTimeout,
		"exec", p.containerID, dockerContainerShell, "-c",
		// No mkdir -p: /usr/local/bin and /tmp exist on any image that can
		// host a container at all, and a cd failure is a refused write, the
		// fail-closed verdict this guard exists to give.
		`cd -- "$1" && pwd -P && cd -- "$2" && pwd -P && cat /proc/self/mountinfo && for f in "$3" "$4"; do if [ -L "$f" ]; then printf '`+leafSymlinkMarker+`%s\n' "$f"; fi; done`,
		"sh", dirA, dirB, dockerAfBinaryPath, leafB)
	if err != nil {
		return dockerStepErr(fmt.Errorf("backend=docker: cannot read the container mount table, so af's files are not written (a cp must stay on the container's own filesystem): %s: %w", strings.TrimSpace(string(out)), err), dockerOpSessionExec)
	}
	lines := strings.SplitN(string(out), "\n", 3)
	if len(lines) < 3 || !strings.HasPrefix(lines[0], "/") || !strings.HasPrefix(lines[1], "/") || strings.TrimSpace(lines[2]) == "" {
		return dockerStepErr(fmt.Errorf("backend=docker: the container mount table answer was not understood, so af's files are not written: %q", strings.TrimSpace(string(out))), dockerOpLocal)
	}
	var mountLines []string
	for _, line := range strings.Split(lines[2], "\n") {
		if strings.HasPrefix(line, leafSymlinkMarker) {
			leaf := strings.TrimSpace(strings.TrimPrefix(line, leafSymlinkMarker))
			return dockerStepErr(fmt.Errorf("backend=docker: refusing to write %s: the destination is a symbolic link in the image, so `docker cp` would follow it somewhere the mount-table check cannot prove is the container's own filesystem; use an image whose %s is a real path",
				leaf, leaf), dockerOpLocal)
		}
		mountLines = append(mountLines, line)
	}
	entries, err := parseMountEntries(strings.Join(mountLines, "\n"))
	if err != nil {
		return dockerStepErr(fmt.Errorf("backend=docker: cannot parse the container mount table, so af's files are not written (a cp must stay on the container's own filesystem): %w", err), dockerOpLocal)
	}
	for i, realParent := range []string{strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])} {
		target := dockerAfBinaryPath
		if i == 1 {
			if len(p.spec.AgentPolicy) == 0 {
				continue
			}
			target = dockerPolicyPath
		}
		realTarget := path.Join(realParent, path.Base(target))
		entry := enclosingMountEntry(entries, realTarget)
		if entry.point != "/" && entry.fstype != "tmpfs" {
			return dockerStepErr(fmt.Errorf("backend=docker: refusing to write %s: it resolves to %s, inside the mount %s (%s) rather than the container's root filesystem — a docker.run_args mount there would carry af's write onto the host; remove that mount or move it elsewhere",
				target, realTarget, entry.point, entry.fstype), dockerOpLocal)
		}
		// A directory at the destination makes cp write beneath it, onto
		// whatever mount owns the matching name. Any non-tmpfs mount below
		// the resolved target could catch that write.
		for _, e := range entries {
			if strings.HasPrefix(e.point, realTarget+"/") && e.fstype != "tmpfs" {
				return dockerStepErr(fmt.Errorf("backend=docker: refusing to write %s: the mount %s (%s) sits beneath the destination, where a cp landing inside it would reach the host — a docker.run_args mount there would carry af's write onto the host; remove that mount or move it elsewhere",
					target, e.point, e.fstype), dockerOpLocal)
			}
		}
	}
	return nil
}

// resetForRunRetry clears the container handle and reap latch after a
// sharing-refused `docker run`, so the cp-path retry creates a fresh container
// whose own reap later removes it rather than collapsing onto the first
// container's latched result.
func (p *dockerProvisioner) resetForRunRetry() {
	p.reapMu.Lock()
	defer p.reapMu.Unlock()
	p.containerID = ""
	p.reaped = false
	p.reapErr = nil
}
