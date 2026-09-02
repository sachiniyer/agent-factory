package session

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

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

// cleanMountinfo is /proc/self/mountinfo from that same container. The account bind
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

// A container af provisioned and nobody tampered with must PASS all three
// checks. Without this the refusals below would be satisfied by a check that
// refuses everything.
func TestAccountRuntimeVerify_CleanContainerPasses(t *testing.T) {
	inspected, err := parseDockerInspectContainer([]byte(cleanInspect(verifyAccountSource)))
	require.NoError(t, err)
	require.NoError(t, verifyConfiguredAccountBoundary(inspected, verifyAccountSource, nil))

	targets, err := parseMountinfoTargets([]byte(cleanMountinfo))
	require.NoError(t, err)
	require.NoError(t, verifyResolvedAccountBoundary(targets, configuredMountPaths(inspected), verifyAccountSource))

	require.NoError(t, verifyAccountDeviceResidue(os.DirFS(t.TempDir()), verifyAccountSource, inspected.HostConfig.Devices))
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
		// residue is what af finds in the account directory ON THE HOST — a
		// --device is a mknod, so it lands in no mount table at all.
		residue        fstest.MapFS
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
			// A device is a mknod, so it appears in NO mount table. It DOES
			// appear in the operator's account directory on the host, written
			// through the account bind, which is where af looks for it.
			extraMountinfo: "",
			residue: fstest.MapFS{
				"planted": &fstest.MapFile{Mode: fs.ModeDevice | fs.ModeCharDevice},
			},
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
			require.NoError(t, verifyConfiguredAccountBoundary(inspected, verifyAccountSource, nil),
				"the configured set is lexically clean — that is the defect")
			configured := configuredMountPaths(inspected)

			targets, err := parseMountinfoTargets([]byte(cleanMountinfo + tt.extraMountinfo))
			require.NoError(t, err)
			err = verifyResolvedAccountBoundary(targets, configured, verifyAccountSource)
			if err == nil {
				err = verifyAccountDeviceResidue(tt.residue, verifyAccountSource, inspected.HostConfig.Devices)
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
	require.NoError(t, verifyConfiguredAccountBoundary(inspected, verifyAccountSource, nil))

	targets, err := parseMountinfoTargets([]byte(cleanMountinfo +
		"2683 2674 8:1 /var/lib/docker/volumes/anon/_data /af-account rw,relatime - ext4 /dev/root rw,discard\n"))
	require.NoError(t, err)
	err = verifyResolvedAccountBoundary(targets, configuredMountPaths(inspected), verifyAccountSource)
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
	configured := configuredMountPaths(inspected)
	require.Empty(t, configured, "the fixture must configure nothing but the account mount")

	t.Run("a nested mount", func(t *testing.T) {
		targets, err := parseMountinfoTargets([]byte(cleanMountinfo +
			"2441 2440 8:2 / /af-account/vault rw,relatime - ext4 /dev/sdb rw\n"))
		require.NoError(t, err)
		err = verifyResolvedAccountBoundary(targets, configured, verifyAccountSource)
		require.Error(t, err, "af cannot tell a nested mount from an aliased one, so it must still refuse")
		assert.Contains(t, err.Error(), "/af-account/vault")
		assert.Contains(t, err.Error(), "nested mount point")
		assert.NotContains(t, err.Error(), "Remove that entry from docker.run_args",
			"there is no run_args entry to remove; telling the operator to edit one sends them hunting for nothing")
		assert.NotContains(t, err.Error(), "the selected image aliases",
			"nothing was configured, so nothing could have been aliased")
	})

}

// A candidate list is not causation (#3602 review).
//
// The clone-source bind every docker session already carries is enough to make
// the configured list non-empty, so a refusal that reads a non-empty list as
// "the image aliased one of these" tells an operator whose real problem is a
// nested host mount to delete a valid run_arg — after which the session is still
// refused, for the reason the message did not name.
func TestAccountRuntimeVerify_DoesNotAssertWhichCauseItWas(t *testing.T) {
	// An ordinary docker session's own run_args: the clone source, nothing near
	// the account.
	inspect := `[{"Mounts":[
		{"Type":"bind","Source":"` + verifyAccountSource + `","Destination":"/af-account","Mode":"z","RW":true},
		{"Type":"bind","Source":"/host/repo.git","Destination":"/repo.git","RW":false}],
		"HostConfig":{}}]`
	inspected, err := parseDockerInspectContainer([]byte(inspect))
	require.NoError(t, err)
	configured := configuredMountPaths(inspected)
	require.Equal(t, []string{"/repo.git"}, configured)

	// …and a mount inside the boundary that the clone source cannot explain.
	targets, err := parseMountinfoTargets([]byte(cleanMountinfo +
		"2441 2440 8:2 / /af-account/vault rw,relatime - ext4 /dev/sdb rw\n"))
	require.NoError(t, err)
	err = verifyResolvedAccountBoundary(targets, configured, verifyAccountSource)
	require.Error(t, err)

	message := err.Error()
	assert.Contains(t, message, "nested mount point",
		"the host-side cause must survive a non-empty candidate list — it is the one a run_args edit cannot fix")
	assert.Contains(t, message, "/repo.git",
		"the candidates are still worth naming, as candidates")
	assert.Contains(t, message, "cannot tell these apart",
		"and the refusal must say it is offering alternatives rather than a diagnosis")
	assert.NotContains(t, message, "the selected image aliases",
		"af did not establish that, and asserting it sends the operator to remove a valid run_arg")
}

// The residue walk classifies on Info(), not on DirEntry.Type(). Type() is
// permitted to be the zero FileMode when a directory read cannot say, and zero
// read as "not a device" is a fail-OPEN: the node is there and the walk reports
// a clean account.
//
// os.DirFS does not produce that today — Go lstats a DT_UNKNOWN entry before
// handing it back — so this presents an fs.FS that does, which is the only way
// to hold the property rather than the current behaviour of another package.
func TestAccountDeviceResidue_DoesNotTrustAnUnknownDirEntryType(t *testing.T) {
	found, err := accountDeviceResidue(unknownTypeFS{
		"planted":   fs.ModeDevice | fs.ModeCharDevice,
		"auth.json": 0,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"planted"}, found,
		"a directory read that cannot say what an entry is must not read as 'not a device'")
}

// unknownTypeFS is a filesystem whose directory entries report an UNKNOWN type
// (the zero FileMode) while Info() tells the truth — the shape a network, FUSE
// or older XFS filesystem presents through DT_UNKNOWN.
type unknownTypeFS map[string]fs.FileMode

func (f unknownTypeFS) Open(name string) (fs.File, error) { return nil, fs.ErrNotExist }

func (f unknownTypeFS) Stat(name string) (fs.FileInfo, error) {
	if name == "." {
		return unknownTypeInfo{name: ".", mode: fs.ModeDir}, nil
	}
	mode, ok := f[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return unknownTypeInfo{name: name, mode: mode}, nil
}

func (f unknownTypeFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name != "." {
		return nil, fs.ErrNotExist
	}
	names := make([]string, 0, len(f))
	for entry := range f {
		names = append(names, entry)
	}
	sort.Strings(names)
	entries := make([]fs.DirEntry, 0, len(names))
	for _, entry := range names {
		entries = append(entries, unknownTypeEntry{fsys: f, name: entry})
	}
	return entries, nil
}

type unknownTypeEntry struct {
	fsys unknownTypeFS
	name string
}

func (e unknownTypeEntry) Name() string { return e.name }
func (e unknownTypeEntry) IsDir() bool  { return false }

// The whole point: the read cannot say, so it says nothing.
func (e unknownTypeEntry) Type() fs.FileMode { return 0 }

func (e unknownTypeEntry) Info() (fs.FileInfo, error) { return e.fsys.Stat(e.name) }

type unknownTypeInfo struct {
	name string
	mode fs.FileMode
}

func (i unknownTypeInfo) Name() string       { return i.name }
func (i unknownTypeInfo) Size() int64        { return 0 }
func (i unknownTypeInfo) Mode() fs.FileMode  { return i.mode }
func (i unknownTypeInfo) ModTime() time.Time { return time.Time{} }
func (i unknownTypeInfo) IsDir() bool        { return i.mode.IsDir() }
func (i unknownTypeInfo) Sys() any           { return nil }

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
			name: "a volume where af installed a bind",
			inspect: `[{"Mounts":[{"Type":"volume","Name":"anon","Source":"/var/lib/docker/volumes/anon/_data","Destination":"/af-account","RW":true}],
				"HostConfig":{}}]`,
			want: "volume",
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
			err = verifyConfiguredAccountBoundary(inspected, verifyAccountSource, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

// The mount at dockerAccountHome is the account af named — decided by DIRECTORY
// identity, never by string equality (#3602 review).
//
// ensureAccountDockerEngineLocal accepts unix:// and npipe:// endpoints, which
// include Docker Desktop, and a daemon that lives in a VM can report its own
// spelling of the client's path. Refusing on a mismatch there would refuse every
// account session on that engine — a worse outcome than the one the check was
// guarding — so af refuses only what it can establish: a source this host
// resolves to a DIFFERENT directory.
func TestClassifyAccountMountSource(t *testing.T) {
	account := t.TempDir()
	other := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(account, link))

	tests := []struct {
		name           string
		recorded, want string
		verdict        accountSourceVerdict
	}{
		{"the same spelling", account, account, accountSourceSame},
		{"the same spelling, uncleaned", account + "/.", account, accountSourceSame},
		{"the same directory through a symlink", link, account, accountSourceSame},
		{"a different directory on this host", other, account, accountSourceForeign},
		{"a path this host cannot resolve", "/run/desktop/mnt/host/c/Users/op/.agent-factory", account, accountSourceUnresolvable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.verdict, classifyAccountMountSource(tt.recorded, tt.want))
		})
	}
}

