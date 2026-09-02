package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// The runtime account-boundary verification (#3598).
//
// The lexical guard reads docker.run_args as strings, and strings cannot see a
// symlink. An image the repository also selects can carry
// `/device-target -> /af-account`; Docker resolves it when it installs -v,
// --mount, --tmpfs and --device, and the mount lands inside af's account
// boundary while validateAccountDockerRunArgs sees an unprotected path.
//
// Every fixture below is a REAL capture from Docker 29.4.0 against the issue's
// own image (`FROM alpine:3.20` + `RUN mkdir -p /af-account && ln -s /af-account
// /device-target`), trimmed to the lines that matter. Writing them by hand is
// how a check ends up agreeing with a table nobody ran: note that `docker
// inspect` reports the CONFIGURED path `/device-target/.config` while mountinfo
// reports the RESOLVED `/af-account/.config`, which is the entire defect, and no
// invented fixture would have that shape by accident.

const verifyAccountSource = "/home/op/.agent-factory/accounts/codex/work"

// cleanInspect is `docker inspect` for a container af provisioned with nothing
// but the account mount.
func cleanInspect(source string) string {
	return fmt.Sprintf(`[{"Mounts":[{"Type":"bind","Source":%q,"Destination":"/af-account","Mode":"z","RW":true,"Propagation":"rprivate"}],"HostConfig":{"Tmpfs":null,"Devices":null}}]`, source)
}

// cleanMountinfo is /proc/1/mountinfo from that same container. The account bind
// appears exactly once, and every other line is Docker's own furniture.
const cleanMountinfo = `2421 268 0:67 / / rw,relatime - overlay overlay rw,lowerdir=/var/lib/containerd/x,upperdir=/var/lib/containerd/y,workdir=/var/lib/containerd/z,nouserxattr
2430 2421 0:85 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
2431 2421 0:86 / /dev rw,nosuid - tmpfs tmpfs rw,size=65536k,mode=755,inode64
2432 2431 0:87 / /dev/pts rw,nosuid,noexec,relatime - devpts devpts rw,gid=5,mode=620,ptmxmode=666
2436 2421 0:88 / /sys ro,nosuid,nodev,noexec,relatime - sysfs sysfs ro
2437 2436 0:29 / /sys/fs/cgroup ro,nosuid,nodev,noexec,relatime - cgroup2 cgroup rw,nsdelegate,memory_recursiveprot
2439 2431 0:89 / /dev/shm rw,nosuid,nodev,noexec,relatime - tmpfs shm rw,size=65536k,inode64
2440 2421 8:1 /home/op/.agent-factory/accounts/codex/work /af-account rw,relatime - ext4 /dev/root rw,discard,errors=remount-ro,commit=30
2443 2421 8:1 /var/lib/docker/containers/cb5a/resolv.conf /etc/resolv.conf rw,relatime - ext4 /dev/root rw,discard,errors=remount-ro,commit=30
2445 2421 8:1 /var/lib/docker/containers/cb5a/hosts /etc/hosts rw,relatime - ext4 /dev/root rw,discard,errors=remount-ro,commit=30
`

// cleanDeviceScan is the scan's output when the boundary holds no device node:
// the completion sentinel and nothing else.
const cleanDeviceScan = accountDeviceScanSentinel + "\n"

// A container af provisioned and nobody tampered with must PASS all three
// checks. Without this the refusals below would be satisfied by a check that
// refuses everything.
func TestAccountRuntimeVerify_CleanContainerPasses(t *testing.T) {
	inspected, err := parseDockerInspectContainer([]byte(cleanInspect(verifyAccountSource)))
	require.NoError(t, err)
	require.NoError(t, verifyConfiguredAccountBoundary(inspected, verifyAccountSource))

	targets, err := parseMountinfoTargets([]byte(cleanMountinfo))
	require.NoError(t, err)
	require.NoError(t, verifyResolvedAccountBoundary(targets, configuredContainerPaths(inspected)))

	require.NoError(t, verifyAccountDeviceScan([]byte(cleanDeviceScan), nil, ""))
}

