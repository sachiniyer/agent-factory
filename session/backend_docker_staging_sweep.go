// The orphaned-staging-dir sweep (#7590): <AF home>/docker-staging/<slug>-<hex>
// dirs that no live session record names, reclaimed once the container that
// could bind them is proven gone. Provisioning stages the payload BEFORE the
// first persisted record exists, so a daemon killed in that window leaves a
// directory (about 49 MB — the af binary plus the policy) that no cleanup
// handle ever reaches.
//
// The pass only ever removes what five proofs leave over:
//
//   - A dir whose base name a live session record still claims (the record's
//     StagingDir field) is kept — the teardown handle owns it.
//   - A dir whose slug belongs to a live or still-provisioning session is
//     kept — the mid-create window has a staged dir with no record yet, and
//     `docker run` may not have bound it yet either.
//   - A dir is a candidate only when the daemon generation that minted it is
//     provably DEAD: provisioning records this process's generation dir name
//     (<home>/tunnels/d<pid>-<rand>) beside the payload, and that dir's .lock
//     is flock'd for the owner's life. A live generation — this daemon, or a
//     predecessor mid-upgrade — still holds it, so a dir THIS generation is
//     mid-provisioning (restore, relaunch, handoff, move, a lazily
//     registered row — every path the create admission barrier does not
//     cover) is never a candidate, however it came to be enumerated (#7616
//     R1). The generation sweep removes a dir only once its lock provably
//     freed, so an absent or lock-free generation dir is the dead generation
//     #7590 exists to reclaim; anything unproven — no marker, a malformed
//     name, a lockless dir, an unreadable path — keeps.
//   - A dir is removed only on the engine that created it, under the home
//     spelling that created it: provisioning records the resolved `docker
//     info` ID and the af.home label spelling beside the staged payload, and
//     a candidate whose markers are missing, malformed, name a different
//     engine, or name a different af.home spelling is kept — a container
//     binding it could live where this engine's listing, filtered by this
//     spelling, cannot see it (a symlinked AGENT_FACTORY_HOME aliases the
//     same real dir under another label). An unreachable engine proves
//     nothing either: the whole pass is Unlisted and removes nothing.
//   - On the recorded (current) engine, a dir is removed only when no
//     container labelled af.session=<its slug> AND af.home=<this home> exists
//     in any state. A listed container is the precise "a container that binds
//     it might still exist" answer: same slug is the only label a binding
//     container can carry, and a stopped container still holds the bind
//     across a restart.
package session

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
)

// StagingSweepResult summarizes one orphaned-staging-dir sweep.
type StagingSweepResult struct {
	Listed  int // shape-conforming staging dirs the pass saw
	Reaped  int // proven orphans removed
	Skipped int // dirs kept: record-claimed, slug-protected, or container-bound
	Errors  int // proven orphans the removal could not delete
	// Unlisted is set when the container check could not run to completion —
	// no docker CLI, an unresolvable endpoint, a failed listing, or an engine
	// that changed under the pass — so nothing was proven and nothing was
	// removed. The mirror of OrphanSweepResult.Unlisted (#7447): a missing
	// docker CLI now does not prove an earlier daemon created no container.
	Unlisted bool
	// EngineID is the identity of the engine the listing ran against, empty
	// when the listing could not be trusted.
	EngineID string
}

// dockerStagingDirSuffixLen is the "-xxxxxxxx" tail dockerStagingDir mints:
// a dash plus four random bytes as eight lowercase hex digits.
const dockerStagingDirSuffixLen = 9

// parseStagingDirName splits a staging-dir base name into its session slug and
// reports whether the name is a shape provisioning could have minted —
// Slugify(title)+"-"+eight lowercase hex. Only such names are sweep
// candidates; anything else under the root is left alone (and never removed,
// whatever it is). The slug half must itself be a value Slugify emits: all
// [a-z0-9-], no leading or trailing dash.
func parseStagingDirName(name string) (slug string, ok bool) {
	if len(name) <= dockerStagingDirSuffixLen || name[len(name)-dockerStagingDirSuffixLen] != '-' {
		return "", false
	}
	hexPart := name[len(name)-8:]
	for _, c := range hexPart {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	slug = name[:len(name)-dockerStagingDirSuffixLen]
	if slug == "" || slug[0] == '-' || slug[len(slug)-1] == '-' {
		return "", false
	}
	for _, c := range slug {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return "", false
		}
	}
	return slug, true
}