// A source this host cannot resolve WARNS and continues. A daemon translating
// client paths is not evidence of a wrong account, and treating it as one takes
// the whole feature away on that engine.
func TestAccountRuntimeVerify_TranslatedSourceWarnsRatherThanRefuses(t *testing.T) {
	account := t.TempDir()
	inspect := `[{"Mounts":[{"Type":"bind","Source":"/run/desktop/mnt/host/wsl/af","Destination":"/af-account","Mode":"z","RW":true}],
		"HostConfig":{}}]`
	inspected, err := parseDockerInspectContainer([]byte(inspect))
	require.NoError(t, err)

	var warnings []string
	err = verifyConfiguredAccountBoundary(inspected, account, func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	})
	require.NoError(t, err, "a source this host cannot resolve is not evidence the account is wrong")
	require.Len(t, warnings, 1, "but it must be said out loud, not swallowed")
	assert.Contains(t, warnings[0], "/run/desktop/mnt/host/wsl/af")
	assert.Contains(t, warnings[0], account)
}

// A source this host CAN resolve, to a different directory, is the one case af
// can actually establish — and it still refuses.
func TestAccountRuntimeVerify_ForeignLocalSourceStillRefuses(t *testing.T) {
	account := t.TempDir()
	other := t.TempDir()
	inspect := `[{"Mounts":[{"Type":"bind","Source":"` + other + `","Destination":"/af-account","Mode":"z","RW":true}],
		"HostConfig":{}}]`
	inspected, err := parseDockerInspectContainer([]byte(inspect))
	require.NoError(t, err)
	err = verifyConfiguredAccountBoundary(inspected, account, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), other)
	assert.Contains(t, err.Error(), account)
}

