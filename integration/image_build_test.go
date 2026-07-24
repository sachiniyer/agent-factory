package integration_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The integration round-trip tests build a slim `FROM alpine:3.20` image on every
// run (buildDockerRoundTripImage / buildSSHDRoundTripImage). That live `docker
// build` reaches Docker Hub to resolve the base image, so a registry blip — an
// i/o timeout, a DNS failure, a 429 pull-rate-limit — reddened master twice in an
// hour (#2515, #2521) with a failure that has nothing to do with af.
//
// buildDockerImageOrSkip makes that boundary honest. A transient failure is
// retried with backoff; a registry that stays unreachable across every attempt
// SKIPs the test — the same "this environment cannot support this test" outcome
// requireDocker already produces — rather than failing red on infrastructure.
//
// The skip is deliberately NARROW. A build failure that is NOT a transient
// network/registry condition — a broken Dockerfile, a failed RUN step, a missing
// build input, or the product itself — fails LOUD and unretried. It would fail
// identically on every attempt, and hiding it behind a retry or a skip is the one
// thing this must never do. The classification lives in the pure functions below
// so it can be unit-tested exhaustively without docker (image_build_classify_test.go).

const (
	dockerBuildAttempts = 3
	dockerBuildTimeout  = 300 * time.Second
)

// dockerBuildBackoff is a var so a wrapper test can zero the sleep. 3s, 6s — the
// backoff is bounded and dwarfed by a single failing attempt's own dial timeout,
// so the total retry budget stays modest.
var dockerBuildBackoff = func(attempt int) time.Duration {
	return time.Duration(attempt) * 3 * time.Second
}

// buildReporter is the slice of *testing.T the wrapper needs. Abstracting it lets
// a test drive the retry/skip/fail routing with a recording reporter instead of
// real docker, which the sanctioned container fence cannot run.
type buildReporter interface {
	Helper()
	Fatalf(format string, args ...any)
	Skipf(format string, args ...any)
}

// dockerBuildFunc runs one `docker build` attempt. Injected so the loop is
// testable without docker.
type dockerBuildFunc func(ctx context.Context, tag, dir string) ([]byte, error)

func realDockerBuild(ctx context.Context, tag, dir string) ([]byte, error) {
	return exec.CommandContext(ctx, "docker", "build", "-t", tag, dir).CombinedOutput()
}

// buildDockerImageOrSkip runs `docker build -t tag dir`, retrying transient
// failures and skipping only a persistently-unreachable registry. `what` names
// the image for the test's failure/skip message. On success it returns; on a real
// build failure it calls t.Fatalf; on registry-unreachable it calls t.Skipf.
func buildDockerImageOrSkip(t *testing.T, tag, dir, what string) {
	t.Helper()
	buildDockerImageOrSkipWith(t, realDockerBuild, tag, dir, what)
}

func buildDockerImageOrSkipWith(r buildReporter, build dockerBuildFunc, tag, dir, what string) {
	r.Helper()
	for attempt := 1; attempt <= dockerBuildAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), dockerBuildTimeout)
		out, err := build(ctx, tag, dir)
		cancel()
		if err == nil {
			return
		}
		switch dockerBuildFailover(string(out), attempt < dockerBuildAttempts) {
		case actionFailDeterministic:
			r.Fatalf("building %s failed — not a transient registry/network error, so this is a real build "+
				"failure and must not be retried or skipped (#2521): %v\n%s", what, err, out)
			return
		case actionRetry:
			time.Sleep(dockerBuildBackoff(attempt))
		case actionSkipRegistry:
			reason, _ := dockerBuildRegistryUnreachable(string(out))
			r.Skipf("skipping %s: the container registry stayed unreachable across %d attempts (%s) — "+
				"an infrastructure condition, not a product failure (#2521)\n%s", what, dockerBuildAttempts, reason, out)
			return
		case actionFailTransient:
			r.Fatalf("building %s failed after %d attempts with a transient but non-registry error; failing "+
				"loud rather than skipping so a real failure is never masked (#2521): %v\n%s", what, dockerBuildAttempts, err, out)
			return
		}
	}
	// Unreachable: the final attempt always resolves to a terminal action (skip or
	// fail) inside the loop. Guard anyway so a later change can't silently fall
	// through to a green return on an unbuilt image.
	r.Fatalf("building %s did not resolve to a build, a skip, or a failure after %d attempts", what, dockerBuildAttempts)
}

