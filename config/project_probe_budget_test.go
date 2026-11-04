package config

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveConfigAppliesPersonalLayerPastAProbeSliceTimeout is the #6746
// regression. A checkout-marker probe that outlives only its own 250ms slice —
// routine on a loaded box at daemon startup — failed the personal lookup and
// the lookup silently degraded to "no personal layer", so per-project
// default_program/program_overrides/root_agent went inert with one warn-once
// line. A lapsed slice inside a still-live scan budget failed an ATTEMPT, not
// the lookup: slow is not absent.
func TestResolveConfigAppliesPersonalLayerPastAProbeSliceTimeout(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "default_program = \"codex\"\n")
	writePersonalConfig(t, project.ID, "default_program = \"gemini\"\n")

	// Every git invocation — the checkout-marker location probe included —
	// sleeps past the 250ms probe slice but well inside the 1s scan, the same
	// shape the three observed warnings had. The first attempt dies to its
	// own clock; the caller's budget is still live.
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	installFakeGit(t, "#!/bin/sh\nsleep 0.3\nexec \""+realGit+"\" \"$@\"\n")

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err, "a probe that outlives only its slice must be re-asked, not degraded")
	assert.Equal(t, "gemini", resolved.DefaultProgram,
		"the configured personal layer must apply: a transient probe timeout is not absence")
}

// TestResolveConfigPropagatesAScanDeadlinedProbe is the visibility half of
// #6746: when the caller's whole budget is spent and the probe still has not
// answered, the personal lookup is UNKNOWN — degrading it to "no personal
// layer" fabricates an absence the registry never stated and resolves the
// user's sessions on the wrong program. The resolve must fail with the probe
// error instead, so the caller retries or reports it.
func TestResolveConfigPropagatesAScanDeadlinedProbe(t *testing.T) {
	_, repoRoot, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "default_program = \"gemini\"\n")

	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	installFakeGit(t, "#!/bin/sh\nsleep 3\nexec \""+realGit+"\" \"$@\"\n")

	resolved, err := ResolveConfig(repoRoot)
	require.Error(t, err, "a probe unanswered within the scan is unknown, never 'no personal layer'")
	require.Nil(t, resolved)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// TestResolveConfigAwaitsAParkedMarkerRead covers the third #6746 shape: the
// checkout-marker os.ReadFile that outlives the probe slice is parked
// uncancellable in its singleflight — the deadline ends the WAIT, never the
// read. While the caller's scan budget is live the resolve must spend it on
// the answer already in flight rather than reporting the read dead and
// degrading the layer.
func TestResolveConfigAwaitsAParkedMarkerRead(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "default_program = \"codex\"\n")
	writePersonalConfig(t, project.ID, "default_program = \"gemini\"\n")

	markerName, err := checkoutMarkerName()
	require.NoError(t, err)
	marker := filepath.Join(repoRoot, ".git", checkoutMarkerDirName, markerName)
	markerData, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.NoError(t, os.Remove(marker))
	require.NoError(t, syscall.Mkfifo(marker, 0o600))

	var releaseOnce sync.Once
	releaseDone := make(chan error, 1)
	released := false
	release := func() {
		releaseOnce.Do(func() {
			go func() { releaseDone <- os.WriteFile(marker, markerData, 0o600) }()
		})
	}
	t.Cleanup(func() {
		if !released {
			release()
			<-releaseDone
		}
		_ = os.Remove(marker)
		_ = os.WriteFile(marker, markerData, 0o600)
	})

	// The parked read becomes answerable past the probe slice but well inside
	// the scan: the same margin the observed flap needed.
	go func() {
		time.Sleep(400 * time.Millisecond)
		release()
	}()

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, <-releaseDone)
	released = true
	require.NoError(t, err, "a marker read that answers inside the scan budget must be used, not abandoned")
	assert.Equal(t, "gemini", resolved.DefaultProgram,
		"the configured personal layer must apply: a slow read is not an absent marker")
}

// TestResolveConfigPropagatesWhenTheGitEntryStatOutlivesTheScan covers the
// fallback's own wedge shape: the .git existence check that disproves a
// checkout after a failed probe is parked like the marker read, because a
// workspace on an unresponsive filesystem stalls a synchronous os.Lstat just
// as dead as the probe itself. When that observation cannot answer inside
// the caller's budget the lookup stays unknown and propagates — it must not
// hang the resolve and must not fabricate "no checkout marker" either.
func TestResolveConfigPropagatesWhenTheGitEntryStatOutlivesTheScan(t *testing.T) {
	_, repoRoot, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "default_program = \"gemini\"\n")

	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	installFakeGit(t, "#!/bin/sh\nsleep 3\nexec \""+realGit+"\" \"$@\"\n")

	// The hook parks the stat goroutine past the scan deadline — the shape a
	// dead mount gives the lstat itself.
	entered := make(chan struct{})
	release := make(chan struct{})
	workspaceGitEntryHookForTest = func() {
		close(entered)
		<-release
	}
	t.Cleanup(func() {
		close(release)
		workspaceGitEntryHookForTest = nil
	})

	resolved, err := ResolveConfig(repoRoot)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the .git observation never ran")
	}
	require.Error(t, err, "a .git observation that cannot answer inside the budget must propagate, not hang")
	require.Nil(t, resolved)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
