package session

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// This file is the #7726 regression suite. The list below is exactly the
// option set the issue measured on Docker 29.4.0: a bare LONG value option
// consumes the NEXT argv element as its value whatever its shape, so a
// following -e or mount flag is the option's value, never a flag — and a
// walk that does not consume re-reads it as one. On master every test here
// fails: the guard walked past the swallowing option, re-read the swallowed
// token, and checked a phantom value while the REAL flag behind it went
// unchecked — exactly the parse that lets docker set the env var af was
// asked to refuse.

// dockerRunLongoptSwallowsEnv is the option list #7726 names for both
// guards: each one takes a value, so its bare form swallows the next token.
var dockerRunLongoptSwallowsEnv = []string{
	"--label",
	"--hostname",
	"--user",
	"--workdir",
	"--name",
	"--network",
	"--entrypoint",
	"--add-host",
	"--cap-add",
	"--publish",
	"--memory",
	"--attach",
	"--platform",
	"--restart",
	"--log-opt",
	"--shm-size",
	"--ulimit",
	"--label-file",
}

// dockerRunLongoptSwallowsCredential adds the mount-installing options
// #7726 lists for the credential path: its guard checks no mounts, so on
// master it walked straight past --volume/--mount/--tmpfs/--device too and
// re-read the -e they swallowed. The account path checks those options, so
// it already consumed their values there — the env-side swallow is the one
// that reached it.
var dockerRunLongoptSwallowsCredential = []string{
	"--volume",
	"--mount",
	"--tmpfs",
	"--device",
}

// TestDockerAccount_RefusesEnvBehindLongoptValue is #7726 item 1 on the
// account guard: the bare long option swallows `-e`, so the --env behind it
// is the flag docker parses — and the one af must check.
func TestDockerAccount_RefusesEnvBehindLongoptValue(t *testing.T) {
	for _, opt := range dockerRunLongoptSwallowsEnv {
		t.Run(opt, func(t *testing.T) {
			args := []string{opt, "-e", "--env=CODEX_API_KEY=repo-identity"}
			err := validateAccountDockerRunArgs(args, "codex", nil)
			require.Errorf(t, err, "env flag behind a consumed %s value was accepted: %v", opt, args)
			require.Contains(t, err.Error(), "CODEX_API_KEY",
				"the refusal must name the variable the operator has to remove, not the phantom --env")
		})
	}
}

// TestDockerAccount_RefusesMountBehindLongoptValue is the same swallow with
// a mount flag: --label eats -v, so --mount= is the flag docker parses — a
// real bind over the account directory af must still refuse.
func TestDockerAccount_RefusesMountBehindLongoptValue(t *testing.T) {
	for _, opt := range []string{"--label", "--user", "--network", "--entrypoint", "--name"} {
		for _, args := range [][]string{
			{opt, "-v", "--mount=type=bind,source=/host,target=/af-account"},
			{opt, "--volume", "--mount=type=bind,source=/host,target=/af-account"},
			{opt, "-e", "--volume=/host:/af-account"},
		} {
			err := validateAccountDockerRunArgs(args, "codex", nil)
			require.Errorf(t, err, "mount flag behind a consumed %s value was accepted: %v", opt, args)
			require.Contains(t, err.Error(), "/af-account",
				"the refusal must name the protected destination, not the swallowed token")
		}
	}
}

// TestDockerAccount_LongoptSwallowsEnvFlagSilently is the acceptance side of
// the same parse: the token a long option consumes is its VALUE — never a
// flag — so a guarded spelling it swallows installs nothing and must not be
// refused. The rows end in a real option or nothing, because a bare token
// after the swallowed value is a positional refused for its own reason
// (#7733 R1).
func TestDockerAccount_LongoptSwallowsEnvFlagSilently(t *testing.T) {
	for _, args := range [][]string{
		// -e is --user's value; the option after it is still parsed.
		{"--user", "-e", "--name", "x"},
		// -v is --label's value — docker installs no mount, so there is
		// nothing to refuse.
		{"--label", "-v", "--name", "x"},
		{"--hostname", "--mount", "--name", "x"},
		// A real env flag AFTER the swallowed token is still checked —
		// and a harmless name still passes.
		{"--label", "-e", "--env=TZ=UTC"},
		{"--user", "-e", "-e", "TZ=UTC"},
	} {
		require.NoErrorf(t, validateAccountDockerRunArgs(args, "codex", nil),
			"a run_args Docker admits was refused: %v", args)
	}
}