// SweepOrphanedStagingDirs removes docker-staging dirs no live session record
// claims, once the container that could bind them is proven gone (#7590).
//
// homeID must equal the value runContainer stamps into af.home
// (config.GetConfigDir()): an empty homeID disables the pass — the container
// proof cannot be scoped without it. claimedNames must name every staging-dir
// base name a live session record still owns; protectedSlugs every live or
// still-provisioning session slug. The caller supplies both from the same
// instance snapshot it protects the container sweep with — while its
// skipped-repos set is drained, so neither set is built on a
// known-incomplete view. The keep sets are defense in depth, not the
// candidacy proof: what a mid-pass provision minted is kept by the
// generation marker's held lock, so the pass deliberately runs WITHOUT the
// create admission barrier and never holds it across a filesystem read or
// removal (#7616 R3).
func SweepOrphanedStagingDirs(homeID string, claimedNames, protectedSlugs map[string]bool) StagingSweepResult {
	var result StagingSweepResult
	if strings.TrimSpace(homeID) == "" {
		return result
	}
	home, err := config.GetConfigDir()
	if err != nil {
		log.WarningLog.Printf("staging dir sweep: cannot resolve the AF home; skipping: %v", err)
		return result
	}
	// The verified-root rule the setup-payload sweep already applies: a
	// symlinked docker-staging root is an operator's own layout (a data disk,
	// a root shared between homes), and deleting "orphaned" entries under it
	// would empty the shared TARGET. Enumeration and removal stay confined to
	// the real directory through the opened handle.
	root, rootPath, err := openVerifiedStagingRoot(home, dockerStagingDirName)
	switch {
	case err != nil:
		log.WarningLog.Printf("staging dir sweep: refusing to run: %s: %v", rootPath, err)
		return result
	case root == nil:
		return result
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		log.WarningLog.Printf("staging dir sweep: cannot list %s: %v", rootPath, err)
		return result
	}
	type candidate struct {
		name string
		slug string
	}
	var candidates []candidate
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		slug, ok := parseStagingDirName(entry.Name())
		if !ok {
			log.WarningLog.Printf("staging dir sweep: skipping %q — not a staging-dir name", entry.Name())
			continue
		}
		candidates = append(candidates, candidate{name: entry.Name(), slug: slug})
	}
	result.Listed = len(candidates)
	if len(candidates) == 0 {
		return result
	}

	// The container proof runs on the engine the CLI currently targets, under
	// the same selector refusal and engine-identity discipline the orphan
	// container sweep uses: an ambiguous selector or an unreadable engine is
	// an Unlisted pass — it proved nothing and must remove nothing.
	if _, err := lookPath("docker"); err != nil {
		result.Unlisted = true
		return result
	}
	env := sessionenv.DockerCLIEnvironment(os.Environ(), "", nil)
	if _, _, err := resolveDockerEngineEndpoint(env); err != nil {
		log.WarningLog.Printf("staging dir sweep: cannot resolve the Docker engine endpoint; skipping this sweep: %v", err)
		result.Unlisted = true
		return result
	}
	// Pin ONE endpoint for the engine-ID read, the listing, and the ID
	// re-read (#7616 R2): without an explicit selector each docker call
	// re-resolves the current context, so a `docker context use` switch
	// landing between the two ID reads could put the LISTING on a third
	// engine both reads never saw. Materializing the resolved selector into
	// the env makes the whole pass address one endpoint; a changed engine
	// behind it is still caught by the before/after ID comparison.
	pin, err := resolveDockerContextPin(env)
	if err != nil {
		log.WarningLog.Printf("staging dir sweep: cannot pin the Docker engine selector; skipping this sweep: %v", err)
		result.Unlisted = true
		return result
	}
	if pin != "" {
		env = append(env, "DOCKER_CONTEXT="+pin)
	}
	ctx, cancel := context.WithTimeout(context.Background(), dockerShortStepTimeout)
	engineID, err := currentDockerEngineID(ctx, env)
	cancel()
	if err != nil {
		log.WarningLog.Printf("staging dir sweep: cannot read the Docker engine identity; skipping this sweep: %v", err)
		result.Unlisted = true
		return result
	}
	result.EngineID = engineID
	containers, err := listAfContainers(env, homeID)
	if err != nil {
		log.WarningLog.Printf("staging dir sweep: cannot list af containers; skipping this sweep: %v", err)
		result.Unlisted = true
		return result
	}
	containerSlugs := make(map[string]bool, len(containers))
	for _, c := range containers {
		containerSlugs[c.slug] = true
	}
	// The listing is only the proof while the engine it came from is still the
	// engine being asked — the post-pass re-read reapListedContainers applies
	// (Codex on #7458). A moved or unreadable engine voids the pass.
	ctx, cancel = context.WithTimeout(context.Background(), dockerShortStepTimeout)
	listedOn, err := currentDockerEngineID(ctx, env)
	cancel()
	if err != nil || listedOn != engineID {
		log.WarningLog.Printf("staging dir sweep: the Docker engine changed or could not be read again during this pass (was %q, now %q: %v); it proves nothing about what is left", engineID, listedOn, err)
		result.Unlisted = true
		result.EngineID = ""
		return result
	}

	for _, c := range candidates {
		switch {
		case claimedNames[c.name]:
			result.Skipped++
		case protectedSlugs[c.slug]:
			result.Skipped++
		case !stagingIdentityMatches(root, c.name, engineID, homeID):
			// Unrecorded, malformed, different engine, or a home-label
			// spelling this listing's filter cannot see: the listing
			// says nothing about whether a container still binds this
			// dir on the engine that created it — keep it.
			result.Skipped++
		case containerSlugs[c.slug]:
			result.Skipped++
		default:
			switch err := removeAllWithinRoot(root, c.name); {
			case err == nil:
				result.Reaped++
				log.InfoLog.Printf("staging dir sweep: removed orphaned staging dir %s — no live session record names it and no container on this engine binds its slug", filepath.Join(rootPath, c.name))
			case errors.Is(err, fs.ErrNotExist):
				// Its own teardown won the race: the keep proofs were
				// already applied, so the goal state is reached quietly.
				result.Skipped++
			default:
				result.Errors++
				log.WarningLog.Printf("staging dir sweep: could not remove %s: %v", filepath.Join(rootPath, c.name), err)
			}
		}
	}
	if result.Reaped > 0 || result.Errors > 0 {
		log.InfoLog.Printf("staging dir sweep: listed=%d reaped=%d skipped=%d errors=%d",
			result.Listed, result.Reaped, result.Skipped, result.Errors)
	}
	return result
}

