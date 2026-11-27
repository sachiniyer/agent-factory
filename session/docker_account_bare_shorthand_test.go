package session

// Regression tests for the bare-shorthand run_args bypass on the account path
// (the bug report's evidence table): a bare two-character value-taking short
// option (-l, -h, -w, -p, -u, -m, -a, -c, -e) consumes the NEXT argv element
// as its value under pflag, so a following `-e` is swallowed and the real
// `--env=NAME=...` behind it is the env Docker honors. The account-path walker
// (validateAccountDockerRunArgs) skipped the bare shorthand and re-read the
// swallowed `-e` as a fresh env flag, checking the phantom name `--env`
// strings.Cut left and letting an identity env through unguarded. The same
// shape defeated the -v mount guard. These tests pin the fix that consumes
// the swallowed value so the outer walk examines the REAL flag behind it.
//
// Also pins the shared helper dockerClusterConsumesNextArg (used by all three
// walkers) against the pflag-correct boundary.
//
// Measured on Docker 26.1.5: `docker run --rm -l -e --env=AFPROBE_GH=zzz
// busybox env` sets AFPROBE_GH=zzz, so the sneaked flag is honored and the
// account guard MUST refuse it.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDockerClusterConsumesNextArg pins the helper the bare-shorthand fix
// shares across all three run_args walkers. It mirrors dockerShorthandValue's
// own pflag rule ("the rest of the cluster, or else the next argument") and
// answers whether the outer walk must skip the NEXT argv element a combined
// short option swallows as its trailing option's value. The cases are the
// pflag-correct boundary: a cluster ending in a value-taking option with an
// empty remainder consumes the next arg; an inline value (`-fvalue`, `-f=value`)
// and an all-boolean cluster do not; an unknown shorthand does NOT consume
// (the fail-closed side, matching dockerCheckShorthandCluster's posture for a
// guarded char hiding behind an unknown one).
func TestDockerClusterConsumesNextArg(t *testing.T) {
	for _, tt := range []struct {
		name string
		arg  string
		want bool
	}{
		// A cluster ending in a value-taking option with an empty remainder
		// sends pflag to the next argument.
		{name: "-ie (env trailing)", arg: "-ie", want: true},
		{name: "-te (env trailing)", arg: "-te", want: true},
		{name: "-tv (volume trailing)", arg: "-tv", want: true},
		{name: "-il (label trailing)", arg: "-il", want: true},
		{name: "-tl (label trailing)", arg: "-tl", want: true},
		{name: "-idPqt before a value-taking (consumes)", arg: "-idPqtv", want: true},
		// An inline value consumes the rest of the cluster, not the next arg.
		{name: "-tvpath (inline value, no consume)", arg: "-tv/tmp:/x", want: false},
		{name: "-ieNAME=env (inline value, no consume)", arg: "-ieCODEX_HOME=/x", want: false},
		// The `-f=value` form: pflag resolves the `=` BEFORE consulting the
		// option kind, so the suffix is the option's value and the cluster
		// ends without touching the next argument.
		{name: "-tv=path (equals form, no consume)", arg: "-tv=/tmp:/x", want: false},
		{name: "-te=NAME=env (equals form, no consume)", arg: "-te=CODEX_HOME=/x", want: false},
		{name: "-t=false (boolean equals form, no consume)", arg: "-t=false", want: false},
		// An all-boolean cluster consumes nothing.
		{name: "-it (all boolean, no consume)", arg: "-it", want: false},
		{name: "-idP (all boolean, no consume)", arg: "-idP", want: false},
		{name: "-itd (all boolean, no consume)", arg: "-itd", want: false},
		// An unknown shorthand is ambiguous: af cannot prove pflag consumed
		// the next arg as its value (it may be a boolean), so the helper does
		// NOT consume — matching dockerCheckShorthandCluster's fail-closed
		// posture for a guarded char hiding behind an unknown one.
		{name: "-Xv (unknown then volume, no consume)", arg: "-Xv", want: false},
		{name: "-itY (unknown after booleans, no consume)", arg: "-itY", want: false},
		{name: "-Ye (unknown before env, no consume)", arg: "-Ye", want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equalf(t, tt.want, dockerClusterConsumesNextArg(tt.arg),
				"dockerClusterConsumesNextArg(%q) = %v, want %v", tt.arg, !tt.want, tt.want)
		})
	}
}