// Every check runs, and every finding reaches the operator (#3602 review).
//
// A container can carry an aliased mount AND an aliased --device at once. The
// mount is a refusal; the device node is a root-created special file sitting in
// the operator's account directory that reaping the container does not remove.
// An operator told only about the first never learns to go delete the second.
func TestAccountRuntimeVerify_ReportsEveryFindingNotJustTheFirst(t *testing.T) {
	dir := t.TempDir()
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		switch {
		case args[0] == "inspect":
			return []byte(`[{"Mounts":[
				{"Type":"bind","Source":"` + dir + `","Destination":"/af-account","Mode":"z","RW":true},
				{"Type":"bind","Source":"/repo/evil","Destination":"/device-target/.config","RW":true}],
				"HostConfig":{"Devices":[{"PathOnHost":"/dev/zero","PathInContainer":"/device-target/planted"}]}}]`), nil
		case args[len(args)-1] == "/proc/self/mountinfo":
			return []byte(cleanMountinfoFor(dir) +
				"2442 2440 8:1 /repo/evil /af-account/.config rw,relatime - ext4 /dev/root rw\n"), nil
		}
		return nil, fmt.Errorf("unexpected docker call: %v", args)
	})()

	// The node the daemon already planted, through the account bind, before any
	// check could run. A real one cannot be mknod'd by a test, so the walk is
	// given a filesystem that reports one.
	restore := setAccountFSForTest(func(string) fs.FS {
		return fstest.MapFS{"planted": &fstest.MapFile{Mode: fs.ModeDevice | fs.ModeCharDevice}}
	})
	defer restore()

	p := &dockerProvisioner{
		spec:        ProvisionSpec{Title: "scoped", Account: sessionenv.Account{Agent: "codex", Name: "work", Dir: dir}},
		containerID: "cid",
	}
	err := p.verifyAccountRuntimeBoundary()
	require.Error(t, err)
	message := err.Error()
	assert.Contains(t, message, "/af-account/.config", "the aliased mount")
	assert.Contains(t, message, "/af-account/planted",
		"AND the device node the reap will not remove — reporting only the first leaves it on the operator's disk")
	assert.Contains(t, message, filepath.Join(dir, "planted"), "named where they must remove it")
}

