// af's own payload delivery for docker sessions (#7577 item 2): the agent-server
// binary and the operator policy reach the container through af-owned read-only
// bind mounts appended LAST in the `docker run` argv — after docker.run_args —
// rather than through `docker cp` + root chmod. A repo-controlled run_args mount
// can then no longer redirect af's writes through a host bind at
// /usr/local/bin/af or /etc/af-policy.json: there is no write, and a bind
// shadowing the af destination is a docker-level duplicate mount the provisioner
// turns into an actionable error.
//
// A VM-based engine's file sharing is the one place a bind can fail: on macOS
// every docker engine mounts through a sharing layer (Docker Desktop, colima,
// OrbStack, a podman machine), and a source path outside the shared roots
// cannot mount at all. The fallback is the old cp mechanism — but only after a
// mount-table check proves each write target sits on the container's own root
// filesystem, refusing any other enclosing mount, tmpfs included.
package session

import (
	"bytes"
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

// dockerStagingInContainer reports whether af itself runs inside a container —
// the marker files every docker-run (/.dockerenv) and podman/k8s
// (/run/.containerenv) container gets, then PID 1's cgroup for runtimes that
// create neither. A containerized af behind a mounted socket talks to the
// HOST's dockerd, which cannot see af's container-local staging dir. A var so
// tests can pin the boundary.
var dockerStagingInContainer = func() bool {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	if data, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		for _, token := range []string{"docker", "kubepods", "containerd", "libpod"} {
			if strings.Contains(string(data), token) {
				return true
			}
		}
	}
	return false
}

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
	switch {
	case p.stagedAfPath == "":
		// Staging failed — the whole pre-#7577 cp+chmod path runs behind the guard.
	case !p.stagingShareable(p.stagingDir):
		log.InfoLog.Printf("docker runtime: staging dir %s is not visible to the engine host as af sees it (shared-root, endpoint, or containerized-daemon check); delivering af's payload via `docker cp` after the mount-table check", p.stagingDir)
	case strings.ContainsAny(p.stagingDir, ":,"):
		// dockerAccountMount's rule: ':' splits a -v spec's fields and ',' splits
		// --mount's. The staged files still carry the right modes, so the guarded
		// cp fallback needs no bind at all.
		log.InfoLog.Printf("docker runtime: staging dir %s contains ':' or ',', which a bind-mount spec cannot encode; delivering af's payload via `docker cp` after the mount-table check", p.stagingDir)
	case runArgsInheritMounts(p.runArgs):
		// --volumes-from inherits the donor's mounts at the donor's own
		// container paths — destinations af cannot enumerate pre-flight. A
		// donor binding a host dir over an af destination would turn the staged
		// file bind into a root-owned stub in that host dir, so inherited
		// mounts take cp delivery, where verifyWriteTargets reads the donor's
		// real mounts inside the created container rather than guessing.
		log.InfoLog.Printf("docker runtime: docker.run_args inherits mounts via --volumes-from, whose destinations af cannot enumerate; delivering af's payload via `docker cp` after the mount-table check")
	default:
		p.payloadViaBinds = true
	}
}

