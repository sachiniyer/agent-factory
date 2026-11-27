package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The round-trip image builds are registry-free AND mirror-free by
// construction (#2521, #7896).
//
// The tests here (the docker/ssh round-trips, the two ArchiveRestore variants,
// the account-alias, symlinked-home and sandbox-host images) build throwaway
// images on top of a public base. Two different fetches have reddened them on
// somebody else's PR:
//
//	the FROM line resolving alpine:3.20 through docker.io (#2521: master on
//	the #2516 merge, then the v1.0.211 stable release an hour later);
//	the apk layer fetching packages from a live alpine mirror inside the
//	build's 300s budget (#3779 first, then #7896 twice: a slow — not down —
//	mirror had the build cancelled at package 16 of 19, on a TUI-only PR).
//
// So the one network-dependent image is built ONCE per test binary here, and
// the per-test builds on top of it fetch nothing at all:
//
//  1. OBTAINING + INSTALLING happens in requireRoundTripBaseImage, at most
//     once per test binary: pull the public base (bounded retry — the only
//     registry step left), then BUILD roundTripBaseImage from it, installing
//     every package the round-trip images need through the hardened apkAddRun
//     layer under baseImageBuildTimeout — a clearly-labelled infrastructure
//     budget a slow mirror spends instead of a per-test build's 300s.
//  2. BUILDING on top happens FROM roundTripBaseImage, a local-only name, and
//     the per-test Dockerfiles carry no package install and no other fetch —
//     a FROM no registry serves plus RUN steps that touch no network means
//     buildkit has nothing it could send out.
//
// Measured against docker 28.5.2 with the daemon's egress dropped: a build
// whose base is already in the local image store completes without a single
// registry request, and a build whose base is absent fails in `[internal]
// load metadata` with exactly the #2521 signature. Presence in the local
// store — not the spelling of the FROM line — is what removes the round
// trip; the local-only name is what keeps it removed, since no future
// resolve-mode or `--pull` change can turn it back into a fetch. The mirror
// half is proven the same way: TestImageBuildsWithRegistryBlocked drops the
// isolated daemon's forwarding too, so the subject builds pass only by
// fetching literally nothing — no registry, no package mirror.
// TestPerTestDockerfilesFetchNothingOverTheNetwork asserts the text side of
// that with no docker at all.
//
// Doing this here rather than in CI is the other half of the fix, for both
// fetches. #2525 warmed the base in a workflow step, but the lanes that run
// `go test ./...` (pr.yml, build.yml, auto-release.yml, stable-release.yml —
// plus sandbox-e2e.yml and the registry-free job) never all got it, and both
// observed #2521 failures landed on lanes that lacked it. A package-install
// step would have to be copied to every lane, and the next lane added would
// forget it; the binary-side build covers every lane and every developer
// machine for free, and the issue's own escape hatch — "the test builds it
// under its own clearly-labelled infra budget" — is exactly what
// ensureRoundTripBaseImage is.
const (
	// roundTripBaseUpstream is the public base the round-trip images are built
	// on, and the one reference in these tests that a registry may serve.
	roundTripBaseUpstream = "alpine:3.20"

	// roundTripBaseImage is the LOCAL-ONLY name the per-test Dockerfiles FROM.
	// Nothing publishes `af-integration-base`, so the reference can only ever
	// be satisfied from the local image store. The tag carries the upstream
	// version so that bumping roundTripBaseUpstream cannot silently keep
	// serving a stale base.
	//
	// Since #7896 it is a BUILT image — upstream plus the package set below —
	// not a bare retag, so "present" means "carries our labels", not merely
	// "the tag resolves" (see dockerBaseImageCurrent).
	roundTripBaseImage = "af-integration-base:alpine-3.20"

	// alpineFallbackMirror is a SECOND repository for the apk layer below
	// (#3779). The base image's own /etc/apk/repositories points at dl-cdn,
	// which is Fastly; this one is kernel.org, a different operator on
	// different infrastructure, so one having a bad minute does not correlate
	// with the other. It is an official mirror listed by alpinelinux.org.
	alpineFallbackMirror = "https://mirrors.edge.kernel.org/alpine"

	// apkAddAttempts bounds the retry. Enough to ride out the seconds-long
	// blip that was observed; small enough that a real outage fails the build
	// promptly rather than holding a runner for minutes.
	apkAddAttempts = 3

	// baseImageBuildTimeout is the budget for the ONE remaining network fetch
	// these tests make — building the base image installs the packages every
	// round-trip image needs, once per test binary (#7896). It is deliberately
	// separate from, and larger than, a per-test build's 300s: a slow mirror
	// spends THIS budget, labelled as infrastructure, instead of being
	// cancelled mid-install inside a test's own build.
	baseImageBuildTimeout = 10 * time.Minute

	// basePackagesLabel and baseDigestLabel mark a base built by this revision
	// of the harness. The tag alone cannot be trusted: a base left by a
	// pre-#7896 run is a bare retag of the upstream image with no packages,
	// and the per-test builds no longer run apk, so trusting it would fail
	// them on a missing git. The digest covers the whole generated Dockerfile
	// (minus the LABEL lines), so ANY drift — not just the package list —
	// rebuilds rather than silently reuses a stale base.
	basePackagesLabel = "af.integration.base-packages"
	baseDigestLabel   = "af.integration.base-digest"
)

