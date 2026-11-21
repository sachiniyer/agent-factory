package session

// Regression tests for the bare-shorthand run_args bypass on the staging-walker
// path (the bug report's evidence table): a bare two-character value-taking
// short option (-l, -h, -w, -p, -u, -m, -a, -c, -e) consumes the NEXT argv
// element as its value under pflag, so a following `-v`/`--mount`/`--tmpfs` is
// swallowed and Docker installs no mount at that position. The bind-delivery
// tag recorder (runArgsMountDestinations) skipped the bare shorthand and
// re-read the swallowed `-v` as a fresh volume spec, recording a destination
// Docker never installs and over-refusing a valid config (turning the cp
// fallback on for a repo that never shadows the af staging path). These tests
// pin the fix that consumes the swallowed value so the walker records only
// REAL mounts, and that a real mount written AFTER the consumed value is
// still examined on its own.
//
// Measured: `docker run --rm -l -v /tmp/x:/y busybox mount` records neither
// the swallowed -v nor any mount at /y. The walker must mirror that.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDockerStagingRunArgsMountDestinations_BareValueShorthandConsumesGuardedToken
// is the staging-walker half of the bare-shorthand bypass fix. Under pflag, a
// bare value-taking short option consumes the NEXT argv element as its value,
// so a token like `-l` swallows the following `-v` and Docker installs no
// mount at that position. The buggy walker skipped the bare shorthand
// entirely and re-read the swallowed `-v` as a fresh volume spec, recording a
// destination Docker never installs and over-refusing a valid config; the
// cluster form (`-ie` then `-v`) had the same blind spot. The fix consumes the
// swallowed value so the walker records only REAL mounts.
//
// Measured: `docker run --rm -l -v /tmp/x:/y busybox mount` records neither
// the swallowed -v nor any mount at /y. The walker must mirror that.
func TestDockerStagingRunArgsMountDestinations_BareValueShorthandConsumesGuardedToken(t *testing.T) {
	// A bare value-taking shorthand swallows the following -v/--mount/-v=path;
	// the swallowed token installs no mount, so runArgsMountDestinations must
	// record NOTHING for it (the over-refusal direction of the bypass).
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "bare -l eats -v", args: []string{"-l", "-v", "/host:/usr/local/bin"}},
		{name: "bare -h eats -v", args: []string{"-h", "-v", "/host:/usr/local/bin"}},
		{name: "bare -w eats -v", args: []string{"-w", "-v", "/host:/usr/local/bin"}},
		{name: "bare -p eats -v", args: []string{"-p", "-v", "/host:/usr/local/bin"}},
		{name: "bare -u eats -v", args: []string{"-u", "-v", "/host:/usr/local/bin"}},
		{name: "bare -m eats -v", args: []string{"-m", "-v", "/host:/usr/local/bin"}},
		{name: "bare -a eats -v", args: []string{"-a", "-v", "/host:/usr/local/bin"}},
		{name: "bare -c eats -v", args: []string{"-c", "-v", "/host:/usr/local/bin"}},
		{name: "bare -e eats -v", args: []string{"-e", "-v", "/host:/usr/local/bin"}},
		{name: "bare -l eats --mount", args: []string{"-l", "--mount", "type=bind,src=/host,dst=/usr/local/bin"}},
		{name: "bare -l eats --tmpfs", args: []string{"-l", "--tmpfs", "/usr/local/bin"}},
		{name: "bare -l eats -v=path", args: []string{"-l", "-v=/host:/usr/local/bin"}},
		{name: "cluster -ie eats -v", args: []string{"-ie", "-v", "/host:/usr/local/bin"}},
		{name: "cluster -te eats -v", args: []string{"-te", "-v", "/host:/usr/local/bin"}},
		{name: "cluster -il eats -v", args: []string{"-il", "-v", "/host:/usr/local/bin"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mounts, err := runArgsMountDestinations(tt.args)
			require.NoError(t, err)
			for _, m := range mounts {
				require.NotContainsf(t, m[1], "/usr/local/bin",
					"the swallowed -v/--mount installs no mount, so no destination Docker never installs may be recorded: %v", mounts)
			}
			// The bare shorthand consumed its value, so a staging destination
			// is recorded only if the real mount lands elsewhere. In every
			// case above the real -v/--mount is consumed, so no enclosing
			// destination is recorded at all.
			require.Emptyf(t, mounts,
				"a guarded mount token swallowed by a bare/cluster value-taking shorthand installs nothing and must not be recorded: %v", mounts)
		})
	}
}

// TestDockerStagingRunArgsMountDestinations_StillRecordsRealMountsAfterBareShorthand
// is the no-regression half: a bare value-taking shorthand consumes its value,
// but a REAL mount written AFTER that value is still examined on its own and
// recorded. The fix consumes only the swallowed value, never the real mount
// that follows it.
func TestDockerStagingRunArgsMountDestinations_StillRecordsRealMountsAfterBareShorthand(t *testing.T) {
	// -l consumes -v (its label value), and the real --mount after it
	// is a fresh mount Docker honors and the walker must record.
	mounts, err := runArgsMountDestinations([]string{"-l", "-v", "--mount", "type=bind,src=/host,dst=/usr/local/bin"})
	require.NoError(t, err)
	require.Len(t, mounts, 1)
	require.Equal(t, "/usr/local/bin", mounts[0][1])

	// A real -v after a bare -l that consumed a non-mount value is recorded.
	mounts, err = runArgsMountDestinations([]string{"-l", "labelval", "-v", "/host:/usr/local/bin"})
	require.NoError(t, err)
	require.Len(t, mounts, 1)
	require.Equal(t, "/usr/local/bin", mounts[0][1])
}