// runPayloadContainer runs `docker run` with the staged binds as the last
// mount arguments. A bind source the sharing layer still refuses — a Docker
// Desktop config the upfront check could not see, or a Desktop install
// reporting an unusual engine name — retries once without the binds rather
// than failing the session for a delivery mechanism. A post-create delivery
// check confirms the staged files actually arrived — the visibility proof can
// false-positive (a unix:// endpoint behind an ssh forward or a snap/lima
// daemon with a different filesystem view) — and its failure takes the same
// one-shot cp retry.
func (p *dockerProvisioner) runPayloadContainer() error {
	if err := p.refuseEnclosingRunArgsMounts(); err != nil {
		// A preflight configuration refusal — no docker run happened, so the
		// engine wrote nothing. Classifying it local keeps a full data root's
		// ENOSPC evidence off a permanently invalid config, which the restore
		// classifier would otherwise retry forever.
		return dockerStepErr(err, dockerOpLocal)
	}
	err := p.runContainer()
	switch {
	case err != nil && (!p.payloadViaBinds || !dockerSharingFailure(err.Error())):
		return err
	case err != nil:
		log.WarningLog.Printf("docker runtime: `docker run` refused the staged binds; retrying once with `docker cp` delivery: %v", err)
		return p.retryRunWithCpDelivery(fmt.Sprintf("`docker run` refused the staged binds: %v", err))
	case p.payloadViaBinds:
		// The same exec that confirms the staged files arrived verifies every
		// root-exec program against the kernel mount table (#7577 item 3): the
		// visibility proof can false-positive, so an answer the staged binary
		// never printed takes the one-shot cp retry — but a FAIL verdict is a
		// real refusal, not a delivery failure, and no cp re-delivers around it.
		outcome, verr := p.verifyRootExecPrograms()
		switch {
		case outcome == dockerRootVerifyRefused:
			return dockerStepErr(verr, dockerOpLocal)
		case outcome == dockerRootVerifyUndelivered:
			log.WarningLog.Printf("docker runtime: the staged binds did not deliver af's payload (%v); retrying once with `docker cp` delivery", verr)
			return p.retryRunWithCpDelivery("the staged binds did not deliver af's payload")
		default:
			return nil
		}
	default:
		return nil // cp delivery stays its own step after the boundary checks
	}
}

// retryRunWithCpDelivery removes the just-created container, drops the bind
// decision, and runs once more so the cp fallback delivers the payload — the
// single retry shared by a sharing-layer refusal and a failed post-create
// delivery confirmation.
func (p *dockerProvisioner) retryRunWithCpDelivery(cause string) error {
	if p.containerID != "" {
		if _, rerr := p.docker(dockerShortStepTimeout, "rm", "-f", p.containerID); rerr != nil {
			// Keep the handle: resetForRunRetry clearing it would orphan a
			// container no record reaches — the orphan sweeper spares a live
			// session's slug. Failing the provision hands it to
			// reapProvisionFailure's `docker rm -f`, which either removes it or
			// confirms it gone.
			return fmt.Errorf("backend=docker: %s and `docker rm -f %s` on the container also failed (%v); abandoning the retry rather than leaking the first container",
				cause, p.shortID(), rerr)
		}
	}
	p.payloadViaBinds = false
	p.resetForRunRetry()
	if err := p.runContainer(); err != nil {
		return err
	}
	return p.deliverPayload()
}

// stagingDestinations returns the in-container paths af's payload claims:
// the af binary always, the policy whenever one will be delivered — staged
// or not, since a cp write to /etc needs the same mount check as a bind.
func (p *dockerProvisioner) stagingDestinations() []string {
	dsts := []string{dockerAfBinaryPath}
	if p.stagedPolicyPath != "" || len(p.spec.AgentPolicy) > 0 {
		dsts = append(dsts, dockerPolicyPath)
	}
	return dsts
}

