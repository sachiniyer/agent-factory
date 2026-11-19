// The orphaned-staging-dir sweep (#7590): <AF home>/docker-staging/<slug>-<hex>
// dirs that no live session record names, reclaimed once the container that
// could bind them is proven gone. Provisioning stages the payload BEFORE the
// first persisted record exists, so a daemon killed in that window leaves a
// directory (about 49 MB — the af binary plus the policy) that no cleanup
// handle ever reaches.
//
// The pass only ever removes what four proofs leave over:
//
//   - A dir whose base name a live session record still claims (the record's
//     StagingDir field) is kept — the teardown handle owns it.
//   - A dir whose slug belongs to a live or still-provisioning session is
//     kept — the mid-create window has a staged dir with no record yet, and
//     `docker run` may not have bound it yet either.
//   - A dir is removed only on the engine that created it: provisioning
//     records the resolved `docker info` ID beside the staged payload, and a
//     candidate whose marker is missing, malformed, or names a different
//     engine is kept — a container binding it could live where this engine's
//     listing cannot see it. An unreachable engine proves nothing either:
//     the whole pass is Unlisted and removes nothing.
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
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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
// instance snapshot it protects the container sweep with — under createSweepMu
// while its skipped-repos set is drained, so neither set is built on a
// known-incomplete view.
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
		case !stagingEngineMatches(root, c.name, engineID):
			// Unrecorded, malformed, or different-engine: the current
			// listing says nothing about whether a container still binds
			// this dir on the engine that created it — keep it.
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

// stagingEngineMatches reports whether the dir's recorded engine identity —
// the marker recordStagingEngine wrote at creation — names the engine this
// pass listed. Only then does the listing prove anything about this dir: no
// marker (a legacy dir, or a provision that staged before the engine
// resolved), a malformed one, or a different engine each mean a container
// binding it could live where this listing cannot see. Every non-match logs
// why the dir is kept.
func stagingEngineMatches(root *os.Root, name, engineID string) bool {
	raw, err := root.ReadFile(filepath.Join(name, dockerStagingEngineFileName))
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
	return true
}