// The four aliased options from the issue's acceptance criteria. Each row is a
// real capture: the configured path is outside the boundary (so the lexical
// guard returns nil for it, which the last assertion pins) and the resolved one
// is inside.
func TestAccountRuntimeVerify_RefusesEveryAliasedOption(t *testing.T) {
	tests := []struct {
		name string
		// runArgs is what the repository wrote, proving the lexical guard
		// accepts this row — i.e. that the runtime check is what closes it.
		runArgs []string
		inspect string
		// mountinfo lines the kernel reported IN ADDITION to the clean set.
		extraMountinfo string
		deviceScan     string
		wantResolved   string
		wantConfigured string
	}{
		{
			name:    "-v landing under the account",
			runArgs: []string{"-v", "/repo/evil:/device-target/.config"},
			inspect: `[{"Mounts":[
				{"Type":"bind","Source":"` + verifyAccountSource + `","Destination":"/af-account","Mode":"z","RW":true},
				{"Type":"bind","Source":"/repo/evil","Destination":"/device-target/.config","Mode":"","RW":true}],
				"HostConfig":{"Tmpfs":null,"Devices":null}}]`,
			extraMountinfo: "2442 2440 8:1 /repo/evil /af-account/.config rw,relatime - ext4 /dev/root rw,discard\n",
			deviceScan:     cleanDeviceScan,
			wantResolved:   "/af-account/.config",
			wantConfigured: "/device-target/.config",
		},
		{
			name:    "--mount landing under the account",
			runArgs: []string{"--mount", "type=bind,src=/repo/evil,dst=/device-target/.config"},
			inspect: `[{"Mounts":[
				{"Type":"bind","Source":"` + verifyAccountSource + `","Destination":"/af-account","Mode":"z","RW":true},
				{"Type":"bind","Source":"/repo/evil","Destination":"/device-target/.config","Mode":"","RW":true}],
				"HostConfig":{"Tmpfs":null,"Devices":null}}]`,
			extraMountinfo: "2442 2440 8:1 /repo/evil /af-account/.config rw,relatime - ext4 /dev/root rw,discard\n",
			deviceScan:     cleanDeviceScan,
			wantResolved:   "/af-account/.config",
			wantConfigured: "/device-target/.config",
		},
		{
			name:    "--tmpfs landing under the account",
			runArgs: []string{"--tmpfs", "/device-target/tmp"},
			inspect: `[{"Mounts":[
				{"Type":"bind","Source":"` + verifyAccountSource + `","Destination":"/af-account","Mode":"z","RW":true}],
				"HostConfig":{"Tmpfs":{"/device-target/tmp":""},"Devices":null}}]`,
			extraMountinfo: "2441 2440 0:90 / /af-account/tmp rw,nosuid,nodev,noexec,relatime - tmpfs tmpfs rw,inode64\n",
			deviceScan:     cleanDeviceScan,
			wantResolved:   "/af-account/tmp",
			wantConfigured: "/device-target/tmp",
		},
		{
			name:    "-v landing exactly ON the account mount",
			runArgs: []string{"-v", "/repo/evil:/device-target"},
			inspect: `[{"Mounts":[
				{"Type":"bind","Source":"` + verifyAccountSource + `","Destination":"/af-account","Mode":"z","RW":true},
				{"Type":"bind","Source":"/repo/evil","Destination":"/device-target","Mode":"","RW":true}],
				"HostConfig":{"Tmpfs":null,"Devices":null}}]`,
			// Docker installs both; the account bind lands as a child of the
			// alias, so TWO filesystems sit at /af-account (measured).
			extraMountinfo: "2683 2674 8:1 /repo/evil /af-account rw,relatime - ext4 /dev/root rw,discard\n",
			deviceScan:     cleanDeviceScan,
			wantResolved:   "2 filesystems at /af-account",
			wantConfigured: "/device-target",
		},
		{
			name:    "--device planting a node under the account",
			runArgs: []string{"--device", "/dev/zero:/device-target/planted"},
			inspect: `[{"Mounts":[
				{"Type":"bind","Source":"` + verifyAccountSource + `","Destination":"/af-account","Mode":"z","RW":true}],
				"HostConfig":{"Tmpfs":null,
				"Devices":[{"PathOnHost":"/dev/zero","PathInContainer":"/device-target/planted","CgroupPermissions":"rwm"}]}}]`,
			// A device is a mknod, so it appears in NO mount table — the scan is
			// the only thing that can see it.
			extraMountinfo: "",
			deviceScan:     "/af-account/planted\n" + accountDeviceScanSentinel + "\n",
			wantResolved:   "/af-account/planted",
			wantConfigured: "/device-target/planted",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The premise: this row is invisible to the lexical guard. If a
			// future change makes it visible there, this row stops testing what
			// it claims to and must be rewritten rather than quietly passing.
			require.NoErrorf(t, validateAccountDockerRunArgs(tt.runArgs, "codex"),
				"premise: the lexical guard cannot see this alias, so the runtime check is what closes it")

			inspected, err := parseDockerInspectContainer([]byte(tt.inspect))
			require.NoError(t, err)
			// The configured half passes: every recorded container path is
			// outside the boundary, which is exactly why a lexical check fails.
			require.NoError(t, verifyConfiguredAccountBoundary(inspected, verifyAccountSource),
				"the configured set is lexically clean — that is the defect")
			configured := configuredContainerPaths(inspected)

			targets, err := parseMountinfoTargets([]byte(cleanMountinfo + tt.extraMountinfo))
			require.NoError(t, err)
			resolvedErr := verifyResolvedAccountBoundary(targets, configured)
			deviceErr := verifyAccountDeviceScan([]byte(tt.deviceScan), configured, verifyAccountSource)

			err = resolvedErr
			if err == nil {
				err = deviceErr
			}
			require.Errorf(t, err, "an aliased %s must refuse", tt.name)
			assert.Containsf(t, err.Error(), tt.wantResolved,
				"the refusal must name where the alias LANDED so the operator sees the boundary was crossed")
			assert.Containsf(t, err.Error(), tt.wantConfigured,
				"and the CONFIGURED path, so the operator knows which run_args entry to remove")
		})
	}
}