// runArgsMountDestinations extracts (entry, destination) pairs for every
// mount-shaped run_args token: -v/--volume specs (the second ':'-field, or
// the only field for an anonymous volume), --mount's target/destination/dst
// key, and --tmpfs's mount point. Attached and combined shorthand spellings
// (-v/host:/dst, -tv /host:/dst) are walked with the account guard's pflag
// machinery — an ambiguous cluster is an error rather than a guess.
func runArgsMountDestinations(runArgs []string) ([][2]string, error) {
	var out [][2]string
	for i := 0; i < len(runArgs); i++ {
		a := runArgs[i]
		var spec, entry string
		var isMount, isTmpfs bool
		next := func() string {
			i++
			if i < len(runArgs) {
				return runArgs[i]
			}
			return ""
		}
		switch {
		case a == "-v" || a == "--volume":
			spec = next()
			entry = a + " " + spec
		case strings.HasPrefix(a, "-v=") || strings.HasPrefix(a, "--volume="):
			spec = strings.SplitN(a, "=", 2)[1]
			entry = a
		case a == "--mount":
			spec = next()
			entry = a + " " + spec
			isMount = true
		case strings.HasPrefix(a, "--mount="):
			spec = strings.SplitN(a, "=", 2)[1]
			entry = a
			isMount = true
		case a == "--tmpfs":
			spec = next()
			entry = a + " " + spec
			isTmpfs = true
		case strings.HasPrefix(a, "--tmpfs="):
			spec = strings.SplitN(a, "=", 2)[1]
			entry = a
			isTmpfs = true
		case len(a) > 2 && a[0] == '-' && a[1] != '-':
			// An attached value or a combined short option — Docker's pflag
			// honors a -v inside either. Walk it with the account guard's
			// cluster machinery, recording each -v's destination; an
			// ambiguous shape fails closed rather than skipping a mount the
			// refusal check cannot see.
			if err := dockerCheckShorthandCluster(a, runArgs, i, "v",
				func(_ byte, value string) error {
					if dest := volumeSpecDest(value); dest != "" {
						out = append(out, [2]string{a, dest})
					}
					return nil
				},
				"the bind-delivery path", "a -v"); err != nil {
				return nil, err
			}
			continue
		default:
			continue
		}
		if spec == "" {
			continue
		}
		dest := ""
		switch {
		case isMount:
			// Docker reads --mount as one CSV record with case-folded keys —
			// the same helpers the account-boundary guard uses.
			for _, kv := range dockerMountFields(spec) {
				if v, ok := dockerMountTarget(kv); ok {
					dest = v
				}
			}
		case isTmpfs:
			dest = spec
		default:
			dest = volumeSpecDest(spec)
		}
		if dest != "" {
			out = append(out, [2]string{entry, dest})
		}
	}
	return out, nil
}

// volumeSpecDest reports the container path a -v spec mounts: the second
// ':'-field for src:dst[:opts], or the only field for an anonymous volume.
func volumeSpecDest(spec string) string {
	fields := strings.Split(spec, ":")
	if len(fields) > 1 {
		return fields[1]
	}
	return fields[0]
}

// runArgsInheritMounts reports whether run_args carries --volumes-from in
// either spelling. The single-dash form is not the option — pflag reads it as
// -v carrying the value "olumes-from" — so only the double-dash spellings
// count here.
func runArgsInheritMounts(runArgs []string) bool {
	for _, a := range runArgs {
		if a == "--volumes-from" || strings.HasPrefix(a, "--volumes-from=") {
			return true
		}
	}
	return false
}

// refuseEnclosingRunArgsMounts refuses — before the container starts — any
// run_args mount whose destination equals or encloses a path af must install.
// Such a mount is NOT a duplicate Docker can reject: af's file bind targets a
// path INSIDE it, so runc creates a root-owned zero-byte stub in the host
// source directory so the mount exists, and that stub persists after the
// session (#7577 item 2 R1). The cp fallback does not rescue it either — the
// same mount would be refused by the write-target guard — so it is refused
// outright with an actionable error naming the entry.
func (p *dockerProvisioner) refuseEnclosingRunArgsMounts() error {
	if !p.payloadViaBinds {
		return nil
	}
	mounts, err := runArgsMountDestinations(p.runArgs)
	if err != nil {
		return err
	}
	if len(mounts) == 0 {
		return nil
	}
	targets := p.stagingDestinations()
	// Lexical judging is not enough: an image may symlink a destination's
	// parent (/usr/local/bin -> /opt/bin) and a repo bind at /opt/bin would
	// then receive the mountpoint stub — and the inverse (/opt -> /usr/local)
	// makes a repo mount at /opt/bin effectively cover /usr/local/bin. A
	// throwaway `docker run --rm` probe resolves af's destination parents AND
	// every repo mount's destination inside the image, and both sides are
	// judged on the resolved paths. It only runs when repo mounts exist, so
	// the common no-mount case never pays for an extra container. If
	// resolution cannot be established (no shell, unreadable answer, a
	// symlinked af leaf, or an unresolvable mount dest), the relationship to
	// repo mounts cannot be established either — switch to guarded cp
	// delivery rather than guessing.
	resolved, resMounts, leafLink, perr := p.probeResolvedDestinations(mounts)
	if perr != nil || leafLink {
		log.InfoLog.Printf("docker runtime: cannot resolve af's destination paths inside the image (probe=%v leaf-symlink=%v); delivering af's payload via `docker cp` after the mount-table check", perr, leafLink)
		p.payloadViaBinds = false
		return nil
	}
	for i, m := range mounts {
		lexical := path.Clean("/" + m[1])
		for _, t := range append(targets, resolved...) {
			for _, dest := range []string{lexical, resMounts[i]} {
				if dest == t || dest == "/" || strings.HasPrefix(t, dest+"/") {
					return fmt.Errorf("backend=docker: docker.run_args entry %q mounts at %s, which encloses %s — af must install that path itself, and the engine would create a root-owned stub inside the host source directory so its mount exists; remove that run_args entry or move it to another destination",
						m[0], m[1], t)
				}
			}
		}
	}
	return nil
}