// roundTripBasePackages is the union of what every round-trip image needs
// (docker: git/tmux/bash; the sshd images: + openssh-server). Installed once,
// into the base — never inside a per-test build (#7896). A per-test image that
// needs another package adds it HERE, not an apk line of its own:
// TestPerTestDockerfilesFetchNothingOverTheNetwork is what makes "here" the
// only place a package may come from.
var roundTripBasePackages = []string{"git", "tmux", "bash", "openssh-server"}

var (
	baseImageOnce sync.Once
	// baseImageUnavailable records that the base could not be obtained at all —
	// environmental, the same class as docker being absent, so it skips.
	baseImageUnavailable error
	// baseImageBroken records a failure past "could not pull the upstream" —
	// the base build itself. Its message already says whether the package
	// layer (the only fetch left) went down with the mirrors or something else
	// is genuinely defective, so it fails loudly either way.
	baseImageBroken error
)

// requireRoundTripBaseImage guarantees roundTripBaseImage — the local-only
// base with the packages baked in — is in the local image store, and returns
// it, fetching and building it at most once per test binary.
//
// It skips ONLY when the upstream base cannot be pulled at all — an
// unreachable registry is environmental unavailability, exactly like
// requireDocker's missing daemon, and no product defect can produce it.
// Everything past that fails loudly, with a message that names the layer that
// failed (#7896): the package install is the only network fetch left, so its
// failure reads as the mirror outage it is, not as "a real build error".
func requireRoundTripBaseImage(t *testing.T) string {
	t.Helper()
	baseImageOnce.Do(func() {
		started := time.Now()
		built, baseImageUnavailable, baseImageBroken := ensureRoundTripBaseImage()
		// One line per test binary: the base is either built here or reused
		// with current labels, and a fetch that takes minutes should be
		// visible in the log rather than attributed to the test that
		// happened to run first (#7896 R5). The failure paths below carry
		// their own named messages.
		switch {
		case baseImageUnavailable != nil || baseImageBroken != nil:
		case built:
			t.Logf("built the round-trip base image %s once for this test binary, in %s",
				roundTripBaseImage, time.Since(started).Round(100*time.Millisecond))
		default:
			t.Logf("reused the round-trip base image %s — its labels are current (checked in %s)",
				roundTripBaseImage, time.Since(started).Round(100*time.Millisecond))
		}
	})
	if baseImageBroken != nil {
		t.Fatalf("preparing the round-trip base image failed: %v", baseImageBroken)
	}
	if baseImageUnavailable != nil {
		t.Skipf("could not pull the round-trip base image's upstream %s, so there is nothing to build the base from: %v\n"+
			"This is the one registry-dependent step left in these tests (#2521); the builds themselves "+
			"never resolve a remote reference. A failure anywhere past this point is a real defect, not this skip.",
			roundTripBaseUpstream, baseImageUnavailable)
	}
	return roundTripBaseImage
}