// An image VOLUME reaches the same boundary with no docker.run_args at all, so a
// check that only looks at run_args would miss it. Docker records it in .Mounts
// as a volume, and mountinfo shows where it landed.
func TestAccountRuntimeVerify_RefusesAnAliasedImageVolume(t *testing.T) {
	inspect := `[{"Mounts":[
		{"Type":"bind","Source":"` + verifyAccountSource + `","Destination":"/af-account","Mode":"z","RW":true},
		{"Type":"volume","Name":"anon","Source":"/var/lib/docker/volumes/anon/_data","Destination":"/device-target","RW":true}],
		"HostConfig":{"Tmpfs":null,"Devices":null}}]`
	inspected, err := parseDockerInspectContainer([]byte(inspect))
	require.NoError(t, err)
	require.NoError(t, verifyConfiguredAccountBoundary(inspected, verifyAccountSource))

	targets, err := parseMountinfoTargets([]byte(cleanMountinfo +
		"2683 2674 8:1 /var/lib/docker/volumes/anon/_data /af-account rw,relatime - ext4 /dev/root rw,discard\n"))
	require.NoError(t, err)
	err = verifyResolvedAccountBoundary(targets, configuredContainerPaths(inspected))
	require.Error(t, err, "an image VOLUME aliased onto the account must refuse; run_args is not the only source of a mount")
	assert.Contains(t, err.Error(), "/device-target")
}