// failoverAction is the decision after one failed `docker build` attempt.
type failoverAction int

const (
	// actionFailDeterministic: the failure carries no transient-network signature,
	// so it is a Dockerfile/RUN/product error that every retry would reproduce.
	actionFailDeterministic failoverAction = iota
	// actionRetry: transient, and attempts remain.
	actionRetry
	// actionSkipRegistry: transient, attempts exhausted, and tied to the registry —
	// the only case that may be skipped.
	actionSkipRegistry
	// actionFailTransient: transient and exhausted, but NOT tied to the registry
	// (e.g. a package-mirror blip) — fail loud rather than swallow a possible bug.
	actionFailTransient
)

// dockerBuildFailover encodes the whole retry/skip/fail policy as a pure function
// of the build output and whether attempts remain, so it is unit-testable without
// docker. The wrapper above is thin glue over this decision.
func dockerBuildFailover(output string, attemptsLeft bool) failoverAction {
	if !dockerBuildLooksTransient(output) {
		return actionFailDeterministic
	}
	if attemptsLeft {
		return actionRetry
	}
	if _, unreachable := dockerBuildRegistryUnreachable(output); unreachable {
		return actionSkipRegistry
	}
	return actionFailTransient
}

// transientNetworkTokens are substrings that appear only for a transient
// network/registry condition — a timeout, a DNS failure, a dropped connection, or
// a rate limit — and never for a Dockerfile/RUN/product error. Their presence is
// what licenses a RETRY (safe: a deterministic failure carries none of them, so it
// is never retried and never masked).
var transientNetworkTokens = []string{
	"i/o timeout",
	"tls handshake timeout",
	"timeout awaiting response headers",
	"no such host",
	"temporary failure in name resolution",
	"connection refused",
	"connection reset by peer",
	"network is unreachable",
	"deadlineexceeded",
	"context deadline exceeded",
	"429 too many requests",
	"toomanyrequests",
}

// dockerRegistryHosts are hostnames a `docker build` contacts only to resolve or
// pull a base image from a registry. buildkit prints them solely when logging such
// a request (a failing `Head https://registry-1.docker.io/...`), never on a
// successful `load metadata` line — so their presence ties a transient failure to
// the REGISTRY, distinguishing it from a package-mirror (dl-cdn.alpinelinux.org)
// or a RUN-step network error, neither of which may be skipped.
var dockerRegistryHosts = []string{
	"registry-1.docker.io",
	"registry.docker.io",
	"index.docker.io",
	"auth.docker.io",
	"production.cloudflare.docker.com",
}

func dockerBuildLooksTransient(output string) bool {
	lo := strings.ToLower(output)
	for _, tok := range transientNetworkTokens {
		if strings.Contains(lo, tok) {
			return true
		}
	}
	return false
}

// dockerBuildRegistryUnreachable reports the NARROW case that may be skipped: the
// base image could not be resolved or pulled because the container registry was
// unreachable or rate-limited. It requires a transient network token AND that the
// failure is tied to the registry (buildkit's base-image resolution phrase or a
// registry hostname), OR an unambiguous registry rate limit. Everything else — a
// failed RUN step, a package-mirror timeout, a missing COPY input, a product bug —
// returns false so the caller fails loud (#2521). reason is a short human label
// for the skip message.
func dockerBuildRegistryUnreachable(output string) (reason string, unreachable bool) {
	lo := strings.ToLower(output)

	// A registry pull-rate-limit is unambiguous on its own: only a registry emits
	// it, and no RUN step in these images talks to one.
	if strings.Contains(lo, "toomanyrequests") || strings.Contains(lo, "429 too many requests") {
		return "registry pull-rate-limit (429 / toomanyrequests)", true
	}

	if !dockerBuildLooksTransient(lo) {
		return "", false
	}
	// Tie the transient failure to the registry specifically. buildkit emits this
	// exact phrase only when the base image's manifest could not be fetched.
	if strings.Contains(lo, "failed to resolve source metadata") {
		return "base-image source-metadata resolution failed (registry unreachable)", true
	}
	for _, host := range dockerRegistryHosts {
		if strings.Contains(lo, host) {
			return "request to container registry " + host + " failed", true
		}
	}
	return "", false
}
