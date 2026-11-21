package session

// Regression tests for the bare-shorthand run_args bypass (the bug report's
// evidence table): a bare two-character value-taking short option (-l, -h, -w,
// -p, -u, -m, -a, -c, -e) consumes the NEXT argv element as its value under
// pflag, so a following `-e` is swallowed and the real `--env=NAME=...` behind
// it is the env Docker honors. The credential-path walker
// (validateCredentialDockerRunArgs) and the warning extractor
// (dockerRunArgsEnvNames) skipped the bare shorthand and re-read the swallowed
// `-e` as a fresh env flag, checking the phantom name `--env` strings.Cut left
// and letting a denied name through. These tests pin the fix that consumes
// the swallowed value so the outer walk examines the REAL flag behind it.
//
// Measured: `docker run --rm -l -e --env=AFPROBE_GH=zzz busybox env` sets
// AFPROBE_GH=zzz, so the sneaked flag is honored and the guard MUST refuse it.

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestDockerCredentialRunArgs_RefusesDeniedEnvBehindBareValueShorthand is the
// core regression from the bare-shorthand bypass report. Under pflag (the
// parser `docker run` uses), a bare value-taking short option consumes the NEXT
// argv element as its value, so a token like `-l` swallows the following `-e`
// and the real `--env=NAME=...` behind it is the env Docker honors. The buggy
// walk skipped the bare shorthand entirely, re-read the swallowed `-e` as a
// fresh env flag, checked the phantom name `--env` strings.Cut left, and let
// the DENIED name flow in unguarded. The fix consumes the swallowed value so
// the outer walk never re-reads it.
//
// Measured: `docker run --rm -l -e --env=AFPROBE_GH=zzz busybox env` sets
// AFPROBE_GH=zzz — the sneaked flag is honored, so the guard MUST refuse it.
func TestDockerCredentialRunArgs_RefusesDeniedEnvBehindBareValueShorthand(t *testing.T) {
	for _, tt := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		// The headline: a bare -l eats the following -e, so the real
		// --env=GH_HOST=... is the env Docker honors and must be refused.
		{name: "label eats -e then --env=NAME", args: []string{"-l", "-e", "--env=GH_HOST=attacker.example"}, wantErr: "GH_HOST"},
		{name: "label eats -e then -e=NAME", args: []string{"-l", "-e", "-e=GH_HOST=attacker.example"}, wantErr: "GH_HOST"},
		{name: "label eats -e then -eNAME", args: []string{"-l", "-e", "-eGH_HOST=attacker.example"}, wantErr: "GH_HOST"},
		// The same shape with every other value-taking short option in the
		// walk's blind spot (each is in dockerRunValueShorthands). -w/-u/-m/-a
		// and -c all consume the next arg the same way under pflag.
		{name: "hostname eats -e then --env=NAME", args: []string{"-h", "-e", "--env=GH_HOST=attacker.example"}, wantErr: "GH_HOST"},
		{name: "workdir eats -e then --env=NAME", args: []string{"-w", "-e", "--env=GH_HOST=attacker.example"}, wantErr: "GH_HOST"},
		{name: "user eats -e then --env=NAME", args: []string{"-u", "-e", "--env=GH_HOST=attacker.example"}, wantErr: "GH_HOST"},
		{name: "memory eats -e then --env=NAME", args: []string{"-m", "-e", "--env=GH_HOST=attacker.example"}, wantErr: "GH_HOST"},
		{name: "attach eats -e then --env=NAME", args: []string{"-a", "-e", "--env=GH_HOST=attacker.example"}, wantErr: "GH_HOST"},
		{name: "cpu-shares eats -e then --env=NAME", args: []string{"-c", "-e", "--env=GH_HOST=attacker.example"}, wantErr: "GH_HOST"},
		{name: "publish eats -e then --env=NAME", args: []string{"-p", "-e", "--env=GH_HOST=attacker.example"}, wantErr: "GH_HOST"},
		// The LD_PRELOAD / LD_LIBRARY_PATH variant: the program-loader names
		// are refused to run_args but never pinned, so a sneaked value lands
		// in the container env af's provisioning docker exec steps inherit.
		{name: "label eats -e then --env=LD_PRELOAD", args: []string{"-l", "-e", "--env=LD_PRELOAD=/workspace/.git/evil.so"}, wantErr: "LD_PRELOAD"},
		{name: "label eats -e then --env=LD_LIBRARY_PATH", args: []string{"-l", "-e", "--env=LD_LIBRARY_PATH=/workspace/.git/evil"}, wantErr: "LD_LIBRARY_PATH"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCredentialDockerRunArgs(tt.args, tmux.ProgramGemini)
			require.Errorf(t, err, "a denied env name sneaked behind a bare value-taking shorthand must be refused: %v", tt.args)
			require.Contains(t, err.Error(), "docker.run_args")
			// The refusal must name the REAL denied variable, not the phantom
			// "--env" the buggy walk checked when it re-read -e's value.
			require.Contains(t, err.Error(), tt.wantErr,
				"the refusal must name the denied variable the repo tried to set")
			require.NotContains(t, err.Error(), "--env",
				"the refusal must not echo the phantom name the buggy walk inspected")
			require.Contains(t, err.Error(), "session_env_passthrough", "the refusal must name the operator remedy")
		})
	}
}