// stagingMarkerMax bounds each recorded identity marker: the engine token,
// the home-label spelling, and the generation dir name are all small
// single-line values. A marker file bigger than the bound — or anything but
// a regular file — is not a marker provisioning wrote, and is never opened:
// the Lstat gate precedes every read so the pass cannot block on a special
// file (#7616 R3).
const stagingMarkerMax = 4096

// readStagingMarker reads one marker file through the verified staging root,
// returning its bytes only when the entry is a regular file of bounded size.
// Anything else — a missing marker, a directory, a symlink, a special file,
// an oversized one — is an error the caller reads as unrecorded-and-kept.
func readStagingMarker(root *os.Root, name, marker string) ([]byte, error) {
	st, err := root.Lstat(filepath.Join(name, marker))
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("marker %s is not a regular file", marker)
	}
	if st.Size() > stagingMarkerMax {
		return nil, fmt.Errorf("marker %s is too large (%d bytes)", marker, st.Size())
	}
	return root.ReadFile(filepath.Join(name, marker))
}

// stagingGenerationAlive reports whether the daemon generation the marker
// names — a <home>/tunnels/d<pid>-<rand> dir — is still alive: its .lock
// held by any process. The lock dies with the holder, which pid reuse cannot
// fake, so this is a proof, not a guess (the same primitive
// sweepStaleTunnelSocketDirs uses, #7495):
//
//   - lock HELD (flock refused): a live af process — this daemon, or a
//     predecessor mid-upgrade — owns the generation. Keep the dir.
//   - lock FREE (flock taken, then released): the generation is provably
//     dead. Its staging dirs are the orphans this pass exists to reclaim.
//   - generation dir ABSENT (ENOENT): also dead — the only sanctioned
//     remover of a generation dir is the lock-gated generation sweep, which
//     proves the lock free first. A manual `rm -rf tunnels/` under a live
//     daemon is an operator deleting daemon state, the same exposure class
//     as deleting the staging dirs themselves; the next mint re-establishes
//     the entry.
//   - anything else — no .lock, an unreadable path, a non-directory entry
//     at the registry or generation name — is unproven and keeps. ENOTDIR is
//     deliberately NOT treated as absent: a planted file at the name is
//     tampering the pass cannot disprove.
func stagingGenerationAlive(gen string) (alive bool, why string) {
	home, err := currentTunnelHome()
	if err != nil {
		return false, "cannot resolve the AF home: " + err.Error()
	}
	homeFd, err := unix.Open(home, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, "cannot open the AF home: " + err.Error()
	}
	defer func() { _ = unix.Close(homeFd) }()
	tunnelsFd, err := unix.Openat(homeFd, tunnelSocketRootName,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, "" // no generation registry at all — nothing named here can be alive
		}
		return false, "cannot open the generation registry: " + err.Error()
	}
	defer func() { _ = unix.Close(tunnelsFd) }()
	genFd, err := unix.Openat(tunnelsFd, gen,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, "" // the generation dir is gone — only ever removed after its lock provably freed
		}
		return false, "cannot open the recorded generation dir: " + err.Error()
	}
	defer func() { _ = unix.Close(genFd) }()
	// Prove .lock is a regular file BEFORE opening it: an open of a special
	// file can block, and this probe must never block the pass (#7616 R3).
	var st unix.Stat_t
	if err := unix.Fstatat(genFd, tunnelGenLockName, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false, "cannot stat the recorded generation's lock: " + err.Error()
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return false, "the recorded generation's lock is not a regular file"
	}
	lockFd, err := unix.Openat(genFd, tunnelGenLockName, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, "cannot open the recorded generation's lock: " + err.Error()
	}
	defer func() { _ = unix.Close(lockFd) }()
	if err := unix.Flock(lockFd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return true, ""
		}
		return false, "cannot probe the recorded generation's lock: " + err.Error()
	}
	_ = unix.Flock(lockFd, unix.LOCK_UN)
	return false, ""
}