// An aliased bind whose destination did not exist is a mountpoint Docker created
// as root, through the account bind, before any check ran — so it survives the
// reap this refusal triggers, and the refusal has to name it on the HOST.
func TestAccountRuntimeVerify_NamesTheHostMountpointResidue(t *testing.T) {
	targets, err := parseMountinfoTargets([]byte(cleanMountinfo +
		"2442 2440 8:1 /repo/evil /af-account/planted-dir rw,relatime - ext4 /dev/root rw\n"))
	require.NoError(t, err)
	err = verifyResolvedAccountBoundary(targets, []string{"/device-target/planted-dir"}, verifyAccountSource)
	require.Error(t, err)
	assert.Contains(t, err.Error(), verifyAccountSource+"/planted-dir",
		"the container-side path is no help on a host where it does not exist")
	assert.Contains(t, err.Error(), "if a path there was not already yours",
		"and af must not tell an operator to delete a path that may hold their own credentials")

	// Nothing under the runtime home reaches the host, so nothing is claimed.
	targets, err = parseMountinfoTargets([]byte(cleanMountinfo +
		"2442 2421 8:1 /repo/evil /af-home/x rw,relatime - ext4 /dev/root rw\n"))
	require.NoError(t, err)
	err = verifyResolvedAccountBoundary(targets, []string{"/device-target/x"}, verifyAccountSource)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "mountpoint on this host",
		"/af-home is created inside the container; nothing there is on the host to remove")
}

// The mount table af reads is the EXEC PROCESS'S, never PID 1's (#3602 review).
//
// docker.run_args may set --pid=host, which the account validator permits, and
// PID 1 is then the HOST's init: its mount table has no /af-account anywhere, so
// the resolved check would read "af's own account mount is missing" and refuse
// every such session. Measured on Docker 29.4.0 — 55 lines, zero matches. docker
// exec joins the container's MOUNT namespace whatever its PID namespace, so the
// reader's own table is the container's either way.
func TestAccountRuntimeVerify_ReadsTheExecProcessMountNamespace(t *testing.T) {
	dir := t.TempDir()
	var read []string
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		switch {
		case args[0] == "inspect":
			return []byte(cleanInspect(dir)), nil
		case args[0] == "exec":
			read = append(read, args[len(args)-1])
			return []byte(cleanMountinfoFor(dir)), nil
		}
		return nil, fmt.Errorf("unexpected docker call: %v", args)
	})()
	p := &dockerProvisioner{
		spec:        ProvisionSpec{Title: "scoped", Account: sessionenv.Account{Agent: "codex", Name: "work", Dir: dir}},
		containerID: "cid",
	}
	require.NoError(t, p.verifyAccountRuntimeBoundary())
	require.Equal(t, []string{"/proc/self/mountinfo"}, read,
		"PID 1 is the HOST's init under --pid=host, and its mount table has no account mount to find")
}