// probeResolvedDestinations runs a throwaway `docker run --rm <image> sh -c …`
// resolving inside the image: each af destination's parent directory AND every
// repo mount's destination (full `cd` resolution, falling back to parent+leaf
// for not-yet-existing leaves and file destinations). Returns the resolved af
// file destinations and the resolved mount destinations in mount order, plus
// whether an af destination leaf is itself a symlink.
func (p *dockerProvisioner) probeResolvedDestinations(mounts [][2]string) ([]string, []string, bool, error) {
	dsts := p.stagingDestinations()
	dirA := path.Dir(dsts[0])
	dirB, leafB := dirA, dsts[0]
	if len(dsts) > 1 {
		dirB = path.Dir(dsts[1])
		leafB = dsts[1]
	}
	argv := []string{"sh", dirA, dirB, dsts[0], leafB}
	for _, m := range mounts {
		argv = append(argv, m[1])
	}
	// --entrypoint is required: Docker appends positional args to the image's
	// own ENTRYPOINT, which would run the image's workload instead of this
	// probe. The af.session label keeps a timed-out probe reapable — the
	// wrapper kills only the CLI and --rm needs the container to exit.
	args := []string{"run", "--rm", "--label", dockerSessionLabel + "=" + Slugify(p.spec.Title),
		"--entrypoint", dockerContainerShell, p.image, "-c",
		dockerRootScriptPrefix + `{ cd -- "$1" && pwd -P && printf '\0'; } || exit 3; { cd -- "$2" && pwd -P && printf '\0'; } || exit 3; for f in "$3" "$4"; do if [ -L "$f" ]; then printf '` + leafSymlinkMarker + `%s\n' "$f"; fi; done; printf '\0'; shift 4; for d in "$@"; do r="$d"; until cd -- "$r" 2>/dev/null; do r="${r%/*}"; [ -z "$r" ] && r="/"; done; printf '%s%s\n' "$(pwd -P)" "${d#"$r"}"; printf '\0'; done`}
	args = append(args, argv...)
	out, err := p.docker(dockerShortStepTimeout, args...)
	if err != nil {
		return nil, nil, false, err
	}
	want := 3 + len(mounts)
	// Every field is NUL-terminated, so the split must produce want fields
	// plus an empty tail — a short answer is truncated, a longer one smuggles
	// fields the last mount check would silently absorb.
	parts := bytes.Split(out, []byte{0})
	if len(parts) != want+1 || len(parts[want]) != 0 {
		return nil, nil, false, fmt.Errorf("probe answer not understood: %q", string(out))
	}
	resolved := make([]string, 0, len(dsts))
	for i := 0; i < len(dsts); i++ {
		dir, ok := bytes.CutSuffix(parts[i], []byte("\n"))
		if !ok || len(dir) == 0 || dir[0] != '/' {
			return nil, nil, false, fmt.Errorf("probe answer not understood: %q", string(out))
		}
		resolved = append(resolved, string(dir)+"/"+path.Base(dsts[i]))
	}
	resMounts := make([]string, len(mounts))
	for i := range mounts {
		d := bytes.TrimSuffix(parts[3+i], []byte{0})
		d = bytes.TrimSuffix(d, []byte("\n"))
		ds := string(d)
		if ds == probeFailMarker || !strings.HasPrefix(ds, "/") {
			return nil, nil, false, fmt.Errorf("mount destination %q cannot be resolved inside the image", mounts[i][1])
		}
		resMounts[i] = path.Clean(ds)
	}
	return resolved, resMounts, bytes.Contains(parts[2], []byte(leafSymlinkMarker)), nil
}