// TestDockerCredential_RefusesEnvBehindLongoptValue is #7726 item 1 on the
// credential-mount guard, over its full option list — the eighteen the two
// guards share plus the four mount options this path does not check.
func TestDockerCredential_RefusesEnvBehindLongoptValue(t *testing.T) {
	for _, opt := range append(append([]string{}, dockerRunLongoptSwallowsEnv...),
		dockerRunLongoptSwallowsCredential...) {
		t.Run(opt, func(t *testing.T) {
			args := []string{opt, "-e", "--env=GH_HOST=evil.example"}
			err := validateCredentialDockerRunArgs(args, tmux.ProgramGemini, nil)
			require.Errorf(t, err, "env flag behind a consumed %s value was accepted: %v", opt, args)
			require.Contains(t, err.Error(), "GH_HOST",
				"the refusal must name the variable the operator has to remove, not the phantom --env")
		})
	}
}

// TestDockerCredential_LongoptSwallowsEnvFlagSilently is the credential
// path's acceptance side: the swallowed token is the option's value, a real
// option after it is still parsed, and a real env flag checked later still
// passes when its name is harmless.
func TestDockerCredential_LongoptSwallowsEnvFlagSilently(t *testing.T) {
	for _, agent := range []string{tmux.ProgramCodex, tmux.ProgramGemini} {
		for _, args := range [][]string{
			{"--user", "-e", "--name", "x"},
			// The mount options this path does not check still consume —
			// --volume's value is -e.
			{"--volume", "-e", "--name", "x"},
			{"--label", "-e", "--env=TZ=UTC"},
			{"--mount", "type=bind,source=/x,target=/y", "-e", "TZ=UTC"},
		} {
			require.NoErrorf(t, validateCredentialDockerRunArgs(args, agent, nil),
				"agent %q: a run_args Docker admits was refused: %v", agent, args)
		}
	}
}

// TestDockerAccount_RefusesUnknownLongopt is #7726 item 2's long-option
// half: an option af's table does not know either takes a value docker
// feeds the next argument — before af can check it — or is one docker
// rejects outright. Both readings say refuse rather than walk past.
func TestDockerAccount_RefusesUnknownLongopt(t *testing.T) {
	for _, args := range [][]string{
		{"--bogus-flag", "-e", "--env=CODEX_API_KEY=repo"},
		{"--bogus-flag=x", "-e", "CODEX_API_KEY=repo"},
		{"--bogus-flag"},
		{"--log-level=debug"}, // a docker GLOBAL option `docker run` rejects
		{"--config=/x"},
	} {
		err := validateAccountDockerRunArgs(args, "codex", nil)
		require.Errorf(t, err, "an option af cannot model was walked past: %v", args)
		require.Contains(t, err.Error(), "docker.run_args")
	}
}

// TestDockerAccount_RefusesUnknownBareShorthand is the same rule one dash
// over: a bare `-x` af cannot classify could take the next argument as its
// value, so it refuses rather than walking on (#7726 item 2).
func TestDockerAccount_RefusesUnknownBareShorthand(t *testing.T) {
	for _, args := range [][]string{
		{"-Z", "-e", "--env=CODEX_API_KEY=repo"},
		{"-Z"},
		{"-D", "-e", "TZ=UTC"},
		{"-H", "unix:///sock"},
	} {
		err := validateAccountDockerRunArgs(args, "codex", nil)
		require.Errorf(t, err, "an unknown bare shorthand was walked past: %v", args)
		require.Contains(t, err.Error(), "docker.run_args")
	}
}

