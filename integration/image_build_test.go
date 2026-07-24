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

// The integration round-trip tests build a slim `FROM alpine:3.20` image on every
// run (buildDockerRoundTripImage / buildSSHDRoundTripImage). That live `docker
// build` reaches Docker Hub to resolve the base image, so a registry blip — an
// i/o timeout, a DNS failure, a 429 pull-rate-limit — reddened master twice in an
// hour (#2515, #2521) with a failure that has nothing to do with af.
//
// buildDockerImageOrSkip makes that boundary honest. A transient failure is
// retried with backoff (bounded by a TOTAL wall-clock budget, not just per
// attempt); a registry that stays unreachable across every attempt SKIPs the test
// — the same "this environment cannot support this test" outcome requireDocker
// already produces — rather than failing red on infrastructure. The first such
// verdict is MEMOIZED for the package run so the remaining docker/ssh tests skip
// immediately instead of each re-proving a durable outage against the package
// timeout (#2521 P2-b).
//
// The skip is deliberately NARROW. A build failure that is NOT a transient
// registry condition — a broken Dockerfile, a failed RUN step, a missing build
// input, an alpine PACKAGE-MIRROR blip (these Dockerfiles run `apk add` against
// dl-cdn.alpinelinux.org), or the product itself — fails LOUD and unretried. It
// would fail identically on every attempt, and hiding it behind a retry or a skip
// is the one thing this must never do (#2521). Every skip carries a loud,
// greppable marker so a persistent skip is visible rather than silently green.
//
// The classification and routing are pure/injectable so they can be unit-tested
// exhaustively without docker (image_build_classify_test.go,
// image_build_wrapper_test.go) — which matters because the docker tests skip in
// the container fence and on any host without docker.

const (
	dockerBuildAttempts = 3
	// dockerBuildTimeout caps a single attempt. dockerBuildTotalBudget caps ALL
	// attempts of one build together, so the worst case is bounded regardless of
	// how slowly the registry hangs (#2521 P2-b): pre-fix, 3 x 300s could reach
	// 900s for ONE build and blow the package timeout, aborting every other test
	// before the skip this exists for was ever reached.
	dockerBuildTimeout = 120 * time.Second

	// dockerRegistrySkipMarker is emitted on every registry-unreachable skip so a
	// persistent skip — which is otherwise a silent green — is greppable in CI
	// output (the Test job runs `go test -v`, which prints skip reasons) and
	// surfaced as a GitHub warning annotation (#2521 P2-a).
	dockerRegistrySkipMarker = "AF-DOCKER-REGISTRY-UNREACHABLE-SKIP"
)

// dockerBuildTotalBudget is a var so a wrapper test can zero it to exercise the
// budget cutoff.
var dockerBuildTotalBudget = 150 * time.Second

// dockerBuildBackoff is a var so a wrapper test can zero the sleep. 3s, 6s — the
// backoff is bounded and dwarfed by a single failing attempt's own dial timeout.
var dockerBuildBackoff = func(attempt int) time.Duration {
	return time.Duration(attempt) * 3 * time.Second
}

// emitRegistrySkipAnnotation surfaces a skip in the CI UI as a warning annotation.
// A var so unit tests can silence it.
var emitRegistrySkipAnnotation = func(reason string) {
	fmt.Printf("::warning::%s — %s\n", dockerRegistrySkipMarker, reason)
}

// registrySkipState memoizes a registry-unreachable verdict for one package run,
// so docker/ssh tests 2..N skip immediately instead of each re-proving a durable
// outage (a rate limit does not clear within a few retries) against the package
// timeout (#2521 P2-b).
type registrySkipState struct {
	mu     sync.Mutex
	hit    bool
	reason string
}

func (s *registrySkipState) verdict() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason, s.hit
}

func (s *registrySkipState) record(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hit {
		s.hit = true
		s.reason = reason
	}
}

// packageRegistrySkip is the memo shared by all real docker/ssh builds in this
// package run.
var packageRegistrySkip = &registrySkipState{}

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
	buildDockerImageOrSkipWith(t, realDockerBuild, packageRegistrySkip, tag, dir, what)
}