// deliverPayload gets af's files into the container on the cp fallback — the
// only path that writes into the container at all (the binds already serve
// them), so the only path the mount guards must keep on the container's own
// filesystem (#7577 item 2's exact hole). Three layers, in order: the daemon's
// own `docker inspect` mount list clears the destination before the binary's
// cp; the just-written binary — reached only through a cleared path — then
// reads the kernel mount table itself and re-proves its placement plus the
// policy destination and every root-exec program; and the policy cp runs
// last, behind the verified answer (#7577 item 3).
func (p *dockerProvisioner) deliverPayload() error {
	if p.payloadViaBinds || p.payloadDelivered {
		return nil
	}
	if err := p.inspectWriteTargetsGuard(); err != nil {
		return dockerStepErr(err, dockerOpLocal)
	}
	if err := p.copyAfBinary(); err != nil {
		return dockerStepErr(err, dockerOpDaemonWrite)
	}
	outcome, verr := p.verifyRootExecPrograms()
	switch {
	case outcome == dockerRootVerifyRefused:
		return dockerStepErr(verr, dockerOpLocal)
	case outcome != dockerRootVerifyOK:
		// An exec that could not run is a session-exec failure — a full
		// docker data root can refuse exec-state creation, and that ENOSPC
		// must reach the capacity-evidence classifier, not the structural
		// retry budget a local refusal consumes (#7607 review).
		return dockerStepErr(fmt.Errorf("backend=docker: the copied af binary did not run its root-exec verification, so its placement and the mount table are unproven: %w", verr), dockerOpSessionExec)
	}
	if err := p.copyPolicy(); err != nil {
		return dockerStepErr(err, dockerOpDaemonWrite)
	}
	p.payloadDelivered = true
	return nil
}

