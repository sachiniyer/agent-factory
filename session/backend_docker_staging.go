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
	case p.payloadViaBinds && !p.payloadDeliveryConfirmed():
		log.WarningLog.Printf("docker runtime: the staged binds did not deliver af's payload; retrying once with `docker cp` delivery")
		return p.retryRunWithCpDelivery("the staged binds did not deliver af's payload")
	default:
		return nil
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
	return p.runContainer()
}

// stagingDestinations returns the in-container paths af's staged payload
// claims: the af binary always, the policy when one is staged.
func (p *dockerProvisioner) stagingDestinations() []string {
	dsts := []string{dockerAfBinaryPath}
	if p.stagedPolicyPath != "" {
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
	// resolution cannot be established (no shell, an answer the frame
	// cannot parse, a component the probe cannot verify — #7601 — or a
	// symlinked af leaf), the relationship to repo mounts cannot be
	// established either — switch to guarded cp delivery rather than
	// guessing.
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
// repo mount's destination. A destination that exists resolves by full `cd`;
// a missing one resolves from its deepest existing ancestor with the
// unresolved tail appended — but only past components the probe proves absent
// or non-directory; anything unverifiable fails the destination instead
// (#7601). Each destination resolves independently of the previous one's cwd.
// Returns the resolved af file destinations and the resolved mount
// destinations in mount order, plus whether an af destination leaf is itself
// a symlink.
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
		// A destination that does not exist is resolved from its DEEPEST
		// existing ancestor (#7601): probe walks up one component at a time
		// until a cd succeeds, accumulating the stripped components, then
		// answers ancestor-pwd + '/' + the unresolved tail. The walk may
		// step up only past a component it PROVES absent or non-directory:
		// a failed cd with no -e and no -L is ENOENT, and a failed cd on a
		// non-directory is ENOTDIR. Anything else — an inaccessible
		// directory (EACCES/EPERM), a symlink the probe cannot traverse
		// (ELOOP or a dangling link), an I/O error — is unverifiable: a
		// component af cannot see could be a symlink retargeting the mount
		// onto an af destination, so the walk stops with AF-PROBE-FAIL and
		// the guarded cp fallback stands. A probe that cannot shrink (a
		// relative destination) fails the same way instead of looping.
		`export PATH=/usr/sbin:/usr/bin:/sbin:/bin; { cd -- "$1" && pwd -P && printf '\0'; } || exit 3; { cd -- "$2" && pwd -P && printf '\0'; } || exit 3; for f in "$3" "$4"; do if [ -L "$f" ]; then printf '` + leafSymlinkMarker + `%s\n' "$f"; fi; done; printf '\0'; shift 4; for d in "$@"; do probe="$d"; rest=; while :; do if cd -- "$probe" 2>/dev/null; then if [ -n "$rest" ]; then printf '%s/%s\n' "$(pwd -P)" "$rest"; else pwd -P; fi; break; elif [ -L "$probe" ] || [ -d "$probe" ]; then printf '%s\n' '` + probeFailMarker + `'; break; fi; rest="${probe##*/}${rest:+/$rest}"; case "$probe" in */*) probe="${probe%/*}"; [ -n "$probe" ] || probe=/ ;; *) printf '%s\n' '` + probeFailMarker + `'; break ;; esac; done; printf '\0'; done`}
	args = append(args, argv...)
	out, err := p.docker(dockerShortStepTimeout, args...)
	if err != nil {
		return nil, nil, false, err
	}
	want := 3 + len(mounts)
	parts := bytes.SplitN(out, []byte{0}, want)
	if len(parts) < want {
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

// payloadDeliveryConfirmed checks — after create — that the staged files
// reached their destinations as the staged regular files. A unix:// endpoint
// can still carry a different filesystem view (an ssh-forwarded socket, a
// snap Docker with a private /tmp, a lima or colima VM): the visibility proof
// cannot see those, and a bind source the engine cannot resolve is silently
// replaced by an empty directory Docker creates at the source path. The check
// proves the destination is a regular, executable file of the staged size —
// an empty-dir mount fails `test -f`, a wrong-but-readable source fails the
// size check — so a delivery miss is caught here rather than surfacing later
// as a session that dies with no clear cause.
func (p *dockerProvisioner) payloadDeliveryConfirmed() bool {
	st, err := os.Stat(p.stagedAfPath)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	policyDest := ""
	if p.stagedPolicyPath != "" {
		policyDest = dockerPolicyPath
	}
	out, err := p.docker(dockerShortStepTimeout,
		"exec", p.containerID, dockerContainerShell, "-c",
		// PATH is pinned before any external name (wc, tr) is looked up: a
		// run_args `-e PATH=/evil:…` would otherwise choose the program this
		// root exec runs. What remains — an image binary at the absolute path,
		// or a run_args bind shadowing it — is the repo-chosen-image trust
		// #7577 items 1 and 3 address.
		`export PATH=/usr/sbin:/usr/bin:/sbin:/bin; test -f "$1" && test -x "$1" && [ "$(wc -c < "$1" | tr -d '[:space:]')" = "$2" ] && { [ -z "$3" ] || { test -f "$3" && test -r "$3"; }; }`,
		"sh", dockerAfBinaryPath, fmt.Sprintf("%d", st.Size()), policyDest)
	if err != nil {
		log.WarningLog.Printf("docker runtime: staged-payload delivery check failed in container %s (%v): %s", p.shortID(), err, strings.TrimSpace(string(out)))
	}
	return err == nil
}

// deliverPayload gets af's files into the container. On the bind path there is
// nothing to write — the mounts already serve them. The cp fallback is the
// only path that writes into the container at all, so it is the only path
// verifyWriteTargets must prove stays on the container's own filesystem —
// a run_args mount at the destination would otherwise carry af's
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

// mountEntry is one mountinfo row's usable slice: the mount point and its
// filesystem type. The type no longer decides anything — no fstype is an
// exception to the rootfs rule — but it still names the refused mount in the
// operator-facing error.
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
		// Check the decoded-but-uncleaned point before path.Clean can
		// collapse a "//" prefix: a mount point must be '/' followed by
		// anything but '/' or '\'. Both shapes are never in a real mountinfo
		// (the kernel octal-escapes special bytes), so a table carrying one
		// is hostile or corrupt — refuse it rather than prefix-match a
		// cleaned path that lost the signal.
		mp := unescapeMountinfo(fields[4])
		if len(mp) == 0 || mp[0] != '/' || (len(mp) > 1 && (mp[1] == '/' || mp[1] == '\\')) || strings.ContainsRune(mp, '\\') {
			return nil, fmt.Errorf("unparseable mountinfo mount point %q", fields[4])
		}
		mp = path.Clean(mp)
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

// probeFailMarker is emitted for a repo mount destination the probe cannot
// prove: a component it cannot inspect (an inaccessible directory, a symlink
// it cannot traverse, an I/O error) or one that cannot shrink. The mount
// relationship then cannot be established and bind delivery yields to the
// guarded cp fallback.
const probeFailMarker = "AF-PROBE-FAIL"

// verifyWriteTargets proves every path af is about to `docker cp` into lands on
// the container's own root filesystem before the cp runs. A bind at the
// destination or an ancestor would otherwise let the write land on a host path
// the repo chose (#7577 item 2's exact hole). This runs only on the cp
// fallback: when the staging binds are used there is no write to guard.
//
// One exec resolves both destination parents and reads the container's own
// /proc/self/mountinfo — the same table shape the chown guard uses — then a
// `test -L` tail marks any symlinked destination leaf. Three cases get refused:
// any non-rootfs mount enclosing the resolved destination, any mount below it
// (cp writes into a destination that is a directory), and a symlinked leaf,
// whose write escapes the checked path entirely. There is no fstype exception:
// a private --tmpfs and a host tmpfs bind (-v /dev/shm:/dst) report the same
// fstype, so allowing tmpfs would read a host bind as safe — a write through
// one reaches the HOST, not the container.
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
		// No mkdir -p: /usr/local/bin and /etc exist on any image that can
		// host a container at all, and a cd failure is a refused write, the
		// fail-closed verdict this guard exists to give. Each resolved parent
		// is framed by a NUL the shell prints itself — a NUL can never sit
		// inside a path — so a directory name carrying whitespace or a
		// newline is judged byte-exactly instead of trimmed into a different
		// path. pwd -P's own trailing newline stays in the field.
		// PATH is pinned before the only external name (cat) is looked up, so
		// a run_args `-e PATH=/evil:…` cannot choose the program reading the
		// mount table for a root exec. What remains — an image binary at the
		// absolute path, or a run_args bind shadowing it — is the repo-chosen-
		// image trust #7577 items 1 and 3 address.
		`export PATH=/usr/sbin:/usr/bin:/sbin:/bin; cd -- "$1" && pwd -P && printf '\0' && cd -- "$2" && pwd -P && printf '\0' && cat /proc/self/mountinfo && for f in "$3" "$4"; do if [ -L "$f" ]; then printf '`+leafSymlinkMarker+`%s\n' "$f"; fi; done`,
		"sh", dirA, dirB, dockerAfBinaryPath, leafB)
	if err != nil {
		return dockerStepErr(fmt.Errorf("backend=docker: cannot read the container mount table, so af's files are not written (a cp must stay on the container's own filesystem): %s: %w", strings.TrimSpace(string(out)), err), dockerOpSessionExec)
	}
	parts := bytes.SplitN(out, []byte{0}, 3)
	if len(parts) < 3 || len(parts[2]) == 0 {
		return dockerStepErr(fmt.Errorf("backend=docker: the container mount table answer was not understood, so af's files are not written: %q", string(out)), dockerOpLocal)
	}
	// Each resolved field must end in exactly the newline pwd -P prints —
	// remove that one byte and keep the rest verbatim. An empty or
	// non-absolute field can only be a corrupt or hostile answer; the check
	// refuses rather than guess a path.
	realDirs := make([]string, 2)
	for i, field := range parts[:2] {
		dir, ok := bytes.CutSuffix(field, []byte("\n"))
		if !ok || len(dir) == 0 || dir[0] != '/' {
			return dockerStepErr(fmt.Errorf("backend=docker: the container mount table answer was not understood, so af's files are not written: %q", string(out)), dockerOpLocal)
		}
		realDirs[i] = string(dir)
	}
	var mountLines []string
	for _, line := range strings.Split(string(parts[2]), "\n") {
		if strings.HasPrefix(line, leafSymlinkMarker) {
			leaf := strings.TrimPrefix(line, leafSymlinkMarker)
			return dockerStepErr(fmt.Errorf("backend=docker: refusing to write %s: the destination is a symbolic link in the image, so `docker cp` would follow it somewhere the mount-table check cannot prove is the container's own filesystem; use an image whose %s is a real path",
				leaf, leaf), dockerOpLocal)
		}
		mountLines = append(mountLines, line)
	}
	entries, err := parseMountEntries(strings.Join(mountLines, "\n"))
	if err != nil {
		return dockerStepErr(fmt.Errorf("backend=docker: cannot parse the container mount table, so af's files are not written (a cp must stay on the container's own filesystem): %w", err), dockerOpLocal)
	}
	for i, realParent := range realDirs {
		target := dockerAfBinaryPath
		if i == 1 {
			if len(p.spec.AgentPolicy) == 0 {
				continue
			}
			target = dockerPolicyPath
		}
		realTarget := path.Join(realParent, path.Base(target))
		entry := enclosingMountEntry(entries, realTarget)
		// No fstype exception: the mount table reports a host tmpfs bind
		// (-v /dev/shm:/dst) exactly as a private --tmpfs, so an exception
		// for tmpfs would read a host bind as safe. Only a resolution under
		// "/" itself is the container's own filesystem.
		if entry.point != "/" {
			return dockerStepErr(fmt.Errorf("backend=docker: refusing to write %s: it resolves to %s, inside the mount %s (%s) rather than the container's root filesystem — the mount table cannot tell a private tmpfs from a host tmpfs bind, so any non-rootfs mount is refused; a docker.run_args mount there would carry af's write onto the host; remove that mount or move it elsewhere",
				target, realTarget, entry.point, entry.fstype), dockerOpLocal)
		}
		// A directory at the destination makes cp write beneath it, onto
		// whatever mount owns the matching name. Any mount below the
		// resolved target could catch that write, tmpfs included.
		for _, e := range entries {
			if strings.HasPrefix(e.point, realTarget+"/") {
				return dockerStepErr(fmt.Errorf("backend=docker: refusing to write %s: the mount %s (%s) sits beneath the destination, where a cp landing inside it would reach the host — a docker.run_args mount there would carry af's write onto the host; remove that mount or move it elsewhere",
					target, e.point, e.fstype), dockerOpLocal)
			}
		}
	}
	return nil
}

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