// dockerContextNameFormat selects a context's own name from `docker context
// inspect` — the selector the pin materializes so every later call addresses
// the context the resolution chose, immune to a `docker context use` switch
// landing mid-pass (#7616 R2).
const dockerContextNameFormat = "{{.Name}}"

// resolveDockerContextPin reports the context name that pins the docker
// CLI's selector: an empty string when the env already carries a selector
// (DOCKER_HOST or DOCKER_CONTEXT — both outrank a `docker context use`
// switch, so the calls are pinned already), else the current context's own
// name, read once through `docker context inspect`. Appending the answer as
// DOCKER_CONTEXT=<name> makes every later call address the context the
// resolution chose: the CLI treats the env var exactly like `--context`,
// and a `docker context use` landing afterwards can no longer move the
// calls to an engine the resolution never saw. Both the sweep's
// ID-read/listing/re-read sequence (#7616 R2) and the provision's
// ID-read/run sequence (Codex on #7616) pin their calls this way — through
// the environment rather than argv, since DockerCLIEnvironment already
// forwards DOCKER_CONTEXT.
func resolveDockerContextPin(env []string) (string, error) {
	if rawEnvironmentValue(env, "DOCKER_HOST") != "" || rawEnvironmentValue(env, "DOCKER_CONTEXT") != "" {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), dockerShortStepTimeout)
	out, err := dockerExec(ctx, env, "context", "inspect", "--format", dockerContextNameFormat)
	cancel()
	if err != nil {
		return "", fmt.Errorf("`docker context inspect` could not name the resolved context: %w", err)
	}
	name := strings.TrimSpace(string(out))
	if name == "" || strings.ContainsAny(name, "\n\r\x00") {
		return "", fmt.Errorf("`docker context inspect` returned a malformed context name %q", name)
	}
	return name, nil
}