// TestDockerAccount_RefusesDashDashTerminator refuses `--`: it ends docker's
// option parsing mid-argv, and af emits its own options and the image name
// after run_args — every one of them would become container command words.
func TestDockerAccount_RefusesDashDashTerminator(t *testing.T) {
	err := validateAccountDockerRunArgs([]string{"--", "-e", "TZ=UTC"}, "codex", nil)
	require.Errorf(t, err, "-- was walked past")
	require.Contains(t, err.Error(), "--")
}

// TestDockerCredential_RefusesUnknownOptions mirrors the account-path
// refusals on the credential guard.
func TestDockerCredential_RefusesUnknownOptions(t *testing.T) {
	for _, args := range [][]string{
		{"--bogus-flag", "-e", "--env=GH_HOST=evil.example"},
		{"--bogus-flag=x"},
		{"-Z", "-e", "--env=GH_HOST=evil.example"},
		{"-Z"},
		{"--", "-e", "GH_HOST=evil.example"},
		{"--log-level=debug"},
	} {
		err := validateCredentialDockerRunArgs(args, tmux.ProgramGemini, nil)
		require.Errorf(t, err, "an option af cannot model was walked past: %v", args)
		require.Contains(t, err.Error(), "docker.run_args")
	}
}

// TestDockerAccount_AdmitsCommonRunArgs is the no-over-refusal pin on the
// account guard: the common docker run option shapes, in both bare and
// attached spellings, plus a walk through positionals, stay admitted.
func TestDockerAccount_AdmitsCommonRunArgs(t *testing.T) {
	for _, args := range [][]string{
		{"-p", "8080:80"},
		{"--publish", "8080:80"},
		{"--publish=8080:80"},
		{"--network", "host"},
		{"--net", "host"},        // hidden alias of --network
		{"--net-alias=af-box"},   // hidden alias of --network-alias
		{"--dns-opt", "ndots:1"}, // hidden alias of --dns-option
		{"-u", "1000"},
		{"--user=1000"},
		{"-w", "/src"},
		{"--workdir=/src"},
		{"--add-host", "a:1.2.3.4"},
		{"-it", "-e", "TERM=xterm"},
		{"--label", "k=v"},
		{"--label=k=v"},
		{"--name", "x"},
		{"--entrypoint", "sh", "-e", "TZ=UTC"},
		{"--platform", "linux/amd64", "-e", "TZ=UTC"},
		{"--pull", "missing"},
		{"--restart=unless-stopped"},
		{"--memory=512m"},
		{"--shm-size", "64m"},
		{"--ulimit", "nofile=1024"},
		{"--label-file", "labels.txt"},
		{"--kernel-memory=1m"},      // deprecated but still registered
		{"--disable-content-trust"}, // deprecated boolean
		{"--cpu-count=2"},           // Windows-annotated, still parsed
		{"--io-maxiops=100"},        // Windows-annotated, still parsed
		{"--isolation=default"},     // Windows-annotated, still parsed
		{"--cgroupns=private"},
		{"--uts=host"},
		{"--userns=host"},
		{"--ipc=private"},
		{"--pid=host"},
		{"--gpus=all"},
		{"--health-cmd", "true", "--health-interval=5s"},
		{"--sig-proxy=false"},
		{"--use-api-socket"},
		{"--init"},
		{"--rm"},
		{"--privileged"},
		{"--read-only"},
		{"--no-healthcheck"},
		{"--oom-kill-disable"},
		{"--interactive"},
		{"--tty"},
		{"--detach"},
		{"--quiet"},
		{"--publish-all"},
		{"--annotation=af=1"},
		{"--link-local-ip=169.254.1.1"},
		{"--mac-address=02:42:ac:11:00:02"},
		{"--cidfile=/tmp/cid"},
		{"--stop-signal=SIGKILL"},
		{"--stop-timeout=5"},
		{"--storage-opt=size=1G"},
		{"--sysctl=net.ipv4.ip_forward=1"},
		{"--security-opt=seccomp=unconfined"},
		{"--log-driver=json-file", "--log-opt=max-size=1m"},
		{"--device-cgroup-rule=c 1:3 mr"},
		{"--device-read-bps=/dev/sda:1mb"},
		{"--blkio-weight=100"},
		{"--cap-drop=ALL"},
		{"--domainname=example"},
		{"--dns=1.1.1.1", "--dns-search=corp"},
		{"--expose=8080"},
		{"--group-add=docker"},
		{"--ip=10.0.0.2"},
		{"--link=other:alias"},
		{"--oom-score-adj=-100"},
		{"--pids-limit=100"},
		{"--runtime=runc"},
		{"--volume-driver=local"},
	} {
		require.NoErrorf(t, validateAccountDockerRunArgs(args, "codex", nil),
			"a run_args shape docker run accepts was refused: %v", args)
	}
}

