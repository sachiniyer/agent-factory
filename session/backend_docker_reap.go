// The container-DESTRUCTION half of the docker runtime (#1592 Phase 4 PR4):
// the memoized reaper, the engine-identity guard that keeps `docker rm -f`
// aimed at the engine that created the container, and the cleanup a failed
// provision runs through them. backend_docker.go owns creation (Provision
// through publishedPort); nothing here starts a container — everything here
// answers "is it gone, and may the record go too" (#2049/#2063/#2382/#2590).
package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// dockerReapTimeout bounds the `docker rm -f` container reap. A var (not a const)
// only so tests can shorten it to exercise the deadline path; production never
// reassigns it. Mirrors networkGitTimeout / tmuxCommandTimeout.
var dockerReapTimeout = 30 * time.Second

// dockerEngineIDFormat selects the daemon's stable, non-secret identity from
// `docker info`. Cleanup tombstones persist this value so a daemon restart
// cannot redirect `docker rm -f` to whichever engine the client happens to
// target later (#2382).
const dockerEngineIDFormat = "{{.ID}}"

// reapProvisionFailure cleans up a container created by a failed provision. No
// Instance record exists to carry a future retry, so a reap that does not succeed
// must remain visible to the caller with both causes and the orphan risk (#2590).
func (p *dockerProvisioner) reapProvisionFailure(provisionErr error) error {
	if p.containerID == "" {
		return provisionErr
	}
	reapErr := p.reap()
	if reapErr == nil {
		return provisionErr
	}
	return fmt.Errorf("backend=docker: provisioning failed and cleanup of container %s for session %q did not complete; a container may still be running; inspect it before retrying: %w",
		p.shortID(), p.spec.Title, errors.Join(provisionErr, reapErr))
}