// When Docker recorded NO container path but af's own account mount, nothing
// could have been aliased — there was no entry to resolve — so the refusal must
// not send the operator hunting for a symlink that is not there.
//
// Both reachable causes need a different remedy than "edit run_args": a device
// node planted by an EARLIER session (the daemon writes those as root at
// container start, before any check can run, so they outlive the refusal that
// finds them) and a nested mount point inside the account directory on the host.
func TestAccountRuntimeVerify_WithNothingConfiguredDoesNotBlameRunArgs(t *testing.T) {
	inspected, err := parseDockerInspectContainer([]byte(cleanInspect(verifyAccountSource)))
	require.NoError(t, err)
	configured := configuredContainerPaths(inspected)
	require.Empty(t, configured, "the fixture must configure nothing but the account mount")

	t.Run("a nested mount", func(t *testing.T) {
		targets, err := parseMountinfoTargets([]byte(cleanMountinfo +
			"2441 2440 8:2 / /af-account/vault rw,relatime - ext4 /dev/sdb rw\n"))
		require.NoError(t, err)
		err = verifyResolvedAccountBoundary(targets, configured)
		require.Error(t, err, "af cannot tell a nested mount from an aliased one, so it must still refuse")
		assert.Contains(t, err.Error(), "/af-account/vault")
		assert.Contains(t, err.Error(), "nested mount point")
		assert.NotContains(t, err.Error(), "Remove that entry from docker.run_args",
			"there is no run_args entry to remove; telling the operator to edit one sends them hunting for nothing")
		assert.NotContains(t, err.Error(), "the selected image aliases",
			"nothing was configured, so nothing could have been aliased")
	})

	t.Run("residue from an earlier session", func(t *testing.T) {
		err := verifyAccountDeviceScan(
			[]byte("/af-account/planted\n"+accountDeviceScanSentinel+"\n"), configured, verifyAccountSource)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "residue left in the account directory by an earlier session")
		assert.Contains(t, err.Error(), verifyAccountSource+"/planted",
			"the operator has to remove it on the HOST, so name the host path")
		assert.NotContains(t, err.Error(), "Remove that entry from docker.run_args")
	})
}

// A node under the runtime home stays inside the container's own filesystem and
// goes away with it, so the refusal must not send the operator looking for it on
// the host.
func TestAccountRuntimeVerify_RuntimeHomeDeviceLeavesNoHostResidue(t *testing.T) {
	err := verifyAccountDeviceScan(
		[]byte("/af-home/planted\n"+accountDeviceScanSentinel+"\n"), []string{"/device-target/planted"}, verifyAccountSource)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/af-home/planted")
	assert.NotContains(t, err.Error(), verifyAccountSource,
		"/af-home is created inside the container, so nothing was written through the bind mount")
}

