package session

// The host-side half of the docker backend's root-exec verification (#7577
// item 3, plan option B: mountinfo-verified exec paths). The property is
// single: every program af runs as root inside the container is resolved
// INSIDE the running container — all mounts applied — component by
// component, and each materialized component plus every symlink hop's
// target must sit on a mount whose covering mount is "/"; anything that
// cannot be resolved or verified fails closed. A docker.run_args mount at
// /bin/cat lands on /bin/busybox and supplies every applet af execs at once,
// but it cannot hide from the kernel's mount table, and it cannot substitute
// the staged af binary that reads it: the staged binary is af's own trusted
// executor, delivered by item 2's read-only binds or by a mount-checked cp.
//
// The verification is one `docker exec` of the staged binary as
// `af __docker-root-verify v1 <checks>`, run right after the payload lands and
// before any root script runs, so every exec site downstream of it — the
// account boundary's stat and cat, the prepare steps' mkdir/chown/find, the
// mount-table reads — runs only verified programs. The same exec carries the
// delivery checks the image's wc/test used to do (the staged binary's size,
// the policy file's readability) and, on the cp path, re-proves the write
// destinations against the kernel table.
//
// Programs are checked in two spellings: absolute exec targets (/bin/sh,
// /bin/cat, the stat candidates, and the cp'd af itself) and the bare names
// the root scripts resolve through PATH — with PATH now pinned inside every
// root script to /usr/sbin:/usr/bin:/sbin:/bin, the same list the verifier
// searches, so a docker.run_args `-e PATH=/evil` chooses nothing.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/dockerrootverify"
	"github.com/sachiniyer/agent-factory/log"
)

// dockerRootProgramPaths are the absolute exec targets af runs as root: the
// script interpreter, the mount-table reader, and the account-owner stat
// candidates. The cp'd af binary joins them on the cp path — a bind at its
// destination would have turned cp's write into a host file, so the verifier
// re-proves the binary's own placement on the kernel table. On the bind path
// the binary is the mount — the verifier af runs is the thing being checked —
// so it stays off the list.
var dockerRootProgramPaths = []string{
	dockerContainerShell,
	dockerContainerCat,
	dockerContainerStatPaths[0],
	dockerContainerStatPaths[1],
}

// dockerRootProgramNames are the bare names af's scripts resolve through the
// pinned PATH — mkdir and cat from the mount-table reads, chown and find from
// the prepare steps, chmod and id from the account/user scripts, git from the
// workspace scripts. Shell builtins (cd, pwd, test, printf, kill, echo) run no
// program and need no check.
var dockerRootProgramNames = []string{"cat", "chmod", "chown", "find", "git", "id", "mkdir"}

// dockerRootScriptPrefix pins PATH at the head of every script af execs as
// root, so a docker.run_args `-e PATH` cannot choose the program any bare
// name resolves to. The value is the verifier package's own pin list — the
// script's PATH and the check's search order can never drift apart.
const dockerRootScriptPrefix = "export PATH=" + dockerrootverify.PinnedPATH + "; "

// dockerRootVerifyOutcome is the three-way answer verifyRootExecPrograms
// gives: every check passed, a check refused, or the staged binary did not
// run at all — which on the bind path means "the binds did not deliver af's
// payload" and takes the one-shot cp retry.
type dockerRootVerifyOutcome int

const (
	dockerRootVerifyOK dockerRootVerifyOutcome = iota
	dockerRootVerifyRefused
	dockerRootVerifyUndelivered
)

// verifyRootExecPrograms execs the staged af binary's verifier inside the
// container as root and judges its verdicts. A FAIL verdict is a refused
// configuration — a docker.run_args mount sits where a root-executed program
// or write destination must not have one — returned as an error naming the
// mount. An exec that produced no verdicts means the binary never ran:
// undelivered, not refused.
func (p *dockerProvisioner) verifyRootExecPrograms() (dockerRootVerifyOutcome, error) {
	checks := p.rootVerifyChecks()
	argv := []string{"exec", "--user", "0:0", p.containerID, dockerAfBinaryPath,
		dockerrootverify.Arg, dockerrootverify.Protocol}
	argv = append(argv, checks...)
	out, err := p.docker(dockerShortStepTimeout, argv...)
	var refusals, delivery []string
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "FAIL") {
			continue
		}
		// prog/write/mountinfo verdicts are mount-security refusals. A
		// size or file verdict is a delivery miss — the staged bind
		// arrived empty or wrong-sized, which the one-shot cp retry was
		// built for — never a mount to refuse (#7607 review).
		switch {
		case strings.HasPrefix(line, "FAIL prog "), strings.HasPrefix(line, "FAIL write "), strings.HasPrefix(line, "FAIL mountinfo"):
			refusals = append(refusals, line)
		default:
			delivery = append(delivery, line)
		}
	}
	if len(refusals) > 0 {
		return dockerRootVerifyRefused, fmt.Errorf(
			"backend=docker: refusing to run root commands in the container: a docker.run_args mount can supply a program af runs as root, or sit where af must write — the kernel mount table says:\n%s\nremove the mount covering the named path, or move it elsewhere",
			strings.Join(refusals, "\n"))
	}
	if len(delivery) > 0 {
		return dockerRootVerifyUndelivered, fmt.Errorf("the staged af payload did not arrive intact: %s", strings.Join(delivery, "; "))
	}
	if err != nil {
		log.WarningLog.Printf("docker runtime: root-exec verification could not run in container %s (%v): %s", p.shortID(), err, strings.TrimSpace(string(out)))
		return dockerRootVerifyUndelivered, fmt.Errorf("the staged af binary did not run its root-exec verification: %w", err)
	}
	return dockerRootVerifyOK, nil
}

