package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
)

// OrphanSweepResult summarizes one orphan-container sweep (#2194).
type OrphanSweepResult struct {
	Listed  int // af containers on this engine+home the sweep saw
	Reaped  int // orphans removed
	Skipped int // containers spared as a live or mid-create session
	Unknown int // orphans left for a later sweep (state unknown)
	Errors  int // orphans docker refused to remove for a definite reason
	// Unlisted is set when the pass could not list this engine's containers
	// at all — no docker CLI, no endpoint, no engine identity, or a failed
	// listing — so it proved nothing about what is still there (#7447). A
	// docker CLI missing now does not prove that a daemon which ran earlier,
	// with one, created no container.
	Unlisted bool
	// EngineID is the identity of the Docker engine the pass listed, empty
	// when it listed none. A whole-home reap checks it against the engines the
	// home recorded (#7447): an empty listing on another engine proves
	// nothing about that home's containers.
	EngineID string
}

// afContainer is one listed managed container: its id, its af.session slug,
// and, when the listing asked for it, its af.home label.
type afContainer struct {
	id   string
	slug string
	home string
}

// dockerSweepListFormat prints "<id>\t<af.session slug>" per container so the
// sweep can decide ownership from the slug without a second inspect call.
const dockerSweepListFormat = "{{.ID}}\t{{.Label \"" + dockerSessionLabel + "\"}}"

// SweepOrphanContainers removes docker containers this daemon leaked — a session
// container that outlived its session because the daemon died without reaping, the
// session record is gone, or a reap raced a create. Nothing else ever cleans
// these up, so they hold memory, disk, and a published port on the box forever.
//
// It is safe by construction — the hard part of reaping here is not destroying a
// workload you do not own:
//
//   - It lists ONLY containers labelled af.session AND af.home=<homeID> (this
//     daemon's home). A container with no af.home label — one a pre-upgrade af
//     created, or one from another af home — never matches, so an unlabelled
//     container is treated as NOT ours and left alone, never as ours.
//   - `docker ps` runs under the daemon's own docker environment, so the query is
//     scoped to the currently-targeted engine; the sweep never sees, and so can
//     never reap, a container on another engine (the #2382 cross-engine hazard).
//     The environment is first resolved through the same selector check every
//     other docker caller uses: when DOCKER_HOST and DOCKER_CONTEXT disagree the
//     sweep is skipped rather than run on the engine the CLI happened to pick.
//   - A listed container whose af.session slug is in protectedSlugs — the slug of
//     a live OR still-provisioning session (the #2549 mid-create window) — is
//     spared. The label is a many-to-one title slug, so this errs toward sparing:
//     a genuinely orphaned same-slug container is left for a later sweep once the
//     colliding session ends, which is the correct default for a destructive pass.
//   - Each orphan is removed through the SAME reap the per-session Kill uses
//     (dockerProvisioner.reap with verifyEngineOnReap), inheriting its
//     engine-identity guard, its three-valued outcome (an ErrWorkspaceStateUnknown
//     result leaves the container for the next sweep rather than claiming it gone),
//     and its bounded exec — no raw `docker rm -f`, no unbounded wait.
//
// homeID must equal the value runContainer stamps into af.home
// (config.GetConfigDir()). An empty homeID disables the sweep — there is nothing
// to scope to safely, and a broad sweep is exactly what must never happen.
func SweepOrphanContainers(homeID string, protectedSlugs map[string]bool) OrphanSweepResult {
	if strings.TrimSpace(homeID) == "" {
		return OrphanSweepResult{}
	}
	return reapListedContainers(containerReap{
		logPrefix: "orphan sweep",
		list:      func(env []string) ([]afContainer, error) { return listAfContainers(env, homeID) },
		spare:     func(c afContainer) bool { return protectedSlugs[c.slug] },
		reapedWhy: "no live session owns it",
	})
}

// ReapHomeContainers removes every af session container whose af.home label
// isHome accepts, whatever its session (#7447). It is for tearing down a whole
// home — an expired throwaway sandbox — that no daemon will start in again, so
// SweepOrphanContainers, scoped to the home of the daemon that runs it, never
// would.
//
// The proof is per container. The listing reads each container's af.session
// slug and its af.home label, and only a container whose label isHome proves
// is a candidate: an unlabelled container, or one another home created, never
// is, and nothing is chosen by name. The label is the raw home path the
// creating daemon resolved, so the caller proves it by what the path names,
// not by its spelling. Each removal then goes through the same per-session
// reap the sweep uses, re-verifying the engine identity before the rm.
func ReapHomeContainers(isHome func(labelHome string) bool) OrphanSweepResult {
	return reapListedContainers(containerReap{
		logPrefix: "home container reap",
		list: func(env []string) ([]afContainer, error) {
			all, err := listLabelledAfContainers(env)
			if err != nil {
				return nil, err
			}
			var proven []afContainer
			for _, c := range all {
				if isHome(c.home) {
					proven = append(proven, c)
				}
			}
			return proven, nil
		},
		spare:     func(afContainer) bool { return false },
		reapedWhy: "its home is being torn down",
	})
}

// containerReap is one pass of reapListedContainers: what it lists, what it
// spares, and how it names itself in the log.
type containerReap struct {
	logPrefix string
	list      func(env []string) ([]afContainer, error)
	spare     func(afContainer) bool
	reapedWhy string
}

