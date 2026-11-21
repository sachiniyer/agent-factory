package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// These tests pin the `docker run` option tables the long-option half of the
// pflag model consumes through. The lists are Docker 29.4.0's: everything
// `docker run --help` prints, plus the spellings it accepts without printing —
// hidden aliases (--net, --net-alias, --dns-opt), deprecated-but-registered
// options (--kernel-memory, --disable-content-trust), and the
// Windows-annotated options a Linux daemon still parses (--cpu-count,
// --cpu-percent, --io-maxbandwidth, --io-maxiops). They deliberately exclude
// the docker CLI's Global Options — `docker run` rejects those outright, and
// af refusing them the same way is the fail-closed posture #7726 asks for.

var dockerRunExpectedValueLongopts = []string{
	"add-host", "annotation", "attach",
	"blkio-weight", "blkio-weight-device",
	"cap-add", "cap-drop", "cgroup-parent", "cgroupns", "cidfile",
	"cpu-count", "cpu-percent", "cpu-period", "cpu-quota",
	"cpu-rt-period", "cpu-rt-runtime", "cpu-shares", "cpus",
	"cpuset-cpus", "cpuset-mems",
	"detach-keys", "device", "device-cgroup-rule",
	"device-read-bps", "device-read-iops", "device-write-bps", "device-write-iops",
	"dns", "dns-opt", "dns-option", "dns-search", "domainname",
	"entrypoint", "env", "env-file", "expose",
	"gpus", "group-add",
	"health-cmd", "health-interval", "health-retries",
	"health-start-interval", "health-start-period", "health-timeout",
	"hostname",
	"io-maxbandwidth", "io-maxiops", "ip", "ip6", "ipc", "isolation",
	"kernel-memory",
	"label", "label-file", "link", "link-local-ip",
	"log-driver", "log-opt",
	"mac-address", "memory", "memory-reservation", "memory-swap", "memory-swappiness",
	"mount",
	"name", "net", "net-alias", "network", "network-alias",
	"oom-score-adj",
	"pid", "pids-limit", "platform", "publish", "pull",
	"restart", "runtime",
	"security-opt", "shm-size", "stop-signal", "stop-timeout", "storage-opt",
	"sysctl", "tmpfs", "ulimit", "user", "userns", "uts",
	"volume", "volume-driver", "volumes-from", "workdir",
}

var dockerRunExpectedBooleanLongopts = []string{
	"detach", "disable-content-trust", "help", "init", "interactive",
	"no-healthcheck", "oom-kill-disable", "privileged", "publish-all",
	"quiet", "read-only", "rm", "sig-proxy", "tty", "use-api-socket",
}

func TestDockerRunLongoptTables_CoverEveryDockerLongopt(t *testing.T) {
	require.Len(t, dockerRunExpectedValueLongopts, 92,
		"the transcribed Docker 29.4.0 value-option list itself drifted")
	require.Len(t, dockerRunExpectedBooleanLongopts, 15,
		"the transcribed Docker 29.4.0 boolean list itself drifted")
	for _, name := range dockerRunExpectedValueLongopts {
		require.Containsf(t, dockerRunValueLongopts, name,
			"--%s takes a value in Docker 29.4.0 but is missing from the value table — a bare --%s would refuse or misparse", name, name)
		require.NotContainsf(t, dockerRunBooleanLongopts, name,
			"--%s sits in BOTH tables", name)
	}
	for _, name := range dockerRunExpectedBooleanLongopts {
		require.Containsf(t, dockerRunBooleanLongopts, name,
			"--%s is boolean in Docker 29.4.0 but is missing from the boolean table", name)
		require.NotContainsf(t, dockerRunValueLongopts, name,
			"--%s sits in BOTH tables", name)
	}
	require.Len(t, dockerRunValueLongopts, len(dockerRunExpectedValueLongopts),
		"the value table holds an option Docker 29.4.0 does not accept — af would consume a next argument docker leaves alone")
	require.Len(t, dockerRunBooleanLongopts, len(dockerRunExpectedBooleanLongopts),
		"the boolean table holds an option Docker 29.4.0 does not accept")
}

// TestDockerRunOptionConsumption_ModelsPflagConsumption pins the classifier
// itself: bare value-taking long options and shorthands consume the next
// argv element, attached and boolean forms do not, and everything af cannot
// model is an error — never a silent walk-past.
func TestDockerRunOptionConsumption_ModelsPflagConsumption(t *testing.T) {
	for _, tt := range []struct {
		arg     string
		consume bool
		wantErr bool
	}{
		{"--label", true, false},
		{"--label=k=v", false, false},
		{"--network", true, false},
		{"--pull", true, false},
		{"--env-file", true, false}, // reached only where the env-file refusal did not
		{"--rm", false, false},
		{"--sig-proxy", false, false},
		{"--sig-proxy=false", false, false},
		{"-l", true, false},
		{"-u", true, false},
		{"-w", true, false},
		{"-d", false, false},
		{"-i", false, false},
		{"-t", false, false},
		{"-P", false, false},
		{"-q", false, false},
		{"--bogus", false, true},
		{"--bogus=x", false, true},
		{"--log-level=x", false, true}, // a global option docker run rejects
		{"-Z", false, true},
		{"-H", false, true},
		{"--", false, true},
		{"-", false, false},          // a bare dash is positional
		{"positional", false, false}, // positionals consume nothing
		{"-it", false, false},        // clusters are the other walk's domain
	} {
		t.Run(tt.arg, func(t *testing.T) {
			consume, err := dockerRunOptionConsumption(tt.arg, "test")
			if tt.wantErr {
				require.Errorf(t, err, "%s should refuse", tt.arg)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.consume, consume)
		})
	}
}

// TestDockerClusterConsumesNextArg_ModelsPflagClusters pins the cluster half:
// the first value-taking shorthand consumes the rest of the cluster inline,
// or the next argv element when nothing follows it; booleans walk on; an
// explicit = ends the cluster before the kind is consulted.
func TestDockerClusterConsumesNextArg_ModelsPflagClusters(t *testing.T) {
	for _, tt := range []struct {
		arg     string
		consume bool
	}{
		{"-it", false},  // all booleans
		{"-itd", false}, // all booleans
		{"-l", true},    // a value shorthand ending the cluster
		{"-il", true},   // booleans then a value shorthand
		{"-tlu", false}, // l takes "u" as its inline value
		{"-lfoo", false},
		{"-t=l", false},
		{"-uv", false}, // u takes "v" inline — v is never an option
		{"-ve", false}, // v takes "e" inline
		{"-te", true},  // e ends the cluster wanting a value
		{"-itv", true},
		{"-Z", false},  // unknown shorthand — af cannot prove a consume
		{"-tZ", false}, // trailing unknown — same
		{"-Zu", false}, // leading unknown — same
	} {
		t.Run(tt.arg, func(t *testing.T) {
			require.Equal(t, tt.consume, dockerClusterConsumesNextArg(tt.arg))
		})
	}
}
