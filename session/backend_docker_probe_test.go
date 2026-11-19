// Tests for the in-image mount-destination resolution probe (#7601). The
// fake `docker run --rm` path runs the probe's -c script in a REAL /bin/sh —
// dash on the CI lanes, the shell family the bug report names — against a
// fixture tree, so the resolved answers are the script's actual semantics,
// not a canned echo. Two properties:
//
//   - a destination missing from the image two or more levels deep is judged
//     from its deepest existing ancestor (the unresolved tail is appended to
//     the ancestor's resolved path);
//   - each destination resolves independently of the previous one's working
//     directory — a single-component missing destination like /data must not
//     inherit the cwd a prior `cd` left behind (dash and busybox treat
//     `cd -- ""` as a successful no-op, so an empty parent probe resolves
//     against whatever directory the loop was last in).
package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// probeScriptThroughRealSh answers a `docker run --rm … -c <script>` call by
// executing the script in the host's /bin/sh with the same positional args
// production passes. The fixture tree stands in for the image's filesystem.
// The deadline keeps a script that cannot terminate (a relative destination
// the walk cannot shrink, say) from hanging the test instead of failing it.
func probeScriptThroughRealSh(t *testing.T, args []string) ([]byte, error) {
	t.Helper()
	for i, a := range args {
		if a == "-c" && i+2 < len(args) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/sh", append([]string{"-c", args[i+1]}, args[i+2:]...)...)
			return cmd.Output()
		}
	}
	t.Fatalf("probe argv carries no -c script: %v", args)
	return nil, nil
}

// probeDestsThroughRealSh installs the docker seam whose --rm run executes the
// probe script in /bin/sh, then calls probeResolvedDestinations for dests.
func probeDestsThroughRealSh(t *testing.T, dests ...string) ([]string, error) {
	t.Helper()
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if args[0] == "run" {
			return probeScriptThroughRealSh(t, args)
		}
		return nil, nil
	}))
	mounts := make([][2]string, len(dests))
	for i, d := range dests {
		mounts[i] = [2]string{"-v /h/src:" + d, d}
	}
	p := &dockerProvisioner{image: "img:latest"}
	_, resMounts, _, err := p.probeResolvedDestinations(mounts)
	return resMounts, err
}

// TestDockerDestProbeResolvesFromDeepestExistingAncestor pins #7601's first
// property: a mount destination missing several levels deep resolves to its
// deepest existing ancestor's real path plus the unresolved tail, instead of
// failing the probe into the silent cp fallback. A symlinked ancestor must
// resolve onto its target.
func TestDockerDestProbeResolvesFromDeepestExistingAncestor(t *testing.T) {
	fix, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(fix, "home", "dev"), 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(fix, "opt", "real"), 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(fix, "exists"), 0755))
	require.NoError(t, os.Symlink(filepath.Join(fix, "opt", "real"), filepath.Join(fix, "link")))

	require.NoError(t, os.WriteFile(filepath.Join(fix, "file"), []byte("x"), 0644))

	res, err := probeDestsThroughRealSh(t,
		// /home/dev exists; .cache/pip is missing two levels deep.
		filepath.Join(fix, "home", "dev", ".cache", "pip"),
		// The deepest existing ancestor is itself a symlink — the answer
		// lands on its resolved target.
		filepath.Join(fix, "link", "deep", "miss"),
		// Nothing under this exists at all — the ancestor is the fixture
		// root, so the dest resolves to itself.
		filepath.Join(fix, "a", "b", "c"),
		// A non-directory mid component is ENOTDIR — proven "not a dir" —
		// so the walk may step up past it.
		filepath.Join(fix, "file", "miss"),
		// A fully present destination resolves itself (control).
		filepath.Join(fix, "exists"),
	)
	require.NoError(t, err)
	require.Len(t, res, 5)
	assert.Equal(t, filepath.Join(fix, "home", "dev", ".cache", "pip"), res[0],
		"the unresolved tail is appended to the deepest existing ancestor")
	assert.Equal(t, filepath.Join(fix, "opt", "real", "deep", "miss"), res[1],
		"the ancestor is resolved with pwd -P — symlink hops land on the target")
	assert.Equal(t, filepath.Join(fix, "a", "b", "c"), res[2])
	assert.Equal(t, filepath.Join(fix, "file", "miss"), res[3],
		"a regular file mid-path is a proven non-directory — the walk steps up")
	assert.Equal(t, filepath.Join(fix, "exists"), res[4])
}

// TestDockerDestProbeResolvesEachDestinationIndependently pins the second
// property: a destination's resolution never depends on the working directory
// a previous destination's probe left behind. The canonical case is a
// single-component destination (/x) whose "${d%/*}" parent probe is the empty
// string — `cd -- ""` is a successful no-op on dash and busybox, so pwd -P
// answers the previous destination's directory.
func TestDockerDestProbeResolvesEachDestinationIndependently(t *testing.T) {
	fix, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(fix, "first"), 0755))
	missing := "/af-probe-missing-7601"
	require.NoFileExists(t, missing, "test fixture needs a destination that cannot exist on the host")

	res, err := probeDestsThroughRealSh(t,
		// Succeeds and leaves the probe's cwd inside the fixture.
		filepath.Join(fix, "first"),
		// Single-component dest: its parent probe is "". Resolution must
		// land on / itself, never on first's directory.
		missing,
		// A later ordinary miss must still resolve on its own terms.
		filepath.Join(fix, "gone"),
	)
	require.NoError(t, err)
	require.Len(t, res, 3)
	assert.Equal(t, missing, res[1],
		"/x resolves against /, not the directory the previous cd left behind")
	assert.Equal(t, filepath.Join(fix, "gone"), res[2])
}