// ensureRoundTripBaseImage populates the local-only base tag — a BUILD, not a
// retag (#7896). It returns (built, unavailable, broken): at most one of the
// errors is non-nil, and built (this call ran the build, rather than reusing a
// label-current base) is only meaningful when both are nil.
//
//	unavailable — the upstream base could not be pulled; a registry problem,
//	              environmental, and the caller skips for it.
//	broken      — anything past that: the build context, or the base build
//	              itself. The message's label comes from the failing step's
//	              own signature (see classifyBaseBuildFailure): a mirror
//	              outage, a registry outage, a slow fetch — still a loud
//	              failure of this one labelled infrastructure step, never a
//	              silent skip and never a per-test build timeout.
func ensureRoundTripBaseImage() (built bool, unavailable, broken error) {
	if dockerBaseImageCurrent() {
		return false, nil, nil
	}
	if !dockerImagePresent(roundTripBaseUpstream) {
		if err := pullRoundTripBase(); err != nil {
			return false, err, nil
		}
	}
	// The base carries every package the round-trip images need, so this is
	// the one build left that can touch a network — once per test binary,
	// under its own labelled budget, instead of inside each test's 300s.
	dir, err := os.MkdirTemp("", "af-integration-base")
	if err != nil {
		return false, nil, fmt.Errorf("create the base image build context: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"),
		[]byte(roundTripBaseDockerfile(roundTripBaseUpstream)), 0o644); err != nil {
		return false, nil, fmt.Errorf("write the base image Dockerfile: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), baseImageBuildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "build", "-t", roundTripBaseImage, dir)
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil, nil
	}
	return false, nil, classifyBaseBuildFailure(ctx.Err(), err, string(out))
}

// classifyBaseBuildFailure labels a failed base build by the signature of the
// step that actually failed (#7896 R2), not by any fetch-adjacent string in
// the log:
//
//   - the context deadline is the infrastructure budget spent — the package
//     layer is the only fetch, so a stall is a slow mirror;
//   - "apk add FAILED" is emitted by the hardened layer itself, only after
//     EVERY attempt on BOTH mirrors was exhausted — the mirror-outage
//     signature. A lone "WARNING: fetching" is not one: apk prints it whenever
//     one mirror hiccups even though the fallback goes on to serve, so it can
//     be present in the log of a build that later failed for another reason
//     entirely (e.g. "no space left" exporting a layer). It never decides the
//     label on its own.
//   - "failed to resolve source metadata" is the registry: the FROM image was
//     verified local a moment ago, so reaching for it at all means something
//     is resolving remotely — #2521's own signature;
//   - anything else is a real build error, and says so.
func classifyBaseBuildFailure(ctxErr, buildErr error, output string) error {
	switch {
	case ctxErr == context.DeadlineExceeded:
		return fmt.Errorf("building the round-trip base image exceeded its %s infrastructure budget: %v. "+
			"The package layer is the only network fetch in this build, so the stall is almost certainly a slow alpine mirror — "+
			"an infrastructure failure of this step, not a defect in this tree; retry when a mirror answers.\n%s",
			baseImageBuildTimeout, buildErr, output)
	case strings.Contains(output, "apk add FAILED"):
		return fmt.Errorf("building the round-trip base image could not install %s from either alpine mirror: %v. "+
			"The package layer's own script reported every attempt exhausted, so this is a mirror outage — an infrastructure "+
			"failure of this step, not a defect in this tree; retry when a mirror answers.\n%s",
			strings.Join(roundTripBasePackages, " "), buildErr, output)
	case strings.Contains(output, "failed to resolve source metadata"):
		return fmt.Errorf("building the round-trip base image could not resolve its FROM image %s: %v. "+
			"The image was in the local store moments ago, so reaching for it at all is a registry outage signature "+
			"(#2521's own) — an infrastructure failure, not a defect in this tree.\n%s",
			roundTripBaseUpstream, buildErr, output)
	default:
		return fmt.Errorf("building the round-trip base image failed outside the package-install layer — "+
			"the only network fetch this build performs — so this is a real build error, not a mirror or registry outage: %v\n%s",
			buildErr, output)
	}
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

// dockerBaseImageCurrent reports whether roundTripBaseImage resolves AND was
// built by this revision of the harness — i.e. carries basePackagesLabel and
// baseDigestLabel with today's values. A base left by a pre-#7896 run is a
// bare retag of the upstream image: the tag resolves but it has no labels and
// no packages, and since the per-test builds no longer run apk, trusting it
// would fail them later on a missing git — much less clearly than rebuilding
// here. "Rebuild it under the labelled budget" is one of the two behaviours
// #7896 allows when the pre-built image is absent, and a stale base is a
// kind of absent.
func dockerBaseImageCurrent() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect",
		"-f", "{{json .Config.Labels}}", roundTripBaseImage)
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	var labels map[string]string
	if err := json.Unmarshal(out, &labels); err != nil {
		return false
	}
	return labels[basePackagesLabel] == strings.Join(roundTripBasePackages, " ") &&
		labels[baseDigestLabel] == baseDockerfileDigest(roundTripBaseUpstream)
}

// baseDockerfileDigest fingerprints the base image's FROM + package layer —
// everything BEFORE the LABEL lines, which is all the digest can cover (it
// cannot quote itself). Any change to the upstream, the package list or the
// apk layer invalidates stored bases.
func baseDockerfileDigest(upstream string) string {
	sum := sha256.Sum256([]byte(roundTripBaseDockerfileBody(upstream)))
	return fmt.Sprintf("%x", sum[:8])
}

// alpineBranch turns roundTripBaseUpstream ("alpine:3.20") into the path
// component an alpine mirror uses for that release ("v3.20").
//
// Derived rather than spelled a second time: a mirror URL carrying a hardcoded
// "v3.20" would keep serving the previous release after someone bumped the base,
// and the build would still succeed — silently installing packages from the
// wrong branch. TestApkMirrorBranchTracksTheBase pins the derivation.
func alpineBranch() string {
	_, version, ok := strings.Cut(roundTripBaseUpstream, ":")
	if !ok {
		return ""
	}
	return "v" + version
}