// TestDockerCredentialRunArgs_RefusesDeniedEnvBehindClusterValueShorthand is
// the cluster sibling of the bare-shorthand bypass. A combined short option
// ending in a value-taking option consumes the NEXT argv element as that
// option's value, so `-ie` swallows the following `-e` and the real
// `--env=NAME=...` behind it is the env Docker honors (measured: `docker run
// --rm -ie -e --env=AFPROBE_GH=zzz busybox env` sets AFPROBE_GH=zzz). The same
// bypass shape existed for the cluster form.
func TestDockerCredentialRunArgs_RefusesDeniedEnvBehindClusterValueShorthand(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "-ie eats -e then --env=NAME", args: []string{"-ie", "-e", "--env=GH_HOST=attacker.example"}},
		{name: "-te eats -e then --env=NAME", args: []string{"-te", "-e", "--env=GH_HOST=attacker.example"}},
		{name: "-il eats -e then --env=NAME", args: []string{"-il", "-e", "--env=GH_HOST=attacker.example"}},
		{name: "-tl eats -e then --env=NAME", args: []string{"-tl", "-e", "--env=GH_HOST=attacker.example"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCredentialDockerRunArgs(tt.args, tmux.ProgramGemini)
			require.Errorf(t, err, "a denied env name sneaked behind a cluster's swallowed -e must be refused: %v", tt.args)
			require.Contains(t, err.Error(), "GH_HOST")
			require.NotContains(t, err.Error(), "--env",
				"the refusal must not echo the phantom name the buggy walk inspected")
		})
	}
}

// TestDockerCredentialRunArgs_AllowsBareValueShorthandConsumingGuardedToken
// is the over-refusal boundary for the bare-shorthand fix: a bare -l that
// swallows a -v (per pflag: `-l -v /spec` means the label value is `-v`)
// installs NO mount, and a -v that swallows an -e installs NO env. The guard
// must not refuse these Docker-never-honored tokens, and a real guard-written
// env after the consumed value is still examined on its own.
func TestDockerCredentialRunArgs_AllowsBareValueShorthandConsumingGuardedToken(t *testing.T) {
	// -l swallows -v as its value: -v is NOT a mount on the credential path.
	// (The trailing tokens are real options — a bare spec would be a
	// positional and is refused for its own reason, #7733 R1.)
	require.NoErrorf(t, validateCredentialDockerRunArgs([]string{"-l", "-v", "-e", "TZ=UTC"}, tmux.ProgramCodex),
		"a -v swallowed by a bare -l installs no mount and must not be refused")
	// -h swallows --mount as its value: --mount is NOT a mount on this path.
	require.NoErrorf(t, validateCredentialDockerRunArgs([]string{"-h", "--mount", "-e", "TZ=UTC"}, tmux.ProgramCodex),
		"a --mount swallowed by a bare -h installs no mount and must not be refused")

	// A -v consumes the next arg as its value too: a following -e is the value
	// of -v, not a fresh env flag. The walk must NOT mis-read that -e as an env
	// and refuse it (the over-refusal mirror). -v itself installs a mount af
	// does not guard on this path.
	require.NoErrorf(t, validateCredentialDockerRunArgs([]string{"-v", "-e", "-e", "TZ=UTC"}, tmux.ProgramCodex),
		"a -e swallowed by a bare -v is -v's value, not a fresh env flag")

	// A bare boolean (-i) consumes nothing, so the following -e is a real env
	// flag setting a harmless name and must still be allowed (fail-closed
	// side, not over-refused).
	require.NoErrorf(t, validateCredentialDockerRunArgs([]string{"-i", "-e", "TZ=UTC"}, tmux.ProgramCodex),
		"a -e after a bare boolean must still be examined as a real env flag")
}

