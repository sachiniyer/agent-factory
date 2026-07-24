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

func TestBuildDockerImageOrSkip_Routing(t *testing.T) {
	// No real backoff sleeps in the routing test.
	orig := dockerBuildBackoff
	dockerBuildBackoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { dockerBuildBackoff = orig })

	buildErr := errors.New("exit status 1")
	ok := struct {
		out string
		err error
	}{"", nil}
	registryFail := struct {
		out string
		err error
	}{outputRegistryIOTimeout, buildErr}
	apkMirrorFail := struct {
		out string
		err error
	}{outputRunApkMirrorTimeout, buildErr}
	dockerfileBug := struct {
		out string
		err error
	}{outputRunUnableToSelectPackages, buildErr}

	t.Run("success on first attempt builds once, no skip/fatal", func(t *testing.T) {
		calls := 0
		r := &recordingReporter{}
		buildDockerImageOrSkipWith(r, scriptedBuild(&calls, ok), "tag", "dir", "img")
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
		buildDockerImageOrSkipWith(r, scriptedBuild(&calls, registryFail, registryFail, registryFail), "tag", "dir", "img")
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
		buildDockerImageOrSkipWith(r, scriptedBuild(&calls, registryFail, ok), "tag", "dir", "img")
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
		buildDockerImageOrSkipWith(r, scriptedBuild(&calls, dockerfileBug, dockerfileBug, dockerfileBug), "tag", "dir", "img")
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
		buildDockerImageOrSkipWith(r, scriptedBuild(&calls, apkMirrorFail, apkMirrorFail, apkMirrorFail), "tag", "dir", "img")
		if calls != dockerBuildAttempts {
			t.Errorf("built %d times, want the %d-attempt cap", calls, dockerBuildAttempts)
		}
		if !r.fatal || r.skip {
			t.Errorf("an exhausted non-registry transient must FAIL not skip (fatal=%v skip=%v): %s", r.fatal, r.skip, r.msg)
		}
	})
}