// A source af cannot READ is a refusal. A check that cannot see is not a check
// that passed — the failure mode this whole issue is an instance of.
func TestAccountRuntimeVerify_UnreadableSourceRefuses(t *testing.T) {
	t.Run("inspect", func(t *testing.T) {
		for _, raw := range []string{"", "not json", "[]", "[{},{}]", `{"Mounts":[]}`} {
			_, err := parseDockerInspectContainer([]byte(raw))
			require.Errorf(t, err, "unreadable `docker inspect` output %q must not read as a clean container", raw)
		}
		// And the refusal shows what it got, bounded. A bare JSON error leaves
		// the operator guessing at a stream they cannot see.
		_, err := parseDockerInspectContainer([]byte("WARNING: something\n[{}]"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "WARNING: something")
		_, err = parseDockerInspectContainer([]byte(strings.Repeat("x", 5000)))
		require.Error(t, err)
		assert.Less(t, len(err.Error()), 500, "an unexpected megabyte of output must not become the message")
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
	t.Run("the account directory", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "gone")
		err := verifyAccountDeviceResidue(os.DirFS(missing), missing,
			[]dockerInspectDevice{{PathOnHost: "/dev/zero", PathInContainer: "/device-target/planted"}})
		require.Error(t, err,
			"an account directory af cannot read is not an account directory it proved holds no planted device")
		require.Contains(t, err.Error(), "cannot read account directory")
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

// The whole check, wired: the provisioner reads both container sources, in that
// order, and refuses on the aliased one.
func TestAccountRuntimeVerify_ReadsBothSources(t *testing.T) {
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
		case args[0] == "exec" && args[len(args)-1] == "/proc/self/mountinfo":
			return []byte(cleanMountinfoFor(dir) +
				"2442 2440 8:1 /repo/evil /af-account/.config rw,relatime - ext4 /dev/root rw\n"), nil
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
	assert.Contains(t, strings.Join(calls[1], " "), "/proc/self/mountinfo",
		"and the kernel's resolved view second — the half a symlink cannot survive")
	assert.NotContains(t, strings.Join(calls[1], " "), "/proc/1/mountinfo",
		"SELF, never PID 1: --pid=host makes PID 1 the host's init, whose mount table has no /af-account at all")
}

// EVERY protected mount is reported, not the first (#3602 review).
//
// An image can alias two configured destinations onto two different paths under
// the account, and Docker may have created BOTH mountpoints through the account
// bind before any check ran. An operator handed only the first can remove
// exactly what the refusal named and still be left with a root-created directory
// in their credential directory.
func TestAccountRuntimeVerify_ReportsEveryProtectedMount(t *testing.T) {
	targets, err := parseMountinfoTargets([]byte(cleanMountinfo +
		"2442 2440 8:1 /repo/evil /af-account/first rw,relatime - ext4 /dev/root rw\n" +
		"2443 2440 8:1 /repo/evil2 /af-account/second rw,relatime - ext4 /dev/root rw\n" +
		"2444 2421 0:90 / /af-home/third rw,relatime - tmpfs tmpfs rw\n"))
	require.NoError(t, err)
	err = verifyResolvedAccountBoundary(targets,
		[]string{"/device-target/first", "/device-target/second"}, verifyAccountSource)
	require.Error(t, err)

	message := err.Error()
	for _, want := range []string{"/af-account/first", "/af-account/second", "/af-home/third"} {
		assert.Containsf(t, message, want, "every protected target must be named, not only the first")
	}
	for _, want := range []string{verifyAccountSource + "/first", verifyAccountSource + "/second"} {
		assert.Containsf(t, message, want,
			"and every host path they may have left behind, or the operator removes one and keeps the other")
	}
	assert.NotContains(t, message, verifyAccountSource+"/third",
		"/af-home is created inside the container, so nothing there reaches the host")
	assert.Contains(t, message, "Their mountpoints on this host are",
		"plural, because reading 'its mountpoint' next to two paths reads as one of them")
}

// Two independent observations about the same boundary are both reported: a
// foreign mount under the account, and a count at the account root that is not
// one.
func TestAccountRuntimeVerify_ReportsBothResolvedObservations(t *testing.T) {
	targets, err := parseMountinfoTargets([]byte(cleanMountinfo +
		"2442 2440 8:1 /repo/evil /af-account rw,relatime - ext4 /dev/root rw\n" +
		"2443 2440 8:1 /repo/evil2 /af-account/under rw,relatime - ext4 /dev/root rw\n"))
	require.NoError(t, err)
	err = verifyResolvedAccountBoundary(targets, []string{"/device-target"}, verifyAccountSource)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/af-account/under")
	assert.Contains(t, err.Error(), "2 filesystems at /af-account")
}

// A --device destination is not a candidate for an aliased MOUNT, and an
// ordinary bind is not a candidate for a planted NODE (#3602 review).
//
// The two diagnoses draw from different lists because the two options do
// different things: a --device makes a node by mknod and installs no filesystem
// at all, so it cannot be what a mount resolved through; a bind or tmpfs cannot
// make a node. Crossing them sends the operator to delete a valid argument that
// had nothing to do with the finding.
func TestAccountRuntimeVerify_CandidateListsDoNotCrossOptionKinds(t *testing.T) {
	inspect := `[{"Mounts":[
		{"Type":"bind","Source":"` + verifyAccountSource + `","Destination":"/af-account","Mode":"z","RW":true},
		{"Type":"bind","Source":"/host/repo.git","Destination":"/repo.git","RW":false}],
		"HostConfig":{"Tmpfs":{"/scratch":""},
		"Devices":[{"PathOnHost":"/dev/fuse","PathInContainer":"/dev/fuse"}]}}]`
	inspected, err := parseDockerInspectContainer([]byte(inspect))
	require.NoError(t, err)

	configured := configuredMountPaths(inspected)
	require.Equal(t, []string{"/repo.git", "/scratch"}, configured,
		"a --device installs no filesystem, so it is not a candidate for an aliased mount")

	targets, err := parseMountinfoTargets([]byte(cleanMountinfo +
		"2442 2440 8:1 /repo/evil /af-account/.config rw,relatime - ext4 /dev/root rw\n"))
	require.NoError(t, err)
	err = verifyResolvedAccountBoundary(targets, configured, verifyAccountSource)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/repo.git")
	assert.NotContains(t, err.Error(), "/dev/fuse",
		"telling an operator their GPU or FUSE passthrough installed a filesystem sends them to remove a valid argument")

	// And the mirror: the device diagnosis names only what was asked for a NODE.
	err = verifyAccountDeviceResidue(fstest.MapFS{
		"planted": &fstest.MapFile{Mode: fs.ModeDevice | fs.ModeCharDevice},
	}, verifyAccountSource, inspected.HostConfig.Devices)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/dev/fuse")
	assert.NotContains(t, err.Error(), "/repo.git",
		"an ordinary bind cannot mknod, so it is not a candidate for a planted node")
}

// The residue walk runs on EVERY account provision, not only when this container
// configured a --device (#3602 review).
//
// The node the daemon plants outlives the session that planted it — written as
// root at container start, before any check can run, and the refusal that finds
// it reaps a container without touching the host. So the operator's retry, which
// has typically had the offending run_arg removed and therefore configures no
// device at all, is exactly the provision that must still find it.
func TestAccountRuntimeVerify_ScansForResidueLeftByAnEarlierSession(t *testing.T) {
	planted := fstest.MapFS{
		".config/settings.json": &fstest.MapFile{Mode: fs.ModeDevice | fs.ModeCharDevice},
	}

	// The retry: no --device configured anywhere, and the account still corrupt.
	err := verifyAccountDeviceResidue(planted, verifyAccountSource, nil)
	require.Error(t, err,
		"a session that configures no device is precisely the retry after the refusal, and the node is still there")
	assert.Contains(t, err.Error(), "/af-account/.config/settings.json",
		"and it is an ACTIVE device where the agent expects its settings")
	assert.Contains(t, err.Error(), verifyAccountSource+"/.config/settings.json",
		"named on the host, which is the only place it can be removed")
	assert.Contains(t, err.Error(), "an earlier session may have left it")
	assert.NotContains(t, err.Error(), "this session asked Docker to create a device node at",
		"this session asked for none, so offering that as a cause sends the operator after a line that is not there")

	// And when this session DID configure one, that candidate is offered too —
	// but only --device entries, never an ordinary bind, which cannot make a node.
	err = verifyAccountDeviceResidue(planted, verifyAccountSource, []dockerInspectDevice{
		{PathOnHost: "/dev/zero", PathInContainer: "/device-target/planted"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/device-target/planted")

	// A clean account still passes, so the above is not a check that refuses all.
	require.NoError(t, verifyAccountDeviceResidue(fstest.MapFS{
		"auth.json": &fstest.MapFile{Data: []byte("{}")},
	}, verifyAccountSource, nil))
}

// The walk classifies by mode, not by name, and does not follow symlinks — an
// entry is an entry, never a door.
func TestAccountDeviceResidue_ClassifiesByMode(t *testing.T) {
	found, err := accountDeviceResidue(fstest.MapFS{
		"auth.json":             &fstest.MapFile{Data: []byte("{}")},
		".config/settings.json": &fstest.MapFile{Data: []byte("{}")},
		".config/block":         &fstest.MapFile{Mode: fs.ModeDevice},
		"nested/deep/char":      &fstest.MapFile{Mode: fs.ModeDevice | fs.ModeCharDevice},
		"link":                  &fstest.MapFile{Mode: fs.ModeSymlink, Data: []byte("/dev/zero")},
	})
	require.NoError(t, err)
	require.Equal(t, []string{".config/block", "nested/deep/char"}, found,
		"both character and block nodes at any depth, and nothing that merely points at one")
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
	for _, broken := range []string{"inspect", "mountinfo"} {
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
				case args[len(args)-1] == "/proc/self/mountinfo":
					if broken == "mountinfo" {
						return fail(cleanMountinfoFor(dir))
					}
					return []byte(cleanMountinfoFor(dir)), nil
				}
				return nil, fmt.Errorf("unexpected docker call: %v", args)
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
		case args[len(args)-1] == "/proc/self/mountinfo":
			return []byte(cleanMountinfoFor(dir)), nil
		}
		return nil, fmt.Errorf("unexpected docker call: %v", args)
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
	require.NoError(t, verifyConfiguredAccountBoundary(inspected, absolute, nil))
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
		case args[0] == "exec" && args[len(args)-1] == "/proc/self/mountinfo":
			return []byte(cleanMountinfoFor(accountDir) +
				"2442 2440 8:1 /repo/evil /af-account/.config rw,relatime - ext4 /dev/root rw\n"), nil
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
	if args[0] == "exec" && strings.HasSuffix(strings.Join(args, " "), "/proc/self/mountinfo") {
		return []byte(cleanMountinfoFor(accountDir)), nil
	}
	return nil, nil
}

// setAccountFSForTest swaps the filesystem the residue walk reads and returns a
// restore func, so a test can present a real character-device mode.
func setAccountFSForTest(f func(string) fs.FS) func() {
	previous := accountFS
	accountFS = f
	return func() { accountFS = previous }
}
