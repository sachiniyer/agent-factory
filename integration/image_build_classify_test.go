package integration_test

import "testing"

// These fixtures are representative of real `docker build` / buildkit output. The
// registry-unreachable cases are the ones #2521 wants skipped; the RUN-step,
// package-mirror, and missing-input cases are the ones that must NEVER be skipped,
// because they are how a broken Dockerfile, the af binary, or the product would
// surface. The classifier is the whole safety of this change, so it is pinned
// directly here rather than only through the docker-gated tests (which skip in the
// container fence and on any host without docker).

// the exact output that reddened #2515 and master (#2521), trimmed.
const outputRegistryIOTimeout = `#1 [internal] load build definition from Dockerfile
#1 DONE 0.0s
#2 [internal] load metadata for docker.io/library/alpine:3.20
#2 ERROR: failed to do request: Head "https://registry-1.docker.io/v2/library/alpine/manifests/3.20": dial tcp 54.224.71.29:443: i/o timeout
------
 > [internal] load metadata for docker.io/library/alpine:3.20:
------
Dockerfile:1
--------------------
   1 | >>> FROM alpine:3.20
   2 |     RUN apk add --no-cache git tmux bash
--------------------
ERROR: failed to build: failed to solve: DeadlineExceeded: alpine:3.20: failed to resolve source metadata for docker.io/library/alpine:3.20: failed to do request: Head "https://registry-1.docker.io/v2/library/alpine/manifests/3.20": dial tcp 54.224.71.29:443: i/o timeout`

const outputRegistryRateLimit = `#2 [internal] load metadata for docker.io/library/alpine:3.20
#2 ERROR: failed to do request: Head "https://registry-1.docker.io/v2/library/alpine/manifests/3.20": toomanyrequests: You have reached your pull rate limit. You may increase the limit by authenticating and upgrading
ERROR: failed to build: failed to solve: failed to resolve source metadata for docker.io/library/alpine:3.20: toomanyrequests: 429 Too Many Requests`

const outputRegistryDNSFailure = `#2 [internal] load metadata for docker.io/library/alpine:3.20
#2 ERROR: failed to do request: Head "https://registry-1.docker.io/v2/library/alpine/manifests/3.20": dial tcp: lookup registry-1.docker.io on 127.0.0.53:53: no such host
ERROR: failed to build: failed to solve: failed to resolve source metadata for docker.io/library/alpine:3.20: no such host`

const outputRegistryTLSTimeout = `#2 [internal] load metadata for docker.io/library/alpine:3.20
#2 ERROR: failed to do request: Head "https://auth.docker.io/token": net/http: TLS handshake timeout
ERROR: failed to build: failed to solve: failed to resolve source metadata for docker.io/library/alpine:3.20: TLS handshake timeout`

// a RUN step that fails because a package name is wrong — a Dockerfile bug. No
// network signature; must fail deterministically, never skip.
const outputRunUnableToSelectPackages = `#2 [internal] load metadata for docker.io/library/alpine:3.20
#2 DONE 0.4s
#5 [2/2] RUN apk add --no-cache git tmux bash bogus-pkg
#5 1.201 ERROR: unable to select packages:
#5 1.201   bogus-pkg (no such package):
#5 1.201     required by: world[bogus-pkg]
#5 ERROR: process "/bin/sh -c apk add --no-cache git tmux bash bogus-pkg" did not complete successfully: exit code: 1
ERROR: failed to build: failed to solve: process "/bin/sh -c apk add --no-cache git tmux bash bogus-pkg" did not complete successfully: exit code: 1`

// a RUN step whose network fetch to the ALPINE PACKAGE MIRROR times out. This is
// transient, so it may be retried — but it is NOT the container registry, so after
// retries it must FAIL, not skip: an apk failure can equally be a Dockerfile bug,
// and #2521 forbids swallowing that. Note the base-image metadata loaded fine
// (DONE), so the docker.io mention here is the routine success line, which must not
// be mistaken for a registry failure.
const outputRunApkMirrorTimeout = `#2 [internal] load metadata for docker.io/library/alpine:3.20
#2 DONE 0.3s
#5 [2/2] RUN apk add --no-cache git tmux bash
#5 2.010 fetch https://dl-cdn.alpinelinux.org/alpine/v3.20/main/x86_64/APKINDEX.tar.gz
#5 32.51 WARNING: fetching https://dl-cdn.alpinelinux.org/alpine/v3.20/main: dial tcp 146.75.0.0:443: i/o timeout
#5 ERROR: process "/bin/sh -c apk add --no-cache git tmux bash" did not complete successfully: exit code: 1
ERROR: failed to build: failed to solve: process "/bin/sh -c apk add --no-cache git tmux bash" did not complete successfully: exit code: 1`