// The configured half is the backstop for what no resolution is needed to see:
// an entry Docker recorded straight onto the boundary. The lexical guard already
// refuses these from argv, so this holds the line for an option af's argv walk
// misreads or a future Docker spelling it has not been taught.
func TestAccountRuntimeVerify_ConfiguredSetIsTheBackstop(t *testing.T) {
	tests := []struct {
		name    string
		inspect string
		want    string
	}{
		{
			name: "a bind recorded under the account",
			inspect: `[{"Mounts":[
				{"Type":"bind","Source":"` + verifyAccountSource + `","Destination":"/af-account","RW":true},
				{"Type":"bind","Source":"/repo/evil","Destination":"/af-account/.config","RW":true}],
				"HostConfig":{}}]`,
			want: "/af-account/.config",
		},
		{
			name: "a tmpfs recorded on the runtime home",
			inspect: `[{"Mounts":[{"Type":"bind","Source":"` + verifyAccountSource + `","Destination":"/af-account","RW":true}],
				"HostConfig":{"Tmpfs":{"/af-home/x":""}}}]`,
			want: "/af-home/x",
		},
		{
			name: "a device recorded under the account",
			inspect: `[{"Mounts":[{"Type":"bind","Source":"` + verifyAccountSource + `","Destination":"/af-account","RW":true}],
				"HostConfig":{"Devices":[{"PathOnHost":"/dev/zero","PathInContainer":"/af-account/planted"}]}}]`,
			want: "/af-account/planted",
		},
		{
			name: "the account mount replaced by a foreign source",
			inspect: `[{"Mounts":[{"Type":"bind","Source":"/repo/evil","Destination":"/af-account","RW":true}],
				"HostConfig":{}}]`,
			want: "/repo/evil",
		},
		{
			name:    "no account mount at all",
			inspect: `[{"Mounts":[],"HostConfig":{}}]`,
			want:    "0 mounts at /af-account",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inspected, err := parseDockerInspectContainer([]byte(tt.inspect))
			require.NoError(t, err)
			err = verifyConfiguredAccountBoundary(inspected, verifyAccountSource)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

// A source af cannot READ is a refusal. A check that cannot see is not a check
// that passed — the failure mode this whole issue is an instance of.
func TestAccountRuntimeVerify_UnreadableSourceRefuses(t *testing.T) {
	t.Run("inspect", func(t *testing.T) {
		for _, raw := range []string{"", "not json", "[]", "[{},{}]", `{"Mounts":[]}`} {
			_, err := parseDockerInspectContainer([]byte(raw))
			require.Errorf(t, err, "unreadable `docker inspect` output %q must not read as a clean container", raw)
		}
	})
	t.Run("mountinfo", func(t *testing.T) {
		for _, raw := range []string{
			"",                                 // an empty read is not an empty mount table
			"\n\n",                             // and neither is whitespace
			"garbage",                          // nor a line with no fields
			"2440 2421 8:1 / /af-account",      // truncated: no options, no separator
			"1 2 0:1 / / rw ext4 /dev/root rw", // no "-" separator at all
		} {
			_, err := parseMountinfoTargets([]byte(raw))
			require.Errorf(t, err, "unreadable mountinfo %q must not read as a clean mount table", raw)
		}
	})
	t.Run("device scan", func(t *testing.T) {
		for _, raw := range []string{
			"",                                       // the scan never ran
			"\n",                                     // and an empty line is not a completion
			"find: /af-account: Permission denied\n", // it ran and failed
			"/af-account/planted\n",                  // it found something and then died
		} {
			require.Errorf(t, verifyAccountDeviceScan([]byte(raw), nil, ""), "an incomplete device scan %q must refuse", raw)
		}
	})
}

// The mount point is field 5 and the kernel octal-escapes it. A reader that
// takes the wrong field, or leaves the escapes in, reports the wrong path.
func TestParseMountinfoTargets_ReadsTheMountPointField(t *testing.T) {
	targets, err := parseMountinfoTargets([]byte(
		"2440 2421 8:1 /host/src /af-account rw,relatime - ext4 /dev/root rw\n" +
			"2441 2440 8:1 /host/e /af-account/my\\040dir rw,relatime shared:1 - ext4 /dev/root rw\n"))
	require.NoError(t, err)
	require.Equal(t, []string{"/af-account", "/af-account/my dir"}, targets,
		"field 5 is the mount point, and its octal escapes must be undone — field 4 is the source's own root")
}

// An escape the kernel never writes must not be silently truncated into a path
// character the source did not have. Three octal digits reach 0777, which does
// not fit a byte.
func TestParseMountinfoTargets_RefusesAnOutOfRangeEscape(t *testing.T) {
	_, err := parseMountinfoTargets([]byte(
		"2440 2421 8:1 /host/src /af-account\\777x rw,relatime - ext4 /dev/root rw\n"))
	require.Error(t, err, "byte(0777) truncates to 0xff and would invent a character; refuse instead")
	assert.Contains(t, err.Error(), "out of range")
}

// Nothing about this runs for a session with no account: the whole boundary
// exists only when one is mounted, and a non-account session must not pay for a
// docker inspect or two execs, let alone be refused by them.
func TestAccountRuntimeVerify_NonAccountSessionIsUntouched(t *testing.T) {
	var calls [][]string
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		return nil, fmt.Errorf("no docker call is expected for a session with no account")
	})()

	p := &dockerProvisioner{spec: ProvisionSpec{Title: "plain"}, containerID: "cid"}
	require.NoError(t, p.verifyAccountRuntimeBoundary())
	require.Empty(t, calls, "a session with no account must not reach docker at all")
}

// The whole check, wired: the provisioner reads both sources and the scan, in
// that order, and refuses on the aliased one.
func TestAccountRuntimeVerify_ReadsBothSourcesAndTheScan(t *testing.T) {
	dir := t.TempDir()
	var calls [][]string
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		switch {
		case args[0] == "inspect":
			return []byte(`[{"Mounts":[
				{"Type":"bind","Source":"` + dir + `","Destination":"/af-account","Mode":"z","RW":true},
				{"Type":"bind","Source":"/repo/evil","Destination":"/device-target/.config","RW":true}],
				"HostConfig":{}}]`), nil
		case args[0] == "exec" && args[len(args)-1] == "/proc/1/mountinfo":
			return []byte(cleanMountinfoFor(dir) +
				"2442 2440 8:1 /repo/evil /af-account/.config rw,relatime - ext4 /dev/root rw\n"), nil
		case args[0] == "exec":
			return []byte(cleanDeviceScan), nil
		}
		return nil, fmt.Errorf("unexpected docker call: %v", args)
	})()

	p := &dockerProvisioner{
		spec:        ProvisionSpec{Title: "scoped", Account: sessionenv.Account{Agent: "codex", Name: "work", Dir: dir}},
		containerID: "cid",
	}
	err := p.verifyAccountRuntimeBoundary()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/af-account/.config", "names where the alias landed")
	assert.Contains(t, err.Error(), "/device-target/.config", "and the run_args entry that aliased onto it")
	assert.Contains(t, err.Error(), `account "work"`, "and which account was refused")

	require.GreaterOrEqual(t, len(calls), 2)
	assert.Equal(t, "inspect", calls[0][0], "the daemon's own record is read first")
	assert.Contains(t, strings.Join(calls[1], " "), "/proc/1/mountinfo",
		"and the kernel's resolved view second — the half a symlink cannot survive")
}