// TestDockerRunArgsEnvNames_ExtractsRealNameBehindBareValueShorthand is the
// warning-extractor half of the bare-shorthand bypass fix. The warning layer
// (dockerRunArgsEnvNames) shares the same walk as the validator, so it had the
// same blind spot: `-l -e --env=GH_HOST=evil` returned the phantom name
// `--env` (strings.Cut on the flag -e re-read as an env value) instead of
// `GH_HOST`, hiding a denied override from the migration notice. The fix
// consumes the value a bare value-taking shorthand swallows so the extractor
// walks the REAL env flag behind it and names the variable the operator
// actually has to forward.
func TestDockerRunArgsEnvNames_ExtractsRealNameBehindBareValueShorthand(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string // the REAL env name Docker honors, not the phantom
	}{
		{name: "bare -l eats -e then --env=NAME", args: []string{"-l", "-e", "--env=GH_HOST=evil"}, want: "GH_HOST"},
		{name: "bare -h eats -e then --env=NAME", args: []string{"-h", "-e", "--env=GH_HOST=evil"}, want: "GH_HOST"},
		{name: "bare -w eats -e then --env=NAME", args: []string{"-w", "-e", "--env=GH_HOST=evil"}, want: "GH_HOST"},
		{name: "bare -p eats -e then --env=NAME", args: []string{"-p", "-e", "--env=GH_HOST=evil"}, want: "GH_HOST"},
		{name: "bare -l eats -e then -eNAME", args: []string{"-l", "-e", "-eGH_HOST=evil"}, want: "GH_HOST"},
		{name: "cluster -ie eats -e then --env=NAME", args: []string{"-ie", "-e", "--env=GH_HOST=evil"}, want: "GH_HOST"},
		{name: "cluster -te eats -e then --env=NAME", args: []string{"-te", "-e", "--env=GH_HOST=evil"}, want: "GH_HOST"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := sortedNamesToSet(dockerRunArgsEnvNames(tt.args))
			require.Containsf(t, got, tt.want, "the REAL env name %s Docker honors must be extracted", tt.want)
			require.NotContainsf(t, got, "--env",
				"the phantom name from the re-read -e value must not be reported: %v", got)
		})
	}
}

// TestDockerRunArgsEnvNames_BareBooleanDoesNotConsume pins the fail-closed
// side of the bare-shorthand fix: a bare BOOLEAN short option (-i, -t, -d, -q,
// -P) consumes nothing, so the following -e is a real env flag and the
// extractor still reports it exactly as before the fix.
func TestDockerRunArgsEnvNames_BareBooleanDoesNotConsume(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "-i then -e NAME", args: []string{"-i", "-e", "GH_HOST=evil"}},
		{name: "-t then -e NAME", args: []string{"-t", "-e", "GH_HOST=evil"}},
		{name: "-d then -e NAME", args: []string{"-d", "-e", "GH_HOST=evil"}},
		{name: "-q then -e NAME", args: []string{"-q", "-e", "GH_HOST=evil"}},
		{name: "-P then -e NAME", args: []string{"-P", "-e", "GH_HOST=evil"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := sortedNamesToSet(dockerRunArgsEnvNames(tt.args))
			require.Contains(t, got, "GH_HOST", "a -e after a bare boolean must still be extracted: %v", tt.args)
		})
	}
}