// reap removes the container. It runs on the session's Kill (after the in-container
// workspace is torn down over REST), on a provisioning failure, and on a
// bad-endpoint NewInstance failure — so a container is never leaked.
//
// It is memoized but only for a reap that COMPLETED: a success, or a failure docker
// ANSWERED with, latches (reaped=true, outcome in reapErr) so the repeated Kill
// retries and the Kill-vs-provision-failure race collapse to that one result.
//
// A TIMEOUT deliberately does NOT latch. The `docker rm -f` was SIGKILLed mid-reap,
// so it did not complete and the container may STILL be running; the daemon's
// kill-retry (finishUserKill, on every poll for the retained record) must therefore
// be able to actually re-run `docker rm -f` rather than see a stale nil. This is the
// #2063-review defect the naive sync.Once had: it latched on the timeout too, so the
// second reap() skipped the closure and returned nil (reapErr was a per-call local),
// the row was deleted, and the container was orphaned exactly one poll later —
// retention that lasted a single tick. Not latching a timeout makes retention
// durable AND the reap genuinely retry-able.
//
// While a reap keeps timing out it keeps returning the ErrWorkspaceStateUnknown
// sentinel (row retained); once docker answers — a later `docker rm -f` succeeds or
// reports (e.g. "No such container", already gone) — it latches and the row may go.
// It never silently returns nil while the container may still be leaked.
func (p *dockerProvisioner) reap() error {
	// Held across the docker call so concurrent reaps serialize (no double
	// `docker rm -f`) and the latch is read/written race-free — the same
	// mutual-exclusion the sync.Once gave, minus its permanent latch.
	p.reapMu.Lock()
	defer p.reapMu.Unlock()
	if p.reaped {
		return p.reapErr
	}
	if p.containerID == "" {
		p.reaped = true
		return nil
	}
	// Own the context here (rather than via p.docker) so a tripped deadline is
	// distinguishable from a reap that docker ANSWERED with an error — the two mean
	// opposite things for the record (#2049).
	ctx, cancel := context.WithTimeout(context.Background(), dockerReapTimeout)
	defer cancel()
	if p.verifyEngineOnReap {
		if strings.TrimSpace(p.engineID) == "" {
			reapErr := fmt.Errorf("%w: backend=docker: refusing to reap container %s for session %q because its cleanup handle predates Docker engine identity tracking; inspect the original Docker engine and remove the container manually",
				ErrWorkspaceStateUnknown, p.shortID(), p.spec.Title)
			log.WarningLog.Printf("%v", reapErr)
			return reapErr
		}
		currentEngineID, err := currentDockerEngineID(ctx, p.dockerEnvironment())
		if err != nil {
			reapErr := fmt.Errorf("%w: backend=docker: cannot verify the Docker engine before reaping container %s for session %q; restore Docker access and retry: %v",
				ErrWorkspaceStateUnknown, p.shortID(), p.spec.Title, err)
			log.WarningLog.Printf("%v", reapErr)
			return reapErr
		}
		if currentEngineID != p.engineID {
			reapErr := fmt.Errorf("%w: backend=docker: refusing to reap container %s for session %q because the current Docker engine is %q, but the cleanup handle belongs to %q; select the original Docker context or DOCKER_HOST and retry",
				ErrWorkspaceStateUnknown, p.shortID(), p.spec.Title, currentEngineID, p.engineID)
			log.WarningLog.Printf("%v", reapErr)
			return reapErr
		}
	}
	out, err := dockerExec(ctx, p.dockerEnvironment(), "rm", "-f", p.containerID)
	if err == nil {
		p.reaped = true
		p.reapErr = nil
		log.InfoLog.Printf("docker runtime: reaped container %s for session %q", p.shortID(), p.spec.Title)
		return nil
	}
	reapErr := fmt.Errorf("backend=docker: `docker rm -f %s` failed: %s: %w", p.shortID(), strings.TrimSpace(string(out)), err)
	// The #914 guard: classify a TIMEOUT only when err != nil AND the ctx deadline
	// tripped, so a bare exec.ErrWaitDelay that dockerExec already normalized to
	// success (err == nil) never reaches here.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// Unknown-state, and NOT latched: RETAIN the row now and re-run on the next
		// reap. exec.CommandContext reports the killed process, not the context
		// sentinel, so join ctx.Err explicitly and keep the deadline classifiable by
		// callers (#2049 classification + #2063 review + #2590 review).
		reapErr = fmt.Errorf("%w: %w", ErrWorkspaceStateUnknown, errors.Join(reapErr, ctx.Err()))
		log.WarningLog.Printf("%v", reapErr)
		return reapErr
	}
	// docker ANSWERED with an error: the reap completed and TOLD us something, so it
	// latches as a KNOWN-state error and the row may be deleted, per the documented
	// deleteSessionRecord contract. Latching also stops retries from re-running a
	// command that will keep answering the same way.
	p.reaped = true
	p.reapErr = reapErr
	log.WarningLog.Printf("%v", reapErr)
	return reapErr
}

// currentEngineID returns the daemon identity that the provisioner's Docker
// client currently targets. New containers record it before `docker run`, and
// restored reapers prove the same identity again before issuing the destructive
// command. It deliberately stores no endpoint, context path, or credential.
func (p *dockerProvisioner) currentEngineID(timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return currentDockerEngineID(ctx, p.dockerEnvironment())
}

// currentDockerEngineID takes the environment explicitly for the same reason
// every other docker invocation here does: the CLI is spawned with a filtered
// environment, never the host's, so an unlisted credential cannot reach it.
// `docker info` is no exception — it is a docker CLI call like any other, and
// leaving it on the ambient environment would be a hole in the boundary that
// happens to be on the reap path rather than the run path.
func currentDockerEngineID(ctx context.Context, environ []string) (string, error) {
	out, err := dockerExec(ctx, environ, "info", "--format", dockerEngineIDFormat)
	if err != nil {
		return "", fmt.Errorf("`docker info` could not report the engine identity: %w", err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", errors.New("`docker info` returned an empty engine identity")
	}
	if len(id) > 256 || len(strings.Fields(id)) != 1 {
		return "", errors.New("`docker info` returned a malformed engine identity")
	}
	return id, nil
}
