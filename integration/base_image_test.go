package integration_test

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// The round-trip image builds are registry-free by construction (#2521).
//
// Four tests here (TestDockerBackendRoundTrip, TestSSHBackendRoundTrip and the
// two ArchiveRestore variants) build a throwaway image on top of a public base.
// While the Dockerfiles said `FROM alpine:3.20`, every one of those builds
// resolved that tag through docker.io, so a Docker Hub blip reddened whatever
// happened to be running: master on the #2581 merge, then the v1.0.211 stable
// release an hour later.
//
// The fix splits the two jobs that build was doing at once:
//
//  1. OBTAINING the base. Happens here, at most once per test binary, with
//     bounded retry, and is the ONLY step that may touch a registry.
//  2. BUILDING on top of it. Happens FROM roundTripBaseImage, a name no
//     registry serves, so buildkit has nothing it could resolve remotely.
//
// Measured against docker 28.5.2 with the daemon's egress dropped: a build whose
// base is already in the local image store completes without a single registry
// request, and a build whose base is absent fails in `[internal] load metadata`
// with exactly the CI signature. So presence in the local store — not the
// spelling of the FROM line — is what removes the round trip; the local-only
// name is what keeps it removed, since no future resolve-mode or `--pull`
// change can turn it back into a fetch. TestImageBuildsWithRegistryBlocked
// holds both halves of that claim.
//
// Doing this here rather than in CI is the other half of the fix. #2525 warmed
// the base in a workflow step, but four separate lanes run `go test ./...`
// (pr.yml, build.yml, auto-release.yml, stable-release.yml) and only pr.yml
// ever got the step — which is why both observed failures happened on lanes
// that never had it. A guarantee that lives in the test binary covers every
// lane, plus every developer machine, and cannot be forgotten by the next
// workflow.
const (
	// roundTripBaseUpstream is the public base the round-trip images are built
	// on, and the one reference in these tests that a registry may serve.
	roundTripBaseUpstream = "alpine:3.20"

	// roundTripBaseImage is the LOCAL-ONLY name the Dockerfiles FROM. Nothing
	// publishes `af-integration-base`, so the reference can only ever be
	// satisfied from the local image store. The tag carries the upstream
	// version so that bumping roundTripBaseUpstream cannot silently keep
	// serving a retag of the old one.
	roundTripBaseImage = "af-integration-base:alpine-3.20"
)

var (
	baseImageOnce sync.Once
	// baseImageUnavailable records that the base could not be obtained at all —
	// environmental, the same class as docker being absent, so it skips.
	baseImageUnavailable error
	// baseImageBroken records a docker failure that is NOT about reachability
	// (a `docker tag` that fails is not a registry outage), so it fails loudly.
	baseImageBroken error
)

// requireRoundTripBaseImage guarantees roundTripBaseImage is in the local image
// store and returns it, fetching the upstream base at most once per test binary.
//
// It skips ONLY when the base cannot be obtained at all — an unreachable
// registry is environmental unavailability, exactly like requireDocker's missing
// daemon, and no product defect can produce it. Every build failure after this
// point is a real one and still fails loudly.
func requireRoundTripBaseImage(t *testing.T) string {
	t.Helper()
	baseImageOnce.Do(func() { baseImageUnavailable, baseImageBroken = ensureRoundTripBaseImage() })
	if baseImageBroken != nil {
		t.Fatalf("preparing the round-trip base image failed for a reason that is not registry reachability: %v", baseImageBroken)
	}
	if baseImageUnavailable != nil {
		t.Skipf("could not obtain the round-trip base image %s, so there is nothing to build on: %v\n"+
			"This is the one registry-dependent step left in these tests (#2521); the builds themselves "+
			"never resolve a remote reference. A failure anywhere past this point is a real defect, not this skip.",
			roundTripBaseUpstream, baseImageUnavailable)
	}
	return roundTripBaseImage
}