// reapListedContainers resolves the engine the way every docker caller does,
// lists the pass's candidates on it, and removes each one it does not spare
// through the per-session reap.
func reapListedContainers(pass containerReap) OrphanSweepResult {
	var result OrphanSweepResult
	if _, err := lookPath("docker"); err != nil {
		// No docker CLI: nothing to do now, and nothing proven about what an
		// earlier daemon may have left (Codex on #7448).
		result.Unlisted = true
		return result
	}

	env := sessionenv.DockerCLIEnvironment(os.Environ(), "", nil)

	// The selector refusal every other docker caller already inherits through
	// resolveDockerEngineEndpoint: with DOCKER_HOST and DOCKER_CONTEXT both set
	// and disagreeing, the CLI dials DOCKER_HOST while its own reference says
	// DOCKER_CONTEXT wins — and the sweep would then feed `docker rm -f` a list
	// read from whichever engine that guess picked. The same refusal applies
	// here before the sweep's first docker call (#4413 review).
	if _, _, err := resolveDockerEngineEndpoint(env); err != nil {
		log.WarningLog.Printf("%s: cannot resolve the Docker engine endpoint; skipping this sweep: %v", pass.logPrefix, err)
		result.Unlisted = true
		return result
	}

	ctx, cancel := context.WithTimeout(context.Background(), dockerShortStepTimeout)
	engineID, err := currentDockerEngineID(ctx, env)
	cancel()
	if err != nil {
		log.WarningLog.Printf("%s: cannot read the Docker engine identity; skipping this sweep: %v", pass.logPrefix, err)
		result.Unlisted = true
		return result
	}
	result.EngineID = engineID

	containers, err := pass.list(env)
	if err != nil {
		log.WarningLog.Printf("%s: cannot list af containers; skipping this sweep: %v", pass.logPrefix, err)
		result.Unlisted = true
		return result
	}

	for _, c := range containers {
		result.Listed++
		if pass.spare(c) {
			result.Skipped++
			continue
		}
		// Reuse the per-session reap verbatim: it re-verifies the engine identity
		// before the destructive rm, so building it with the engine we just read is
		// safe even if the daemon's docker context changes under us.
		p := &dockerProvisioner{
			spec:               ProvisionSpec{Title: c.slug},
			containerID:        c.id,
			engineID:           engineID,
			verifyEngineOnReap: true,
		}
		switch err := p.reap(); {
		case err == nil:
			result.Reaped++
			log.InfoLog.Printf("%s: reaped leaked container %s (af.session=%q) — %s", pass.logPrefix, shortContainerID(c.id), c.slug, pass.reapedWhy)
		case errors.Is(err, ErrWorkspaceStateUnknown):
			result.Unknown++
			log.WarningLog.Printf("%s: leaving container %s for a later sweep (state unknown): %v", pass.logPrefix, shortContainerID(c.id), err)
		default:
			result.Errors++
			log.WarningLog.Printf("%s: could not reap container %s: %v", pass.logPrefix, shortContainerID(c.id), err)
		}
	}
	if result.Reaped > 0 || result.Unknown > 0 || result.Errors > 0 {
		log.InfoLog.Printf("%s: listed=%d reaped=%d skipped=%d unknown=%d errors=%d",
			pass.logPrefix, result.Listed, result.Reaped, result.Skipped, result.Unknown, result.Errors)
	}
	return result
}

// listAfContainers lists this home's managed containers (any state) with their
// af.session slug. The two label filters run engine-scoped under env.
func listAfContainers(env []string, homeID string) ([]afContainer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dockerShortStepTimeout)
	defer cancel()
	out, err := dockerExec(ctx, env,
		"ps", "-a",
		"--filter", "label="+dockerSessionLabel,
		"--filter", "label="+dockerHomeLabel+"="+homeID,
		"--format", dockerSweepListFormat,
	)
	if err != nil {
		return nil, err
	}
	var containers []afContainer
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		id, slug, _ := strings.Cut(line, "\t")
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		containers = append(containers, afContainer{id: id, slug: strings.TrimSpace(slug)})
	}
	return containers, nil
}

// dockerHomeReapListFormat prints, per container, its id, then its af.session
// slug and its af.home label each as a JSON string. JSON keeps a label's bytes
// exactly — a trailing space, a tab or a newline is part of a distinct home's
// path, and a whole-home reap must never trim one home's label into another's
// (#7447) — and keeps a newline inside a label from splitting the line.
const dockerHomeReapListFormat = "{{.ID}}\t{{json (.Label \"" + dockerSessionLabel + "\")}}\t{{json (.Label \"" + dockerHomeLabel + "\")}}"

// listLabelledAfContainers lists every managed container that carries an
// af.home label, with its slug and that label verbatim, on the engine env
// selects. Which of them belong to a home is the caller's proof, made per
// container.
func listLabelledAfContainers(env []string) ([]afContainer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dockerShortStepTimeout)
	defer cancel()
	out, err := dockerExec(ctx, env,
		"ps", "-a",
		"--filter", "label="+dockerSessionLabel,
		"--filter", "label="+dockerHomeLabel,
		"--format", dockerHomeReapListFormat,
	)
	if err != nil {
		return nil, err
	}
	var containers []afContainer
	// The label is never trimmed: only the line terminator is removed.
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			continue // not a line this format prints; never guess at one
		}
		id := strings.TrimSpace(fields[0])
		var slug, home string
		if id == "" || json.Unmarshal([]byte(fields[1]), &slug) != nil || json.Unmarshal([]byte(fields[2]), &home) != nil || home == "" {
			continue
		}
		containers = append(containers, afContainer{id: id, slug: slug, home: home})
	}
	return containers, nil
}

// shortContainerID trims a container id to its short form for logs.
func shortContainerID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