// TestDockerAccount_RefusesPositionalTokens is the #7733 R1 fix: `docker
// run` and `docker create` parse options non-interspersed
// (SetInterspersed(false)), so a token pflag treats as a non-option argument
// — an image name, a lone `-`, an empty string — becomes the IMAGE, and
// every option af appends after run_args (the labels, env pins, mounts,
// --entrypoint, the configured image) becomes container command words
// instead. The remedy the refusal names is the operator's own config key.
func TestDockerAccount_RefusesPositionalTokens(t *testing.T) {
	for _, args := range [][]string{
		{"evil/img"},
		{"evil/img", "-e", "TZ=UTC"},
		{"weird-image", "-e", "TZ=UTC"},
		{"-"},
		{""},
		{"-it", "evil/img"},
	} {
		err := validateAccountDockerRunArgs(args, "codex", nil)
		require.Errorf(t, err, "a non-option argument was admitted: %v", args)
		require.Contains(t, err.Error(), "docker.image",
			"the refusal must name the config key that sets the image")
	}
	// A token that LOOKS positional but is an option's consumed value is not
	// a positional: "evil/img" is --label's value, so docker's parsing
	// continues and the real image is still af's configured one.
	require.NoError(t, validateAccountDockerRunArgs(
		[]string{"--label", "evil/img", "-e", "TZ=UTC"}, "codex", nil))
}

// TestDockerCredential_AdmitsCommonRunArgs is the same pin on the credential
// guard, minus --env-file (which this path refuses) and plus the mount
// options it does not check but must still consume correctly.
func TestDockerCredential_AdmitsCommonRunArgs(t *testing.T) {
	for _, agent := range []string{tmux.ProgramCodex, tmux.ProgramGemini} {
		for _, args := range [][]string{
			{"-p", "8080:80"},
			{"--network", "host"},
			{"-u", "1000"},
			{"-w", "/src"},
			{"--add-host", "a:1.2.3.4"},
			{"-it", "-e", "TERM=xterm"},
			{"--label", "k=v"},
			{"--name", "x"},
			{"--net", "host"},
			{"--dns-opt", "ndots:1"},
			{"--kernel-memory=1m"},
			{"--cpu-count=2"},
			{"--volume", "/host:/container"},
			{"--volume=/host:/container"},
			{"-v", "/host:/container"},
			{"-v/host:/container"},
			{"--mount", "type=bind,source=/x,target=/y"},
			{"--mount=type=bind,source=/x,target=/y"},
			{"--tmpfs", "/run"},
			{"--device", "/dev/fuse"},
			{"--device=/dev/fuse"},
			{"--entrypoint", "sh", "-e", "TZ=UTC"},
			{"--pull", "missing"},
			{"--sig-proxy=false"},
		} {
			require.NoErrorf(t, validateCredentialDockerRunArgs(args, agent, nil),
				"agent %q: a run_args shape docker run accepts was refused: %v", agent, args)
		}
	}
}