// TestDockerAccount_RefusesIdentityEnvBehindBareValueShorthand is the
// account-path regression from the bare-shorthand bypass report. Under pflag
// (the parser `docker run` uses), a bare value-taking short option consumes
// the NEXT argv element as its value, so a token like `-l` swallows the
// following `-e` and the real `--env=NAME=...` behind it is the env Docker
// honors. The buggy walk skipped the bare shorthand entirely, re-read the
// swallowed `-e` as a fresh env flag, checked the phantom name `--env` (and
// `--mount`) strings.Cut left, and let an identity env through unguarded.
//
// Measured on Docker 26.1.5: `docker run --rm -l -e --env=AFPROBE_GH=zzz busybox
// env` sets AFPROBE_GH=zzz — the sneaked flag is honored, so the account guard
// MUST refuse it. The denied names below are the ones
// accountDockerDeniedNames("claude") actually holds (CLAUDE_CONFIG_DIR is the
// config var; ANTHROPIC_API_KEY/ANTHROPIC_AUTH_TOKEN/CLAUDE_CODE_OAUTH_TOKEN are
// the auth selectors) plus the universal program-choice names
// (PATH/LD_PRELOAD/LD_LIBRARY_PATH, refused via accountDockerProgramChoiceReason
// for every agent).
func TestDockerAccount_RefusesIdentityEnvBehindBareValueShorthand(t *testing.T) {
	for _, tt := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "label eats -e then --env=CLAUDE_CONFIG_DIR", args: []string{"-l", "-e", "--env=CLAUDE_CONFIG_DIR=/x"}, wantErr: "CLAUDE_CONFIG_DIR"},
		{name: "hostname eats -e then --env=ANTHROPIC_API_KEY", args: []string{"-h", "-e", "--env=ANTHROPIC_API_KEY=repo"}, wantErr: "ANTHROPIC_API_KEY"},
		{name: "workdir eats -e then --env=ANTHROPIC_AUTH_TOKEN", args: []string{"-w", "-e", "--env=ANTHROPIC_AUTH_TOKEN=repo"}, wantErr: "ANTHROPIC_AUTH_TOKEN"},
		{name: "user eats -e then --env=CLAUDE_CODE_OAUTH_TOKEN", args: []string{"-u", "-e", "--env=CLAUDE_CODE_OAUTH_TOKEN=repo"}, wantErr: "CLAUDE_CODE_OAUTH_TOKEN"},
		{name: "memory eats -e then --env=CLAUDE_CONFIG_DIR", args: []string{"-m", "-e", "--env=CLAUDE_CONFIG_DIR=/x"}, wantErr: "CLAUDE_CONFIG_DIR"},
		{name: "publish eats -e then --env=ANTHROPIC_API_KEY", args: []string{"-p", "-e", "--env=ANTHROPIC_API_KEY=repo"}, wantErr: "ANTHROPIC_API_KEY"},
		// The program-choice / loader names (PATH, LD_*) are refused on the
		// account path too (accountDockerProgramChoiceReason); the same
		// bare-shorthand shape defeats them without the fix.
		{name: "label eats -e then --env=PATH", args: []string{"-l", "-e", "--env=PATH=/workspace/.x"}, wantErr: "PATH"},
		{name: "label eats -e then --env=LD_PRELOAD", args: []string{"-l", "-e", "--env=LD_PRELOAD=/workspace/.git/evil.so"}, wantErr: "LD_PRELOAD"},
		{name: "label eats -e then --env=LD_LIBRARY_PATH", args: []string{"-l", "-e", "--env=LD_LIBRARY_PATH=/workspace/.git/evil"}, wantErr: "LD_LIBRARY_PATH"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAccountDockerRunArgs(tt.args, "claude", nil)
			require.Errorf(t, err, "an identity env sneaked behind a bare value-taking shorthand must be refused: %v", tt.args)
			require.Contains(t, err.Error(), tt.wantErr,
				"the refusal must name the REAL denied variable, not the phantom --env")
			require.NotContains(t, err.Error(), "--env",
				"the refusal must not echo the phantom name the buggy walk inspected")
		})
	}
}