// A read that FAILS is a refusal on every one of the three sources.
//
// Each source is broken in the way that a check ignoring exit status would sail
// straight through: docker reports a non-zero exit while the output it already
// printed reads as a perfectly clean container. Failing the read with unusable
// output would be caught by the content checks above and would prove nothing
// about whether the error itself is honoured.
func TestAccountRuntimeVerify_EveryFailedReadRefuses(t *testing.T) {
	dir := t.TempDir()
	for _, broken := range []string{"inspect", "mountinfo", "scan"} {
		t.Run(broken, func(t *testing.T) {
			fail := func(out string) ([]byte, error) {
				return []byte(out), fmt.Errorf("exit status 125")
			}
			defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
				switch {
				case args[0] == "inspect":
					if broken == "inspect" {
						return fail(cleanInspect(dir))
					}
					return []byte(cleanInspect(dir)), nil
				case args[len(args)-1] == "/proc/1/mountinfo":
					if broken == "mountinfo" {
						return fail(cleanMountinfoFor(dir))
					}
					return []byte(cleanMountinfoFor(dir)), nil
				default:
					if broken == "scan" {
						return fail(cleanDeviceScan)
					}
					return []byte(cleanDeviceScan), nil
				}
			})()
			p := &dockerProvisioner{
				spec:        ProvisionSpec{Title: "scoped", Account: sessionenv.Account{Agent: "codex", Name: "work", Dir: dir}},
				containerID: "cid",
			}
			require.Errorf(t, p.verifyAccountRuntimeBoundary(),
				"docker failed reading the %s, so af never saw that source; clean-looking output from a failed command is not a clean answer", broken)
		})
	}
}

// And the clean container passes through the wired path too, so the refusals
// above are not a check that refuses everything.
func TestAccountRuntimeVerify_WiredCleanContainerPasses(t *testing.T) {
	dir := t.TempDir()
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		switch {
		case args[0] == "inspect":
			return []byte(cleanInspect(dir)), nil
		case args[len(args)-1] == "/proc/1/mountinfo":
			return []byte(cleanMountinfoFor(dir)), nil
		default:
			return []byte(cleanDeviceScan), nil
		}
	})()
	p := &dockerProvisioner{
		spec:        ProvisionSpec{Title: "scoped", Account: sessionenv.Account{Agent: "codex", Name: "work", Dir: dir}},
		containerID: "cid",
	}
	require.NoError(t, p.verifyAccountRuntimeBoundary())
}

// A relative account directory must verify against the ABSOLUTE path Docker was
// given, or the check refuses the very mount af installed.
func TestAccountRuntimeVerify_MatchesTheAbsoluteMountSource(t *testing.T) {
	dir := t.TempDir()
	absolute, err := filepath.Abs(dir)
	require.NoError(t, err)
	inspected, err := parseDockerInspectContainer([]byte(cleanInspect(absolute)))
	require.NoError(t, err)
	require.NoError(t, verifyConfiguredAccountBoundary(inspected, absolute))
}

// cleanMountinfoFor is cleanMountinfo with the account bind's source swapped for
// a test's own directory. Only the mount POINT is read, so the source text is
// cosmetic — but a fixture whose two halves disagree invites the next reader to
// conclude the source matters.
func cleanMountinfoFor(source string) string {
	return strings.ReplaceAll(cleanMountinfo, verifyAccountSource, strings.TrimPrefix(source, "/"))
}