// TestDockerDestProbeUnverifiableComponentsFailClosed pins the fail-closed
// half of #7601's rule: the walk may step up only past a component it PROVES
// absent or non-directory. An inaccessible directory (EACCES), a symlink the
// probe cannot traverse (ELOOP/dangling), and a destination that cannot
// shrink (a relative path) are all unverifiable — the probe fails them, and
// the provisioner takes the guarded cp fallback rather than resolving from an
// ancestor it cannot see inside.
func TestDockerDestProbeUnverifiableComponentsFailClosed(t *testing.T) {
	fix, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	locked := filepath.Join(fix, "locked")
	require.NoError(t, os.MkdirAll(locked, 0755))
	require.NoError(t, os.Chmod(locked, 0000))
	t.Cleanup(func() { os.Chmod(locked, 0755) })
	require.NoError(t, os.Symlink(filepath.Join(fix, "loop"), filepath.Join(fix, "loop")))
	dangling := filepath.Join(fix, "dangling")
	require.NoError(t, os.Symlink(filepath.Join(fix, "gone-target"), dangling))

	for _, tc := range []struct {
		name      string
		dest      string
		needsPerm bool // mode-000 must actually deny — meaningless as root
	}{
		// Codex's exact P1 shape: /locked is EACCES, so the probe cannot
		// prove what /locked/deep IS — a symlink retargeting a mount could
		// hide inside. Resolved-from-ancestor would let it slip the guard.
		{"unreadable intermediate directory", filepath.Join(locked, "deep"), true},
		// The same, as the destination's own leaf.
		{"unreadable directory as leaf", locked, true},
		// A symlink loop the probe cannot traverse — ELOOP, not ENOENT.
		{"symlink loop intermediate", filepath.Join(fix, "loop", "deep"), false},
		// A dangling link the probe cannot traverse — the component exists
		// but is not proven absent.
		{"dangling symlink as leaf", dangling, false},
		// A relative destination cannot shrink — the walk must terminate
		// rather than loop forever.
		{"relative destination", "af-nonexistent-relative-7601/subdir", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.needsPerm && os.Geteuid() == 0 {
				t.Skip("root reads a mode-000 directory")
			}
			_, err := probeDestsThroughRealSh(t, tc.dest)
			require.Error(t, err, "an unverifiable destination must fail the probe, not resolve from an ancestor")
			assert.Contains(t, err.Error(), "cannot be resolved inside the image")
		})
	}
}

// TestDockerStagingDeepMissingMountDestKeepsBinds is the behavior consequence
// at provision level: a run_args mount whose destination the old probe could
// not resolve silently selected cp delivery. Resolved from the deepest
// ancestor, the same mount is judged like any other and binds stay.
func TestDockerStagingDeepMissingMountDestKeepsBinds(t *testing.T) {
	stagingTestEnv(t)
	var calls [][]string
	old := dockerStagingProbeArgs
	dockerStagingProbeArgs = func(args []string) ([]byte, error) {
		return probeScriptThroughRealSh(t, args)
	}
	defer func() { dockerStagingProbeArgs = old }()
	t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))
	p := &dockerProvisioner{
		image:   "img:latest",
		afBin:   stagingBinarySource(t),
		runArgs: []string{"-v", "/host/src:/home/dev/.cache/pip"},
		spec:    ProvisionSpec{Title: "deep", CloneURL: "file:///repo"},
	}
	_, err := p.provision()
	require.NoError(t, err, "calls=%v", calls)
	assert.True(t, p.payloadViaBinds,
		"a dest resolvable from its deepest ancestor no longer selects the cp fallback")
	require.NotNil(t, findDockerCall(calls, "run", "--rm"), "the resolution probe ran: calls=%v", calls)
}

// TestDockerStagingUnverifiableMountDestFallsBackToCp is the fail-closed half
// at provision level: a run_args mount whose destination sits under a
// component the probe cannot inspect (here an unreadable directory) fails the
// probe and takes the guarded cp fallback — it never resolves from an
// ancestor the probe could not see inside.
func TestDockerStagingUnverifiableMountDestFallsBackToCp(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 directory")
	}
	stagingTestEnv(t)
	fix, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	locked := filepath.Join(fix, "locked")
	require.NoError(t, os.MkdirAll(locked, 0755))
	require.NoError(t, os.Chmod(locked, 0000))
	t.Cleanup(func() { os.Chmod(locked, 0755) })
	var calls [][]string
	old := dockerStagingProbeArgs
	dockerStagingProbeArgs = func(args []string) ([]byte, error) {
		return probeScriptThroughRealSh(t, args)
	}
	defer func() { dockerStagingProbeArgs = old }()
	t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))
	p := &dockerProvisioner{
		image:   "img:latest",
		afBin:   stagingBinarySource(t),
		runArgs: []string{"-v", "/host/src:" + filepath.Join(locked, "deep")},
		spec:    ProvisionSpec{Title: "unv", CloneURL: "file:///repo"},
	}
	_, err = p.provision()
	require.NoError(t, err, "calls=%v", calls)
	assert.False(t, p.payloadViaBinds,
		"an unverifiable destination yields binds to the guarded cp fallback")
	require.NotNil(t, findDockerCall(calls, "cp"), "calls=%v", calls)
}