// ensureRoundTripBaseImage populates the local-only base tag. It returns
// (unavailable, broken): at most one is non-nil.
func ensureRoundTripBaseImage() (unavailable, broken error) {
	if dockerImagePresent(roundTripBaseImage) {
		return nil, nil
	}
	if !dockerImagePresent(roundTripBaseUpstream) {
		if err := pullRoundTripBase(); err != nil {
			return err, nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "tag", roundTripBaseUpstream, roundTripBaseImage)
	cmd.WaitDelay = 10 * time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("docker tag %s %s: %v: %s", roundTripBaseUpstream, roundTripBaseImage, err, strings.TrimSpace(string(out)))
	}
	return nil, nil
}

// pullRoundTripBase fetches the public base with bounded retry. The blips that
// caused #2521 lasted seconds, so a few spaced attempts absorb them; a sustained
// outage returns the last error and the caller skips.
func pullRoundTripBase() error {
	var last error
	for i, wait := range []time.Duration{0, 5 * time.Second, 15 * time.Second} {
		if wait > 0 {
			time.Sleep(wait)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		cmd := exec.CommandContext(ctx, "docker", "pull", roundTripBaseUpstream)
		cmd.WaitDelay = 10 * time.Second
		out, err := cmd.CombinedOutput()
		cancel()
		if err == nil {
			return nil
		}
		last = fmt.Errorf("pull attempt %d/3: %v: %s", i+1, err, strings.TrimSpace(string(out)))
	}
	return last
}

// dockerImagePresent reports whether a reference is in the local image store.
// `docker image inspect` is local-only — it answers with the registry
// unreachable, which is what makes it a usable precondition here.
func dockerImagePresent(ref string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", ref)
	cmd.WaitDelay = 10 * time.Second
	return cmd.Run() == nil
}

// dockerRoundTripDockerfile is the docker round-trip image: the minimum a BYO
// image needs for the in-container agent-server (git worktree + tmux PTY).
//
// It takes the base as a parameter rather than baking it in so that
// TestImageBuildsWithRegistryBlocked builds THIS text, not a copy of it — a
// second spelling of the FROM line is exactly how the property under test would
// rot without anything going red.
func dockerRoundTripDockerfile(base string) string {
	return "FROM " + base + "\nRUN apk add --no-cache git tmux bash\n"
}

// sshdRoundTripDockerfile is the ssh round-trip image: a real sshd target for
// the ssh runtime, with host keys baked at build time so they are stable for
// known_hosts pinning.
func sshdRoundTripDockerfile(base string) string {
	return "FROM " + base + "\n" +
		"RUN apk add --no-cache git tmux bash openssh-server\n" +
		"RUN ssh-keygen -A && mkdir -p /root/.ssh && chmod 700 /root/.ssh\n" +
		// The ssh runtime reaches the remote agent-server through an ssh
		// local-forward (direct-tcpip) tunnel, which the sshd must permit — alpine's
		// default sshd_config ships an active `AllowTcpForwarding no` (first-match
		// wins, so replace it in place rather than appending an override).
		"RUN sed -i 's/^AllowTcpForwarding.*/AllowTcpForwarding yes/' /etc/ssh/sshd_config\n" +
		"COPY entrypoint.sh /entrypoint.sh\n" +
		"RUN chmod +x /entrypoint.sh\n" +
		"ENTRYPOINT [\"/entrypoint.sh\"]\n"
}

// sshdRoundTripEntrypoint installs the mounted authorized_keys and runs sshd in
// the foreground.
func sshdRoundTripEntrypoint() string {
	return "#!/bin/sh\n" +
		"set -e\n" +
		"if [ -f /authorized_keys ]; then\n" +
		"  cp /authorized_keys /root/.ssh/authorized_keys\n" +
		"  chmod 600 /root/.ssh/authorized_keys\n" +
		"fi\n" +
		"exec /usr/sbin/sshd -D -e\n"
}