// A refusal must TEAR THE CONTAINER DOWN, not merely decline to use it.
//
// The container is already running with the account bind-mounted when this check
// fires, so a refusal that leaked it would leave the operator's credential
// directory mounted into a repository-selected image indefinitely — an outcome
// worse than the shadowing it just refused. Provision's reapProvisionFailure is
// what closes that, and this drives the whole path to prove the reap happens.
func TestAccountRuntimeVerify_RefusalReapsTheContainer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	accountDir := filepath.Join(home, "accounts", "codex", "work")
	require.NoError(t, os.MkdirAll(accountDir, 0o700))

	repoRoot := initTempGitRepo(t)
	writeInRepoConfig(t, repoRoot, map[string]any{
		"backend": "docker",
		"docker": map[string]any{
			"image": "aliasimg:latest",
			// The issue's own reproduction: a container path the lexical guard
			// has no reason to refuse, which the image symlinks onto the account.
			"run_args": []string{"-v", "/repo/evil:/device-target/.config"},
		},
	})
	defer SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil })()
	defer SetDockerSelfBinaryForTest(filepath.Join(t.TempDir(), "af"))()

	var calls [][]string
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		switch {
		case args[0] == "context":
			return []byte("unix:///var/run/docker.sock\n"), nil
		case args[0] == "info":
			return []byte("engine-1\n"), nil
		case args[0] == "run":
			return []byte(dockerCreatedID + "\n"), nil
		case args[0] == "inspect":
			return []byte(`[{"Mounts":[
				{"Type":"bind","Source":"` + accountDir + `","Destination":"/af-account","Mode":"z","RW":true},
				{"Type":"bind","Source":"/repo/evil","Destination":"/device-target/.config","RW":true}],
				"HostConfig":{}}]`), nil
		case args[0] == "exec" && args[len(args)-1] == "/proc/1/mountinfo":
			return []byte(cleanMountinfoFor(accountDir) +
				"2442 2440 8:1 /repo/evil /af-account/.config rw,relatime - ext4 /dev/root rw\n"), nil
		case args[0] == "exec":
			return []byte(cleanDeviceScan), nil
		case args[0] == "rm":
			return []byte(dockerCreatedID + "\n"), nil
		}
		return nil, fmt.Errorf("unexpected docker call: %v", args)
	})()

	_, err := dockerRuntime{}.Provision(ProvisionSpec{
		RepoRoot: repoRoot,
		Title:    "aliased",
		Program:  "codex",
		CloneURL: "file:///x",
		Account:  sessionenv.Account{Agent: "codex", Name: "work", Dir: accountDir},
	})
	require.Error(t, err, "an aliased account mount must refuse the create")
	assert.Contains(t, err.Error(), "/af-account/.config")
	assert.Contains(t, err.Error(), "/device-target/.config")

	require.Truef(t, sawDockerRm(calls, dockerCreatedID),
		"the refused container must be reaped, not left running with the account mounted; docker calls=%v", calls)

	// And the refusal reaches the operator BEFORE anything used the account: no
	// clone, no binary copy, no agent-server.
	for _, call := range calls {
		assert.NotEqual(t, "cp", call[0], "a refused session must not have had the af binary copied into it")
	}
}

// fakeAccountBoundaryDockerResponse answers the runtime boundary verification
// for a fake docker CLI, as the CLEAN container: af's own account mount at
// dockerAccountHome and nothing else anywhere near it.
//
// Tests about other account behaviour route their unmatched docker calls through
// it, because the check runs on every account provision and a fixture that
// cannot answer it refuses the session before those tests reach what they
// assert. It is deliberately a fixed clean answer rather than something derived
// from the call: a fake that echoed the request back would report whatever the
// production code asked for and could never fail open.
func fakeAccountBoundaryDockerResponse(args []string, accountDir string) ([]byte, error) {
	if len(args) == 0 {
		return nil, nil
	}
	if args[0] == "inspect" {
		return []byte(cleanInspect(accountDir)), nil
	}
	if args[0] == "exec" {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasSuffix(joined, "/proc/1/mountinfo"):
			return []byte(cleanMountinfoFor(accountDir)), nil
		case strings.Contains(joined, accountDeviceScanSentinel):
			return []byte(cleanDeviceScan), nil
		}
	}
	return nil, nil
}