// dockerStagingDir is the per-session staging root beneath the AF home: mode
// 0700, unique per provision, removed on reap. The unique suffix keeps two
// projects that share a session title from colliding, and makes a leftover
// directory identifiable rather than ambiguous. The leaf is created with an
// EXCLUSIVE os.Mkdir: a pre-existing leaf is collision or residue, so a fresh
// suffix is tried rather than silently sharing or reusing a directory.
func (p *dockerProvisioner) dockerStagingDir() (string, error) {
	afHome, err := config.GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("backend=docker: cannot resolve the AF home for the session staging dir: %w", err)
	}
	root := filepath.Join(afHome, dockerStagingDirName)
	if err := config.MkdirAllUnderAFHome(root, 0o700); err != nil {
		return "", fmt.Errorf("backend=docker: cannot create the session staging root %s: %w", root, err)
	}
	for range 4 {
		var suffix [4]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", fmt.Errorf("backend=docker: cannot name the session staging dir: %w", err)
		}
		dir := filepath.Join(root, Slugify(p.spec.Title)+"-"+hex.EncodeToString(suffix[:]))
		err := os.Mkdir(dir, 0o700)
		switch {
		case err == nil:
			return dir, nil
		case os.IsExist(err):
			continue
		default:
			return "", fmt.Errorf("backend=docker: cannot create the session staging dir %s: %w", dir, err)
		}
	}
	return "", fmt.Errorf("backend=docker: cannot create the session staging dir under %s: every candidate name already exists", root)
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
	// WriteFile applies the daemon's umask — under 0077 the file lands 0700 and
	// a non-root container user cannot exec it. Pin the mode the contract means;
	// both delivery paths carry it verbatim now that no in-container chmod runs.
	if err := os.Chmod(afStaged, 0o755); err != nil {
		return fmt.Errorf("backend=docker: cannot set the staged af binary's mode in %s: %w", dir, err)
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
		if err := os.Chmod(policyStaged, 0o644); err != nil {
			return fmt.Errorf("backend=docker: cannot set the staged policy's mode in %s: %w", dir, err)
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
// this engine. Docker resolves a bind source on the DAEMON host, so the staged
// files reach the container only when dockerd sees the same path af wrote:
// the endpoint must carry the dockerEndpointOnThisHost proof (a tcp://
// loopback endpoint can be an `ssh -L` forward to a remote engine), and af
// itself must not be containerized — a unix socket mounted into af's
// container is the HOST's dockerd, while the staging dir lives in af's own
// container filesystem, where the run either fails or — per Docker's own
// bind-mount contract — a missing source is created as a directory, leaving
// an empty dir over /usr/local/bin/af. Neither outcome reaches the
// sharing-retry, so the answer must be the always-correct cp fallback.
//
// On macOS the boundary is the VM file-sharing layer, not the engine name:
// dockerd cannot run natively there, so EVERY reachable engine — Docker
// Desktop, colima, OrbStack, a podman machine — mounts through it, and a
// source outside the shared roots is a hard failure rather than a mount. The
// shared-roots list covers the defaults all of them share (/Users, /Volumes,
// /private — /tmp itself resolves beneath /private, so the comparison walks
// symlink-resolved real paths); an engine configured with extra shares simply
// takes the cp fallback, which is always correct. A Windows daemon cannot
// produce an af binary a Linux container can exec either way, so the bind
// path is no worse than the cp it replaces there.
func (p *dockerProvisioner) stagingShareable(dir string) bool {
	endpoint, _, err := p.dockerEngineEndpoint()
	if err != nil || !dockerEndpointOnThisHost(endpoint) || dockerStagingInContainer() {
		return false
	}
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
	return fmt.Errorf("backend=docker: `docker run` failed for a duplicate mount destination: %s — two mounts claim the same in-container path; if a docker.run_args entry shadows a path af must install itself (%s%s), remove that entry or move it to another destination",
		strings.TrimSpace(out), dockerAfBinaryPath, ", "+dockerPolicyPath)
}

// dockerReadOnlyMountpointError rewrites runc's mountpoint-creation failure —
// the error a read-only parent bind produces when the engine tries to create
// the destination node for af's staged bind inside it — into an actionable
// error instead of raw runc text.
func dockerReadOnlyMountpointError(out string, err error) error {
	if !strings.Contains(strings.ToLower(out), "creating mountpoint") {
		return nil
	}
	return fmt.Errorf("backend=docker: `docker run` failed creating the mountpoint for af's staged bind: %s — a docker.run_args mount covers a destination af installs (%s%s) with a read-only bind, so the engine cannot create the mount node inside it; remove that run_args entry or make it writable",
		strings.TrimSpace(out), dockerAfBinaryPath, ", "+dockerPolicyPath)
}

// leafSymlinkMarker prefixes the lines the write-target probe prints for each
// destination whose final component is a symlink. A symlinked destination is
// refused outright: `docker cp` resolves it and writes wherever the target
// points, which the resolved-parent + mount-table check cannot bound.
const leafSymlinkMarker = "AF-LEAF-SYMLINK "

// probeFailMarker is emitted for a repo mount destination that cannot be
// resolved inside the image — the mount relationship then cannot be
// established and bind delivery yields to the guarded cp fallback.
const probeFailMarker = "AF-PROBE-FAIL"

// dockerStagingRecordName is the persistable half of the staging path: its
// base name only. The cleanup record is operator-visible storage, and a full
// path in it would be a deletion target a tampered record could aim anywhere;
// the base name re-roots under the af-owned staging root on restore instead.
func dockerStagingRecordName(dir string) string {
	if dir == "" {
		return ""
	}
	return filepath.Base(dir)
}

// restoredDockerStagingDir re-binds a persisted staging dir name for a
// teardown rebuilt after a daemon restart. Only the exact shape provisioning
// mints — Slugify(title)+"-"+exactly 8 lowercase hex — is accepted: any other
// prefix-shaped name is a stale or tampered record, and degrades to the
// pre-field leak rather than removing a directory this session cannot prove
// it owns.
func restoredDockerStagingDir(title, recorded string) string {
	slug := Slugify(title)
	if recorded == "" || filepath.Base(recorded) != recorded || !strings.HasPrefix(recorded, slug+"-") {
		return ""
	}
	suffix := recorded[len(slug)+1:]
	if len(suffix) != 8 {
		return ""
	}
	for _, c := range suffix {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ""
		}
	}
	afHome, err := config.GetConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(afHome, dockerStagingDirName, recorded)
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
