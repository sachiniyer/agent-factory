package session

import (
	"context"
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
}

// afContainer is one listed managed container: its id and its af.session slug.
type afContainer struct {
	id   string
	slug string
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
	var result OrphanSweepResult
	if strings.TrimSpace(homeID) == "" {
		return result
	}
	if _, err := lookPath("docker"); err != nil {
		return result // no docker CLI → nothing this daemon could have created
	}

	env := sessionenv.DockerCLIEnvironment(os.Environ(), "", nil)

	ctx, cancel := context.WithTimeout(context.Background(), dockerShortStepTimeout)
	engineID, err := currentDockerEngineID(ctx, env)
	cancel()
	if err != nil {
		log.WarningLog.Printf("orphan sweep: cannot read the Docker engine identity; skipping this sweep: %v", err)
		return result
	}

	containers, err := listAfContainers(env, homeID)
	if err != nil {
		log.WarningLog.Printf("orphan sweep: cannot list af containers; skipping this sweep: %v", err)
		return result
	}

	for _, c := range containers {
		result.Listed++
		if protectedSlugs[c.slug] {
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
			log.InfoLog.Printf("orphan sweep: reaped leaked container %s (af.session=%q) — no live session owns it", shortContainerID(c.id), c.slug)
		case errors.Is(err, ErrWorkspaceStateUnknown):
			result.Unknown++
			log.WarningLog.Printf("orphan sweep: leaving container %s for a later sweep (state unknown): %v", shortContainerID(c.id), err)
		default:
			result.Errors++
			log.WarningLog.Printf("orphan sweep: could not reap container %s: %v", shortContainerID(c.id), err)
		}
	}
	if result.Reaped > 0 || result.Unknown > 0 || result.Errors > 0 {
		log.InfoLog.Printf("orphan sweep: listed=%d reaped=%d skipped=%d unknown=%d errors=%d",
			result.Listed, result.Reaped, result.Skipped, result.Unknown, result.Errors)
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

// shortContainerID trims a container id to its short form for logs.
func shortContainerID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