// TestDockerAccount_RefusesMountBehindBareValueShorthand is the -v half of the
// same bypass. A bare value-taking short option that swallows a following -v
// re-reads the real `--mount`/`--volume`/`-v` spec behind it as a fresh mount,
// but without the fix it SKIPPED the bare shorthand and re-read the swallowed
// -v as a fresh mount. The real regression here is the over-refusal /
// under-refusal pair: a -v swallowed by -l installs no mount, so the bare -v
// case must NOT be re-read as a fresh mount and must not refuse; a --mount
// behind the swallowed -v must still be examined on its own and refused.
func TestDockerAccount_RefusesMountBehindBareValueShorthand(t *testing.T) {
	// -l swallows -v: -v is the label value and installs NO mount. The guard
	// must NOT refuse this (over-refusal): Docker honors no mount here. The
	// fixture stops at the swallowed token — a bare spec after it would be a
	// positional, refused for its own reason (#7733 R1).
	require.NoErrorf(t, validateAccountDockerRunArgs([]string{"-l", "-v"}, "codex", nil),
		"a -v swallowed by a bare -l is the label's value and installs no mount")

	// -v swallows the next arg as its value: a following --mount is NOT a fresh
	// mount (it is -v's value), so no mount lands and the guard must not refuse.
	require.NoErrorf(t, validateAccountDockerRunArgs([]string{"-v", "--mount"}, "codex", nil),
		"a --mount swallowed by a bare -v is -v's value, not a fresh mount")

	// -h swallows --mount: --mount is -h's value and installs no mount.
	require.NoErrorf(t, validateAccountDockerRunArgs([]string{"-h", "--mount"}, "codex", nil),
		"a --mount swallowed by a bare -h installs no mount")

	// A REAL --mount written AFTER the bare shorthand's consumed value must
	// still be examined on its own and refused. -l consumes -v (its value);
	// the --mount after that is a fresh mount Docker honors and the guard
	// must catch it. Before the fix, -l was skipped and -v re-read as a mount
	// (refusing the wrong spec); after the fix, -v is consumed as -l's value
	// and the real --mount is examined and refused.
	require.Errorf(t, validateAccountDockerRunArgs([]string{"-l", "-v", "--mount", "type=bind,src=/tmp/other,dst=/af-account"}, "codex", nil),
		"a real --mount after a bare shorthand's consumed value must be examined and refused")
}

// TestDockerAccount_RefusesIdentityEnvBehindClusterValueShorthand is the
// cluster sibling of the bare-shorthand bypass. A combined short option ending
// in a value-taking option consumes the NEXT argv element as that option's
// value, so `-ie` swallows the following `-e` and the real `--env=NAME=...`
// behind it is the env Docker honors (measured: `docker run --rm -ie -e
// --env=AFPROBE_GH=zzz busybox env` sets AFPROBE_GH=zzz).
func TestDockerAccount_RefusesIdentityEnvBehindClusterValueShorthand(t *testing.T) {
	for _, tt := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "-ie eats -e then --env=CLAUDE_CONFIG_DIR", args: []string{"-ie", "-e", "--env=CLAUDE_CONFIG_DIR=/x"}, wantErr: "CLAUDE_CONFIG_DIR"},
		{name: "-te eats -e then --env=ANTHROPIC_API_KEY", args: []string{"-te", "-e", "--env=ANTHROPIC_API_KEY=repo"}, wantErr: "ANTHROPIC_API_KEY"},
		{name: "-il eats -e then --env=ANTHROPIC_AUTH_TOKEN", args: []string{"-il", "-e", "--env=ANTHROPIC_AUTH_TOKEN=repo"}, wantErr: "ANTHROPIC_AUTH_TOKEN"},
		{name: "-tl eats -e then --env=LD_PRELOAD", args: []string{"-tl", "-e", "--env=LD_PRELOAD=/workspace/.git/evil.so"}, wantErr: "LD_PRELOAD"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAccountDockerRunArgs(tt.args, "claude", nil)
			require.Errorf(t, err, "an identity env sneaked behind a cluster's swallowed -e must be refused: %v", tt.args)
			require.Contains(t, err.Error(), tt.wantErr)
			require.NotContains(t, err.Error(), "--env")
		})
	}
}