// TestDockerCredential_RefusesPositionalTokens pins the credential guard on
// the #7733 R1 property: a non-option argument ends docker's option parsing
// and becomes the image, turning af's trailing options into container Cmd.
func TestDockerCredential_RefusesPositionalTokens(t *testing.T) {
	for _, agent := range []string{tmux.ProgramCodex, tmux.ProgramGemini} {
		for _, args := range [][]string{
			{"evil/img"},
			{"evil/img", "-e", "TZ=UTC"},
			{"weird-image", "-e", "TZ=UTC"},
			{"-"},
			{"-it", "evil/img"},
		} {
			err := validateCredentialDockerRunArgs(args, agent, nil)
			require.Errorf(t, err, "agent %q: a non-option argument was admitted: %v", agent, args)
			require.Contains(t, err.Error(), "docker.image",
				"agent %q: the refusal must name the config key that sets the image", agent)
		}
	}
}

// TestValidateDockerRunArgsShape covers the guard every docker session
// shares — the plain-session gate in runPayloadContainer — where run_args
// sits directly ahead of af's own options and the image.
func TestValidateDockerRunArgsShape(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"-p", "8080:80"},
		{"--network", "host"},
		{"-it", "-e", "TERM=xterm"},
		{"--label", "k=v"},
		{"--name", "x"},
		{"-v", "/host:/container"},
		{"--mount=type=bind,source=/x,target=/y"},
		{"--label", "evil/img", "-e", "TZ=UTC"}, // evil/img is --label's value
		{"-v", "/host:/dst", "--pull", "always"},
	} {
		require.NoErrorf(t, validateDockerRunArgsShape(args),
			"an option-shaped run_args list was refused: %v", args)
	}
	for _, args := range [][]string{
		{"evil/img"},
		{"evil/img", "-e", "TZ=UTC"},
		{"-"},
		{""},
		{"--", "-e", "TZ=UTC"},
		{"--frobnicate", "x"},
		{"-it", "evil/img"},
	} {
		err := validateDockerRunArgsShape(args)
		require.Errorf(t, err, "a run_args shape that ends or outlasts docker's parsing was admitted: %v", args)
	}
}

// TestDockerRunArgsEnvNames_SwallowedEnvReportsTheRealName pins the warning
// extractor on the same parse: a -e a long option swallowed is not an env
// flag, and the --env behind it is — the name af reports must be the real
// one, never the phantom "--env" master's re-read produced (#7726's
// "warning names the phantom --env" observation).
func TestDockerRunArgsEnvNames_SwallowedEnvReportsTheRealName(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "swallowed -e reports the flag behind it",
			args: []string{"--label", "-e", "--env=GH_HOST=evil"}, want: []string{"GH_HOST"}},
		{name: "shorthand swallow",
			args: []string{"-l", "-e", "--env=GH_HOST=evil"}, want: []string{"GH_HOST"}},
		{name: "swallowed -e, positional value",
			args: []string{"--label", "-e", "TZ=UTC"}, want: nil},
		{name: "boolean then env",
			args: []string{"--interactive", "-e", "TZ=UTC"}, want: []string{"TZ"}},
		{name: "attached value consumes nothing",
			args: []string{"--label=k=v", "-e", "TZ=UTC"}, want: []string{"TZ"}},
		{name: "cluster swallow then real env",
			args: []string{"-il", "-e", "--env=GH_HOST=evil"}, want: []string{"GH_HOST"}},
		{name: "positional ends docker's parsing",
			args: []string{"evil/img", "-e", "GH_HOST=evil"}, want: nil},
		{name: "positional mid-list stops the extraction",
			args: []string{"-e", "TZ=UTC", "img", "--env=GH_HOST=evil"}, want: []string{"TZ"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, dockerRunArgsEnvNames(tt.args))
		})
	}
}