// stagingIdentityMatches reports whether the dir's recorded creation
// identity — the markers recordStagingIdentity wrote — clears candidacy for
// this pass: the recorded daemon generation is provably dead, AND the
// recorded engine is the one this pass listed, AND the recorded home-label
// spelling is the one its container filter used. Only then do the listing
// and the keep sets prove anything about this dir: no marker (a legacy dir,
// or a provision that staged before the identity resolved), a malformed
// one, a live generation, a different engine, or a different af.home
// spelling each mean a container binding it could live where this listing
// cannot see — a dir THIS generation is mid-provisioning is exactly such a
// case (#7616 R1), and a symlinked AGENT_FACTORY_HOME reaches the same real
// dir under a spelling the label filter does not match (Codex on #7616).
// Every non-match logs why the dir is kept.
func stagingIdentityMatches(root *os.Root, name, engineID, homeID string) bool {
	rawGen, err := readStagingMarker(root, name, dockerStagingGenerationFileName)
	if err != nil {
		log.InfoLog.Printf("staging dir sweep: keeping %s: no recorded generation — a dir this daemon may still be provisioning is indistinguishable from an orphan (%v)", name, err)
		return false
	}
	gen := strings.TrimSuffix(string(rawGen), "\n")
	if !tunnelGenDirRE.MatchString(gen) {
		log.InfoLog.Printf("staging dir sweep: keeping %s: malformed recorded generation %q", name, gen)
		return false
	}
	alive, why := stagingGenerationAlive(gen)
	switch {
	case alive:
		log.InfoLog.Printf("staging dir sweep: keeping %s: minted by generation %s, whose lock a live af process still holds — its container may not exist yet", name, gen)
		return false
	case why != "":
		log.InfoLog.Printf("staging dir sweep: keeping %s: cannot prove the recorded generation %s is gone (%s)", name, gen, why)
		return false
	}
	raw, err := readStagingMarker(root, name, dockerStagingEngineFileName)
	if err != nil {
		log.InfoLog.Printf("staging dir sweep: keeping %s: no recorded engine — a container binding it could exist where this listing cannot see (%v)", name, err)
		return false
	}
	recorded := strings.TrimSpace(string(raw))
	// The same shape currentDockerEngineID enforces on a live answer: one
	// field, bounded length. A torn or tampered marker fails here and keeps.
	if recorded == "" || len(recorded) > 256 || len(strings.Fields(recorded)) != 1 {
		log.InfoLog.Printf("staging dir sweep: keeping %s: malformed recorded engine %q", name, recorded)
		return false
	}
	if recorded != engineID {
		log.InfoLog.Printf("staging dir sweep: keeping %s: created on engine %q, not the listed engine %q — a container binding it could still exist there", name, recorded, engineID)
		return false
	}
	rawHome, err := readStagingMarker(root, name, dockerStagingHomeFileName)
	if err != nil {
		log.InfoLog.Printf("staging dir sweep: keeping %s: no recorded home label — a container binding it could carry an af.home spelling this filter misses (%v)", name, err)
		return false
	}
	recordedHome := string(rawHome)
	// The marker format is the label spelling plus one trailing newline;
	// the label itself is matched lexically, so spaces are legal but a
	// newline or NUL inside the file is not a value provisioning wrote.
	recordedHome = strings.TrimSuffix(recordedHome, "\n")
	if recordedHome == "" || len(recordedHome) > 4096 || strings.ContainsAny(recordedHome, "\n\r\x00") {
		log.InfoLog.Printf("staging dir sweep: keeping %s: malformed recorded home label %q", name, recordedHome)
		return false
	}
	if recordedHome != homeID {
		log.InfoLog.Printf("staging dir sweep: keeping %s: created under af.home %q, not the listed home %q — a container binding it could exist under a spelling the filter misses", name, recordedHome, homeID)
		return false
	}
	return true
}
