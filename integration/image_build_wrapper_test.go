package integration_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// recordingReporter captures the wrapper's terminal decision (Fatalf vs Skipf)
// without failing or skipping the enclosing test, so the retry/skip/fail routing
// can be asserted directly. The wrapper returns after each terminal call, so —
// unlike a real *testing.T — this reporter does not need to abort control flow.
type recordingReporter struct {
	fatal bool
	skip  bool
	msg   string
}

func (r *recordingReporter) Helper() {}
func (r *recordingReporter) Fatalf(format string, args ...any) {
	r.fatal = true
	r.msg = fmt.Sprintf(format, args...)
}
func (r *recordingReporter) Skipf(format string, args ...any) {
	r.skip = true
	r.msg = fmt.Sprintf(format, args...)
}

// scriptedBuild returns a dockerBuildFunc that yields outs[i] on attempt i and
// counts the calls. A nil error entry means that attempt "succeeded".
func scriptedBuild(calls *int, outs ...struct {
	out string
	err error
}) dockerBuildFunc {
	return func(_ context.Context, _, _ string) ([]byte, error) {
		i := *calls
		*calls++
		if i >= len(outs) {
			return []byte("no scripted output for attempt"), errors.New("unexpected extra build attempt")
		}
		return []byte(outs[i].out), outs[i].err
	}
}

// quietSkipAnnotations silences the CI warning annotation so the routing tests do
// not spam `::warning::` lines, and restores it after.
func quietSkipAnnotations(t *testing.T) {
	orig := emitRegistrySkipAnnotation
	emitRegistrySkipAnnotation = func(string) {}
	t.Cleanup(func() { emitRegistrySkipAnnotation = orig })
}

type buildStep = struct {
	out string
	err error
}

func TestBuildDockerImageOrSkip_Routing(t *testing.T) {
	quietSkipAnnotations(t)
	// No real backoff sleeps in the routing test.
	origBackoff := dockerBuildBackoff
	dockerBuildBackoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { dockerBuildBackoff = origBackoff })

	buildErr := errors.New("exit status 1")
	ok := buildStep{"", nil}
	registryFail := buildStep{outputRegistryIOTimeout, buildErr}
	apkMirrorFail := buildStep{outputRunApkMirrorTimeout, buildErr}
	dockerfileBug := buildStep{outputRunUnableToSelectPackages, buildErr}

	t.Run("success on first attempt builds once, no skip/fatal", func(t *testing.T) {
		calls := 0
		r := &recordingReporter{}
		buildDockerImageOrSkipWith(r, scriptedBuild(&calls, ok), &registrySkipState{}, "tag", "dir", "img")
		if calls != 1 {
			t.Errorf("built %d times, want 1", calls)
		}
		if r.fatal || r.skip {
			t.Errorf("a clean build reported fatal=%v skip=%v", r.fatal, r.skip)
		}
	})

	t.Run("registry unreachable every attempt → retries to the cap, then SKIPS", func(t *testing.T) {
		calls := 0
		r := &recordingReporter{}
		buildDockerImageOrSkipWith(r, scriptedBuild(&calls, registryFail, registryFail, registryFail), &registrySkipState{}, "tag", "dir", "img")
		if calls != dockerBuildAttempts {
			t.Errorf("built %d times, want the %d-attempt cap", calls, dockerBuildAttempts)
		}
		if !r.skip || r.fatal {
			t.Errorf("a persistently-unreachable registry did not SKIP (skip=%v fatal=%v): %s", r.skip, r.fatal, r.msg)
		}
	})

	t.Run("registry recovers on a retry → builds, no skip/fatal", func(t *testing.T) {
		calls := 0
		r := &recordingReporter{}
		buildDockerImageOrSkipWith(r, scriptedBuild(&calls, registryFail, ok), &registrySkipState{}, "tag", "dir", "img")
		if calls != 2 {
			t.Errorf("built %d times, want 2 (one blip then success)", calls)
		}
		if r.fatal || r.skip {
			t.Errorf("a recovered build reported fatal=%v skip=%v", r.fatal, r.skip)
		}
	})

	t.Run("Dockerfile bug fails LOUD on the first attempt, no retry, no skip", func(t *testing.T) {
		calls := 0
		r := &recordingReporter{}
		buildDockerImageOrSkipWith(r, scriptedBuild(&calls, dockerfileBug, dockerfileBug, dockerfileBug), &registrySkipState{}, "tag", "dir", "img")
		if calls != 1 {
			t.Errorf("a deterministic build failure was retried %d times; it must fail on the first attempt", calls)
		}
		if !r.fatal || r.skip {
			t.Errorf("a Dockerfile bug did not fail loud (fatal=%v skip=%v): %s", r.fatal, r.skip, r.msg)
		}
	})

	t.Run("transient but NON-registry (apk mirror) every attempt → retries, then FAILS, never skips", func(t *testing.T) {
		calls := 0
		r := &recordingReporter{}
		buildDockerImageOrSkipWith(r, scriptedBuild(&calls, apkMirrorFail, apkMirrorFail, apkMirrorFail), &registrySkipState{}, "tag", "dir", "img")
		if calls != dockerBuildAttempts {
			t.Errorf("built %d times, want the %d-attempt cap", calls, dockerBuildAttempts)
		}
		if !r.fatal || r.skip {
			t.Errorf("an exhausted non-registry transient must FAIL not skip (fatal=%v skip=%v): %s", r.fatal, r.skip, r.msg)
		}
	})
}