// apkAddRun renders the RUN line that installs pkgs into the BASE image — the
// one package fetch left in these tests (#7896), executed once per test binary
// inside the labelled infrastructure step, never inside a per-test build.
//
// THE BUG it survives (#3779). This one fetch is intrinsic, which is what
// separates it from #3774: there the failing source was one the step had no
// use for, so the fix was to stop letting it vote. Here the packages genuinely
// have to come from a mirror, so the layer tolerates one mirror's bad minute
// without tolerating an outage:
//
//	a SECOND mirror   — on a different operator, so apk has somewhere to go when
//	                    one of them is unwell. apk warns about the unreachable
//	                    repository and installs from the other; it fails only
//	                    when the packages are available from neither.
//	a BOUNDED retry   — for the case where both are unwell in the same moment.
//	a LOUD failure    — every attempt exhausted exits 1 with the package list.
//	                    Deliberately NOT a skip-with-reason: a check that
//	                    declines to run is the failure mode this repo keeps
//	                    paying for, and a sustained mirror outage SHOULD fail
//	                    this one infrastructure step, loudly.
//
// Package names are rendered as literal words and never re-parsed, and the
// emitted script is exercised directly against a stubbed apk in
// apk_mirror_test.go — the mirrors are the variable there, so the retry and the
// fallback are measured rather than assumed.
func apkAddRun(pkgs ...string) string {
	list := strings.Join(pkgs, " ")
	branch := alpineBranch()
	return "RUN set -eu; \\\n" +
		"    for attempt in $(seq 1 " + strconv.Itoa(apkAddAttempts) + "); do \\\n" +
		"      if apk add --no-cache \\\n" +
		"          -X " + alpineFallbackMirror + "/" + branch + "/main \\\n" +
		"          -X " + alpineFallbackMirror + "/" + branch + "/community \\\n" +
		"          " + list + "; then exit 0; fi; \\\n" +
		"      echo \"apk add attempt $attempt failed (" + list + "); retrying\" >&2; \\\n" +
		"      sleep $((attempt * 3)); \\\n" +
		"    done; \\\n" +
		"    echo \"apk add FAILED after " + strconv.Itoa(apkAddAttempts) +
		" attempts on every mirror: " + list + "\" >&2; \\\n" +
		"    exit 1\n"
}

// roundTripBaseDockerfileBody is the fetchable part of the base Dockerfile —
// the upstream FROM plus the ONE package-install layer every round-trip image
// shares (#7896) — and the exact bytes the digest label fingerprints.
func roundTripBaseDockerfileBody(upstream string) string {
	return "FROM " + upstream + "\n" + apkAddRun(roundTripBasePackages...)
}

// roundTripBaseDockerfile renders the base image's own Dockerfile: the body
// plus the labels dockerBaseImageCurrent verifies. The digest is taken over
// the body only — it cannot quote itself.
func roundTripBaseDockerfile(upstream string) string {
	return roundTripBaseDockerfileBody(upstream) +
		"LABEL " + basePackagesLabel + "=\"" + strings.Join(roundTripBasePackages, " ") + "\" \\\n" +
		"      " + baseDigestLabel + "=\"" + baseDockerfileDigest(upstream) + "\"\n"
}

// dockerRoundTripDockerfile is the docker round-trip image: the local base —
// which already carries git/tmux/bash (#7896) — plus the fixture's git safety
// mark. No package install, no fetch of any kind.
//
// It takes the base as a parameter rather than baking it in so that
// TestImageBuildsWithRegistryBlocked builds THIS text, not a copy of it — a
// second spelling of the FROM line is exactly how the property under test would
// rot without anything going red.
func dockerRoundTripDockerfile(base string) string {
	return "FROM " + base + "\n" + trustBindMountedRepoRun
}

// trustBindMountedRepoRun marks the fixture's bind-mounted bare repo as safe.
// The clone source (/repo.git) is owned by the host test user, not the
// container's root, so git refuses it as "dubious ownership". The product no
// longer writes a global safe.directory into the host (#5571 W2.7), so the
// fixture image carries the exemption instead.
const trustBindMountedRepoRun = "RUN git config --system --add safe.directory '*'\n"

// sshdRoundTripDockerfile is the ssh round-trip image: a real sshd target for
// the ssh runtime, with host keys baked at build time so they are stable for
// known_hosts pinning. openssh-server comes from the base (#7896); every step
// below is local-only.
func sshdRoundTripDockerfile(base string) string {
	return "FROM " + base + "\n" +
		trustBindMountedRepoRun +
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