// a missing COPY input (the sshd image COPYs entrypoint.sh). A build-context bug;
// no network signature; must fail deterministically.
const outputMissingCopyInput = `#6 [3/5] COPY entrypoint.sh /entrypoint.sh
#6 ERROR: failed to calculate checksum of ref: "/entrypoint.sh": not found
ERROR: failed to build: failed to solve: failed to compute cache key: failed to calculate checksum of ref: "/entrypoint.sh": not found`

func TestDockerBuildRegistryUnreachable_SkipsOnlyRegistryFailures(t *testing.T) {
	skip := []struct {
		name   string
		output string
	}{
		{"base-image i/o timeout (the #2521 signature)", outputRegistryIOTimeout},
		{"registry pull-rate-limit 429", outputRegistryRateLimit},
		{"registry DNS no such host", outputRegistryDNSFailure},
		{"registry TLS handshake timeout", outputRegistryTLSTimeout},
	}
	for _, tc := range skip {
		t.Run("skip/"+tc.name, func(t *testing.T) {
			reason, unreachable := dockerBuildRegistryUnreachable(tc.output)
			if !unreachable {
				t.Fatalf("registry-unreachable output was NOT classified as skippable, so CI would go red on infrastructure:\n%s", tc.output)
			}
			if reason == "" {
				t.Error("a skippable registry failure produced an empty reason for the skip message")
			}
		})
	}

	mustFail := []struct {
		name   string
		output string
	}{
		{"RUN unable to select packages (Dockerfile bug)", outputRunUnableToSelectPackages},
		{"RUN apk MIRROR network timeout (transient but not the registry)", outputRunApkMirrorTimeout},
		{"missing COPY input (build-context bug)", outputMissingCopyInput},
		{"empty output", ""},
		{"a bare successful-looking log", "#2 [internal] load metadata for docker.io/library/alpine:3.20\n#2 DONE 0.2s"},
	}
	for _, tc := range mustFail {
		t.Run("fail/"+tc.name, func(t *testing.T) {
			if reason, unreachable := dockerBuildRegistryUnreachable(tc.output); unreachable {
				t.Fatalf("a NON-registry failure was classified as skippable (%q) — this is exactly the swallowed-real-failure #2521 forbids:\n%s", reason, tc.output)
			}
		})
	}
}

// TestDockerBuildFailover_Policy pins the end-to-end decision the wrapper acts on,
// so retry/skip/fail is proven without needing docker.
func TestDockerBuildFailover_Policy(t *testing.T) {
	tests := []struct {
		name         string
		output       string
		attemptsLeft bool
		want         failoverAction
	}{
		{"registry timeout, attempts left → retry", outputRegistryIOTimeout, true, actionRetry},
		{"registry timeout, exhausted → skip", outputRegistryIOTimeout, false, actionSkipRegistry},
		{"registry 429, exhausted → skip", outputRegistryRateLimit, false, actionSkipRegistry},
		{"apk mirror timeout, attempts left → retry", outputRunApkMirrorTimeout, true, actionRetry},
		{"apk mirror timeout, exhausted → FAIL (not skip)", outputRunApkMirrorTimeout, false, actionFailTransient},
		{"Dockerfile bug, attempts left → fail now (no retry)", outputRunUnableToSelectPackages, true, actionFailDeterministic},
		{"Dockerfile bug, exhausted → fail now", outputRunUnableToSelectPackages, false, actionFailDeterministic},
		{"missing COPY input → fail now", outputMissingCopyInput, true, actionFailDeterministic},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := dockerBuildFailover(tc.output, tc.attemptsLeft); got != tc.want {
				t.Fatalf("dockerBuildFailover = %d, want %d\n%s", got, tc.want, tc.output)
			}
		})
	}
}

// TestDockerBuildLooksTransient_GatesRetry pins that a deterministic failure never
// looks transient — the property that keeps a real bug from being retried into a
// skip.
func TestDockerBuildLooksTransient_GatesRetry(t *testing.T) {
	transient := []string{outputRegistryIOTimeout, outputRegistryRateLimit, outputRegistryDNSFailure, outputRegistryTLSTimeout, outputRunApkMirrorTimeout}
	for _, out := range transient {
		if !dockerBuildLooksTransient(out) {
			t.Errorf("transient output was not recognized as retryable:\n%s", out)
		}
	}
	deterministic := []string{outputRunUnableToSelectPackages, outputMissingCopyInput, "", "some unrelated build log with exit code: 2"}
	for _, out := range deterministic {
		if dockerBuildLooksTransient(out) {
			t.Errorf("deterministic failure looked transient — it would be retried and could be masked:\n%s", out)
		}
	}
}