// TestBuildDockerImageOrSkip_MemoizesRegistryVerdict is #2521 P2-b: once one build
// finds the registry unreachable, the next build sharing the package memo skips
// AT ONCE (zero attempts) rather than re-proving a durable outage against the
// package timeout.
func TestBuildDockerImageOrSkip_MemoizesRegistryVerdict(t *testing.T) {
	quietSkipAnnotations(t)
	origBackoff := dockerBuildBackoff
	dockerBuildBackoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { dockerBuildBackoff = origBackoff })

	buildErr := errors.New("exit status 1")
	registryFail := buildStep{outputRegistryIOTimeout, buildErr}
	shared := &registrySkipState{}

	first := 0
	r1 := &recordingReporter{}
	buildDockerImageOrSkipWith(r1, scriptedBuild(&first, registryFail, registryFail, registryFail), shared, "tag", "dir", "first")
	if first != dockerBuildAttempts || !r1.skip {
		t.Fatalf("first build: attempts=%d skip=%v, want %d and a skip", first, r1.skip, dockerBuildAttempts)
	}

	second := 0
	r2 := &recordingReporter{}
	buildDockerImageOrSkipWith(r2, scriptedBuild(&second, registryFail), shared, "tag", "dir", "second")
	if second != 0 {
		t.Errorf("second build attempted the registry %d times; the memoized verdict must skip it with ZERO attempts (#2521 P2-b)", second)
	}
	if !r2.skip || r2.fatal {
		t.Errorf("second build did not skip immediately (skip=%v fatal=%v)", r2.skip, r2.fatal)
	}
}

// TestBuildDockerImageOrSkip_TotalBudget is #2521 P2-b: a zero total budget stops
// after the first attempt instead of running the full retry cap, so one build
// cannot blow the package timeout — while still routing to skip (registry) vs fail
// (non-registry) on that one attempt's output.
func TestBuildDockerImageOrSkip_TotalBudget(t *testing.T) {
	quietSkipAnnotations(t)
	origBudget := dockerBuildTotalBudget
	dockerBuildTotalBudget = 0
	t.Cleanup(func() { dockerBuildTotalBudget = origBudget })

	buildErr := errors.New("exit status 1")

	t.Run("registry unreachable, budget spent → one attempt then SKIP", func(t *testing.T) {
		calls := 0
		r := &recordingReporter{}
		reg := buildStep{outputRegistryIOTimeout, buildErr}
		buildDockerImageOrSkipWith(r, scriptedBuild(&calls, reg, reg, reg), &registrySkipState{}, "tag", "dir", "img")
		if calls != 1 {
			t.Errorf("the total budget did not cap retries: %d attempts, want 1", calls)
		}
		if !r.skip || r.fatal {
			t.Errorf("budget-exhausted registry outage must skip (skip=%v fatal=%v): %s", r.skip, r.fatal, r.msg)
		}
	})

	t.Run("non-registry transient, budget spent → one attempt then FAIL", func(t *testing.T) {
		calls := 0
		r := &recordingReporter{}
		apk := buildStep{outputApkMirrorRateLimit, buildErr}
		buildDockerImageOrSkipWith(r, scriptedBuild(&calls, apk, apk, apk), &registrySkipState{}, "tag", "dir", "img")
		if calls != 1 {
			t.Errorf("the total budget did not cap retries: %d attempts, want 1", calls)
		}
		if !r.fatal || r.skip {
			t.Errorf("budget-exhausted non-registry transient must FAIL not skip (fatal=%v skip=%v): %s", r.fatal, r.skip, r.msg)
		}
	})
}
