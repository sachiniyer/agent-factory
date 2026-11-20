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
	"os"
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
// reap() runs even when `docker run` refused before naming a container — the
// staged payload dir still needs clearing, or every failed provision leaks one
// full copy of the af binary.
func (p *dockerProvisioner) reapProvisionFailure(provisionErr error) error {
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
		// No container was ever created — the staged payload dir's only
		// consumer cannot exist, so it is safe to clear. Every other path
		// clears it only after `docker rm` succeeds (#7577 item 2 R5): while
		// the container's fate is unproven — an engine check refusal, a
		// timeout, an answered error — the directory stays so the cleanup
		// record's StagingDir still names a real directory a later reap owns.
		p.clearStagingDir()
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
		// The container is confirmed gone — now the staged payload dir may go
		// too. Clearing it only here keeps the ordering: never before `docker
		// rm` has succeeded (and never ahead of the engine check above).
		p.clearStagingDir()
		p.reaped = true
		p.reapErr = nil
		log.InfoLog.Printf("docker runtime: reaped container %s for session %q", p.shortID(), p.spec.Title)
		return nil
	}
	reapErr := fmt.Errorf("backend=docker: `docker rm -f %s` failed: %s: %w", p.shortID(), strings.TrimSpace(string(out)), err)
	if strings.Contains(string(out), "No such container") {
		// An answered "No such container" proves the container gone — the same
		// certainty a successful rm gives — so the staging dir releases too.
		p.clearStagingDir()
	}
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
	// The "answer" was a TRANSPORT FAILURE — the daemon was unreachable (down or
	// restarting, the socket/endpoint was refused, access was denied, or a pinned
	// DOCKER_CONTEXT could not be resolved) — so the `docker rm -f` never reached an
	// engine and the container's fate is unknown. This is the same state-unknown
	// class as the deadline above: do NOT latch, so the daemon's kill-retry re-runs
	// the rm once Docker access is restored instead of orphaning a container behind
	// a deleted row. Without this guard a dockerd blip during a Kill would latch as
	// a known-state teardown, the row would be deleted, and the (still-running)
	// container would outlive its only in-process handle (#2063-review omission).
	//
	// "No such container" is excluded because it is the OPPOSITE case — an answered
	// error that PROVES the container gone (handled above for the staging dir, and
	// falling through to the known-state latch below). A real daemon answer — "Error
	// response from daemon: …" — does not match and still latches known-state.
	if !strings.Contains(string(out), "No such container") && dockerAnsweredTransportFailure(out) {
		reapErr = fmt.Errorf("%w: %w", ErrWorkspaceStateUnknown, reapErr)
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

// dockerAnsweredTransportFailure reports whether out is a docker CLI failure that
// proves a `docker rm -f` never reached the daemon and so never acted on the
// container. The daemon was unreachable (down or restarting, the endpoint was
// refused, or DOCKER_HOST named a bad host), access to its socket/endpoint was
// denied, or the pinned DOCKER_CONTEXT could not be resolved.
//
// The CLI still exits non-zero WITHOUT tripping the reap's deadline for any of
// these, so — exactly like a timeout, and unlike a real answer — the container's
// fate is unknown: it must NOT latch, and the reap must re-run once Docker access
// is restored. Mapping a transport failure onto the known-state latch (the
// #2063-review answer taxonomy's lone omission) deleted the row while the
// container lived and left no in-process handle to retry it.
//
// This is the counterpart of the in-reap "No such container" match: that is the
// ONE answered error that PROVES the container gone (so the staging dir releases
// and the outcome latches known-state); this is an answer that proves the rm
// never ran. A genuine daemon response — "Error response from daemon: …" —
// matches neither and still latches as a known-state teardown per the
// deleteSessionRecord contract. Callers exclude "No such container" themselves
// so the two opposite classes stay disjoint.
func dockerAnsweredTransportFailure(out []byte) bool {
	s := string(out)
	// The CLI's canonical "daemon unreachable" banner, emitted for a down or
	// restarting dockerd, a refused/bad DOCKER_HOST, or a TLS mismatch — every
	// pre-29 form opens with "Cannot connect to the Docker daemon".
	if strings.Contains(s, "Cannot connect to the Docker daemon") {
		return true
	}
	// Docker 29+ reworded the same client-side connection failure
	// (moby/moby@67596f0, surfacing as moby/moby#52641): "failed to connect to
	// the docker API at unix:///var/run/docker.sock; check if the path is
	// correct and if the daemon is running: …". It is the same transport
	// failure — the rm never reached an engine — under the new spelling.
	if strings.Contains(s, "failed to connect to the docker API") {
		return true
	}
	// A socket/endpoint permission denial, e.g. "Got permission denied while
	// trying to connect to the Docker daemon socket at unix:///var/run/docker.sock:
	// …". A bare "permission denied" alone is too broad: a real daemon answer may
	// carry it for a container operation, so require the docker-daemon or socket
	// context that marks the failure as transport rather than an answer.
	if strings.Contains(s, "permission denied") &&
		(strings.Contains(s, "Docker daemon") || strings.Contains(s, "docker.sock")) {
		return true
	}
	// An unresolvable pinned DOCKER_CONTEXT ("context \"foo\": context not found")
	// — the CLI could not name an engine to dial at all.
	if strings.Contains(s, "context not found") {
		return true
	}
	return false
}

// clearStagingDir removes the per-session staging dir. Called only once the
// container it fed is confirmed gone — after `docker rm` succeeds, or when no
// container was ever created.
func (p *dockerProvisioner) clearStagingDir() {
	if p.stagingDir == "" {
		return
	}
	if err := os.RemoveAll(p.stagingDir); err != nil {
		log.WarningLog.Printf("docker runtime: removing the staging dir %s failed: %v", p.stagingDir, err)
	}
	p.stagingDir = ""
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