func buildDockerImageOrSkipWith(r buildReporter, build dockerBuildFunc, skip *registrySkipState, tag, dir, what string) {
	r.Helper()

	// Tests 2..N of a genuine outage skip at once: the registry was already found
	// unreachable this package run, and a durable rate limit will not have cleared.
	if reason, hit := skip.verdict(); hit {
		emitRegistrySkipAnnotation(reason + " (memoized)")
		r.Skipf("%s: skipping %s — the container registry was already found unreachable earlier in this "+
			"package run (%s); not re-proving a durable outage against the package timeout (#2521)",
			dockerRegistrySkipMarker, what, reason)
		return
	}

	deadline := time.Now().Add(dockerBuildTotalBudget)
	var lastOut []byte
	var lastErr error
	for attempt := 1; attempt <= dockerBuildAttempts; attempt++ {
		// The total budget, not just the per-attempt cap, bounds the worst case.
		// Attempt 1 always runs; later attempts stop once the budget is spent.
		if attempt > 1 && time.Until(deadline) <= 0 {
			break
		}
		perAttempt := dockerBuildTimeout
		if remaining := time.Until(deadline); remaining > 0 && remaining < perAttempt {
			perAttempt = remaining
		}
		ctx, cancel := context.WithTimeout(context.Background(), perAttempt)
		out, err := build(ctx, tag, dir)
		cancel()
		if err == nil {
			return
		}
		lastOut, lastErr = out, err

		moreAttempts := attempt < dockerBuildAttempts && time.Until(deadline) > 0
		switch dockerBuildFailover(string(out), moreAttempts) {
		case actionFailDeterministic:
			r.Fatalf("building %s failed — not a transient registry/network error, so this is a real build "+
				"failure and must not be retried or skipped (#2521): %v\n%s", what, err, out)
			return
		case actionRetry:
			time.Sleep(dockerBuildBackoff(attempt))
		case actionSkipRegistry:
			reason, _ := dockerBuildRegistryUnreachable(string(out))
			skip.record(reason)
			emitRegistrySkipAnnotation(reason)
			r.Skipf("%s: skipping %s: the container registry stayed unreachable across %d attempts (%s) — "+
				"an infrastructure condition, not a product failure (#2521)\n%s", dockerRegistrySkipMarker, what, attempt, reason, out)
			return
		case actionFailTransient:
			r.Fatalf("building %s failed after %d attempts with a transient but non-registry error; failing "+
				"loud rather than skipping so a real failure is never masked (#2521): %v\n%s", what, attempt, err, out)
			return
		}
	}

	// The loop exited only because the total budget was spent mid-retry. Decide on
	// the last failure the same way: skip a registry outage, fail anything else.
	if reason, unreachable := dockerBuildRegistryUnreachable(string(lastOut)); unreachable {
		skip.record(reason)
		emitRegistrySkipAnnotation(reason + " (wall-clock budget exhausted)")
		r.Skipf("%s: skipping %s: the container registry stayed unreachable until the %s build budget was "+
			"spent (%s) — infrastructure, not a product failure (#2521)\n%s",
			dockerRegistrySkipMarker, what, dockerBuildTotalBudget, reason, lastOut)
		return
	}
	r.Fatalf("building %s failed until the %s build budget was spent, with a transient but non-registry "+
		"error; failing loud rather than skipping so a real failure is never masked (#2521): %v\n%s",
		what, dockerBuildTotalBudget, lastErr, lastOut)
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
// is never retried and never masked). A rate limit is included here, NOT as a
// standalone skip, precisely because these images `apk add` from a package mirror
// that can itself 429 — so a 429 must still be tied to the registry before it may
// be skipped (#2521 P2-a).
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
// failure is tied to the registry — buildkit's base-image resolution phrase, or a
// Docker registry hostname. Everything else — a failed RUN step, a package-mirror
// timeout OR rate-limit, a missing COPY input, a product bug — returns false so
// the caller fails loud (#2521). reason is a short human label for the skip
// message.
//
// A 429 is NOT skipped on its own: these images `apk add` from a package mirror
// that can 429 too, and that is the swallowed-real-failure this must not admit. It
// must clear the same registry tie as every other transient token (#2521 P2-a).
func dockerBuildRegistryUnreachable(output string) (reason string, unreachable bool) {
	lo := strings.ToLower(output)
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