// rootVerifyChecks builds the verifier's tagged argv: the staged binary's
// identity, the policy's readability on the bind path, the write destinations
// and the cp'd binary's own placement on the cp path, and every program a
// root script or absolute exec uses.
func (p *dockerProvisioner) rootVerifyChecks() []string {
	var checks []string
	if size := p.payloadSize(); size > 0 {
		checks = append(checks, fmt.Sprintf("size=%d", size))
	}
	if !p.payloadViaBinds {
		// The cp just landed the binary; both verdicts apply to its
		// destination — prog= proves the resolved path is unmounted (a
		// symlink leaf would resolve the check onto its target), write=
		// proves the leaf is no symlink and no mount sits beneath. The
		// policy destination is judged before its cp runs below.
		checks = append(checks, "prog="+dockerAfBinaryPath, "write="+dockerAfBinaryPath)
		if p.stagedPolicyPath != "" || len(p.spec.AgentPolicy) > 0 {
			checks = append(checks, "write="+dockerPolicyPath)
		}
	} else if p.stagedPolicyPath != "" {
		checks = append(checks, "file="+dockerPolicyPath)
	}
	for _, prog := range dockerRootProgramPaths {
		checks = append(checks, "prog="+prog)
	}
	for _, name := range dockerRootProgramNames {
		checks = append(checks, "prog="+name)
	}
	return checks
}

// payloadSize is the byte count the verifier's self-check compares against:
// the staged file when staging succeeded, the daemon's own af otherwise.
func (p *dockerProvisioner) payloadSize() int64 {
	source := p.stagedAfPath
	if source == "" {
		source = p.afBin
	}
	st, err := os.Stat(source)
	if err != nil || !st.Mode().IsRegular() {
		return 0
	}
	return st.Size()
}

// inspectWriteTargetsGuard is the cp path's pre-write mount check: every
// filesystem the daemon installed for the container — run_args binds,
// volumes, tmpfs mounts, volumes-from donors — is read from `docker inspect`
// and refused when a mount destination LEXICALLY encloses an af destination.
// It is a failfast, not the authority: resolution is judged only by the
// in-container verifier, which walks every component and symlink hop after
// all mounts are applied — an image symlink that hops through a mount's own
// content, or a destination whose parents the image lacks, is invisible to
// any host-side reading of the pristine image (#7607 review). inspect is the
// daemon's own word, so this read cannot be forged by anything mounted inside
// the container; the verifier then proves the resolved truth before any root
// script runs.
func (p *dockerProvisioner) inspectWriteTargetsGuard() error {
	out, err := p.docker(dockerShortStepTimeout, "inspect", "--format", "{{json .}}", p.containerID)
	if err != nil {
		return fmt.Errorf("backend=docker: cannot inspect the container's mounts, so af's files are not written (a cp must stay on the container's own filesystem): %s: %w",
			strings.TrimSpace(string(out)), err)
	}
	inspected, err := parseDockerInspectMounts(out)
	if err != nil {
		return fmt.Errorf("backend=docker: cannot read the container's mounts, so af's files are not written (a cp must stay on the container's own filesystem): `docker inspect` output could not be read as JSON (%s): %w",
			boundedOutputExcerpt(out), err)
	}
	// Every mount the daemon configured is judged — the account bind at
	// dockerAccountHome included. (configuredMountPaths intentionally skips
	// the account bind — it serves the boundary check, where the bind itself
	// is expected; here it is just another place af must not write.)
	var dests []string
	for _, m := range inspected.Mounts {
		dests = append(dests, m.Destination)
	}
	for mp := range inspected.HostConfig.Tmpfs {
		dests = append(dests, mp)
	}
	for _, d := range dests {
		dest := path.Clean("/" + d)
		for _, t := range p.stagingDestinations() {
			if dest == t || dest == "/" || strings.HasPrefix(t, dest+"/") {
				return fmt.Errorf("backend=docker: refusing to write %s: the mount %s encloses the destination, and a cp landing inside it would reach the host; remove that docker.run_args mount or move it elsewhere",
					t, d)
			}
		}
	}
	return nil
}

// parseDockerInspectMounts reads the mounts a `docker inspect` reports: the
// plain call's single-element array, or a `--format`-style lone object. An
// array describing anything but exactly one container, and output that is
// neither shape, is an unreadable source — the caller turns it into a
// refusal, matching parseDockerInspectContainer's rule.
func parseDockerInspectMounts(raw []byte) (dockerInspectContainer, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return parseDockerInspectContainer(raw)
	}
	var c dockerInspectContainer
	if err := json.Unmarshal(raw, &c); err != nil {
		return dockerInspectContainer{}, err
	}
	return c, nil
}
