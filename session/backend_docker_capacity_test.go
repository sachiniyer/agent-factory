package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// fakeUnixDockerSocket stands up a real unix socket in a temp dir so the
// mount-namespace gate has something genuinely dialable; it returns the
// DOCKER_HOST endpoint form.
func fakeUnixDockerSocket(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets are unavailable on windows")
	}
	// macOS's sun_path tops out near 104 bytes — a t.TempDir() path under
	// /var/folders/... overflows it (`bind: invalid argument`); /tmp stays short.
	dir, err := os.MkdirTemp("/tmp", "af-sock-")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { l.Close() })
	return "unix://" + sock
}

// stubPeerDataRoot installs the three seams the data-root identity proof
// consults: socketPeerPid reports the answering process as pid 424242 (or
// fails when peerErr is set), af's own stat of the data root reports
// "maj:min" dev, and peerMountinfo answers peerTable for the peer and
// selfTable for af's own pid — "" simulates an unreadable table. Proof
// requires the peer's covering mount to show the same device af's stat
// reports, or — when the peer's table is unreadable — af's own table
// covering the root on a whole filesystem.
func stubPeerDataRoot(t *testing.T, peerUID int, dev, peerTable, selfTable string, peerErr error) {
	t.Helper()
	var maj, min uint64
	_, err := fmt.Sscanf(dev, "%d:%d", &maj, &min)
	require.NoError(t, err)
	oldP, oldM, oldS, oldE := socketPeerCred, peerMountinfo, statDev, peerExe
	socketPeerCred = func(net.Conn) (int, int, error) {
		if peerErr != nil {
			return 0, 0, peerErr
		}
		return 424242, peerUID, nil
	}
	peerMountinfo = func(pid int) ([]byte, error) {
		table := peerTable
		if pid != 424242 {
			table = selfTable
		}
		if table == "" {
			return nil, fmt.Errorf("process gone")
		}
		return []byte(table), nil
	}
	// peerExe mirrors the real gate: readable exactly when the peer's own
	// table is. Tests that need a non-dockerd peer re-stub after this.
	peerExe = func(pid int) (string, error) {
		if pid == 424242 && peerTable != "" {
			return "/usr/bin/dockerd", nil
		}
		return "", fmt.Errorf("process gone")
	}
	// No listening-socket holders by default — a directly-bound dockerd
	// never reaches the scan (its own exe answers first).
	oldH := listenerHolders
	listenerHolders = func(string) (map[int]struct{}, bool) {
		return map[int]struct{}{}, true
	}
	statDev = func(string) (uint64, bool) { return unix.Mkdev(uint32(maj), uint32(min)), true }
	t.Cleanup(func() {
		socketPeerCred, peerMountinfo, statDev, peerExe, listenerHolders =
			oldP, oldM, oldS, oldE, oldH
	})
}

// peerTableOn returns a mountinfo whose deepest mount covering the data
// root reports device dev — the proven shape when dev matches af's stat.
func peerTableOn(dev string) string {
	return "24 1 " + dev + " / / rw,relatime - ext4 /dev/sda1 rw\n"
}

// peerTableSubvol is a mountinfo whose data-root covering mount has the
// fs-internal subtree root a mounted subvolume reports (Btrfs `/@`):
// legitimately non-"/", yet still the whole local filesystem.
func peerTableSubvol(dev, root, mp string) string {
	return "24 1 " + dev + " " + root + " " + mp + " rw,relatime - btrfs /dev/sda1 rw\n"
}

// dockerInfoAnswer is the fake `docker info` payload for capacity-evidence
// tests. The production evidence path issues one `docker info --format
// '{{json .}}'` snapshot and reads DockerRootDir, Name and SecurityOptions;
// fakes dispatch on the --format value so the same fake also answers the
// engine-id lookup ({{.ID}}) and the pre-#5382 single-field contract
// ({{.DockerRootDir}}).
func dockerInfoAnswer(t *testing.T, root, daemonName string, rootless bool) []byte {
	t.Helper()
	sec := []string{"name=seccomp,profile=builtin"}
	if rootless {
		sec = append(sec, "name=rootless")
	}
	blob, err := json.Marshal(map[string]any{
		"DockerRootDir":   root,
		"Name":            daemonName,
		"SecurityOptions": sec,
	})
	require.NoError(t, err)
	return blob
}

// fakeDockerCapacityProbe installs the docker CLI fake the capacity-evidence
// tests share. info answers the engine-id format, the json format, and the
// single-field root-dir format; every other argv goes to dispatch.
func fakeDockerCapacityProbe(t *testing.T, infoPayload []byte,
	dispatch func(args []string) ([]byte, error)) (calls *[][]string) {
	t.Helper()
	seen := [][]string{}
	calls = &seen
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		seen = append(seen, append([]string(nil), args...))
		if args[0] == "info" {
			format := ""
			for i, a := range args {
				if a == "--format" && i+1 < len(args) {
					format = args[i+1]
				}
			}
			switch {
			case strings.Contains(format, "json"):
				return infoPayload, nil
			case strings.Contains(format, "DockerRootDir"):
				// The pre-#5382 contract: `docker info --format
				// '{{.DockerRootDir}}'` answered with the bare path.
				var raw struct {
					DockerRootDir string `json:"DockerRootDir"`
				}
				_ = json.Unmarshal(infoPayload, &raw)
				return []byte(raw.DockerRootDir + "\n"), nil
			default:
				return []byte("engine-capacity-test\n"), nil
			}
		}
		// The non-account provision probes the image's default USER via `id
		// -u`/`id -g`; a root default (0:0) makes prepareNonAccountUser a
		// no-op so these capacity probes reach the step each case targets
		// instead of failing on an unparseable probe.
		if args[0] == "exec" && containsArg(args, "id -u") {
			return []byte("0:0\n"), nil
		}
		return dispatch(args)
	}))
	return calls
}

// TestProvisionFailure_JoinsDataRootFullEvidenceEveryStep is the #5382 item-4
// regression: `docker run` is not the only provisioning step that can hit
// ENOSPC — the data root fills during configureGit, the clone, the af-binary
// copy, agent-server startup, or the port read too. Every step's failure must
// carry the statfs-derived ENOSPC evidence, not only runContainer's.
func TestProvisionFailure_JoinsDataRootFullEvidenceEveryStep(t *testing.T) {
	host, err := os.Hostname()
	require.NoError(t, err)
	t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
	t.Setenv("DOCKER_CONTEXT", "")
	stubPeerDataRoot(t, 0, "8:1", peerTableOn("8:1"), peerTableOn("8:1"), nil)

	// Each failingStep names a docker argv shape emitted by one provision
	// step; matching it makes the fake fail that call. The matchers read argv
	// the way the steps emit it: the detached exec carries a literal "-d"
	// flag, the banner poll is the one non-detached exec whose script carries
	// the liveness marker — the banner path alone also appears inside
	// startAgentServer's redirect script.
	failingSteps := map[string]func(args []string) bool{
		"docker run":         func(args []string) bool { return args[0] == "run" },
		"configureGit":       func(args []string) bool { return args[0] == "exec" && containsArg(args, "user.email") },
		"cloneWorkspace":     func(args []string) bool { return args[0] == "exec" && containsArg(args, "git clone") },
		"copyAfBinary cp":    func(args []string) bool { return args[0] == "cp" },
		"copyAfBinary chmod": func(args []string) bool { return args[0] == "exec" && containsArg(args, "chmod") },
		"startAgentServer":   func(args []string) bool { return args[0] == "exec" && exactArg(args, "-d") },
		// A banner read whose every `docker exec` fails — the daemon-side
		// exec state is itself a data-root write, so the fs verdict applies.
		"readBanner": func(args []string) bool {
			return args[0] == "exec" && !exactArg(args, "-d") && containsArg(args, dockerBannerPath) && containsArg(args, remoteProcAlive)
		},
		// `docker port` is read-only and its parse failures are af's own —
		// local errors get their own regression test below.
	}

	for name, failing := range failingSteps {
		t.Run(name, func(t *testing.T) {
			if name == "readBanner" {
				// readBanner polls until its deadline — shrink it so the
				// failing exec reaches the error path fast.
				oldT, oldI := dockerBannerPollTimeout, dockerBannerPollInterval
				dockerBannerPollTimeout, dockerBannerPollInterval = 60*time.Millisecond, 10*time.Millisecond
				defer func() { dockerBannerPollTimeout, dockerBannerPollInterval = oldT, oldI }()
			}
			fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false),
				func(args []string) ([]byte, error) {
					if failing(args) {
						return []byte("Error response from daemon: no space left on device\n"), fmt.Errorf("exit status 125")
					}
					if args[0] == "run" {
						return []byte(dockerCreatedID + "\n"), nil
					}
					if args[0] == "exec" && containsArg(args, dockerBannerPath) {
						return []byte(`{"addr":":8000","token":"t","title":"x"}`), nil
					}
					if args[0] == "port" {
						return []byte("127.0.0.1:49152\n"), nil
					}
					return nil, nil
				})

			statfsCalls := 0
			old := statfsFull
			statfsFull = func(string) (bool, error) { statfsCalls++; return true, nil }
			defer func() { statfsFull = old }()

			p := &dockerProvisioner{
				image: "img:latest",
				afBin: "/tmp/test-af",
				spec:  ProvisionSpec{Title: "full-root", CloneURL: "file:///repo"},
			}
			_, err := p.provision()
			require.Error(t, err)
			assert.True(t, errors.Is(err, syscall.ENOSPC),
				"%s: a failed provisioning step on a full data root must carry typed ENOSPC evidence: %v", name, err)
			assert.Positive(t, statfsCalls, "%s: the data root was never probed", name)
		})
	}
}

func containsArg(args []string, needle string) bool {
	for _, a := range args {
		if strings.Contains(a, needle) {
			return true
		}
	}
	return false
}

func exactArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// TestParseDockerEngineInfo_CombinedOutput pins the parse against the
// dockerExec seam's reality: CombinedOutput merges stderr into the buffer, and
// `docker info` prints daemon warnings there — including "Running in
// rootless-mode", which lands on exactly the engines item 5 targets. The
// parser must skip leading noise to the JSON object; a buffer with no JSON
// object is unproven, not malformed evidence.
func TestParseDockerEngineInfo_CombinedOutput(t *testing.T) {
	blob := dockerInfoAnswer(t, "/home/u/.local/share/docker", "myhost", true)

	info, ok := parseDockerEngineInfo(blob)
	require.True(t, ok)
	assert.Equal(t, "/home/u/.local/share/docker", info.root)
	assert.Equal(t, "myhost", info.name)
	assert.True(t, info.rootless)

	withWarnings := append([]byte("WARNING: API is accessible on http://0.0.0.0:2375 without encryption\nWARNING: Running in rootless-mode\n"), blob...)
	info, ok = parseDockerEngineInfo(withWarnings)
	require.True(t, ok, "daemon warnings must not defeat the parse — rootless engines always print one")
	assert.Equal(t, "myhost", info.name)
	assert.True(t, info.rootless)

	_, ok = parseDockerEngineInfo([]byte("docker: Error response from daemon: boom\n"))
	assert.False(t, ok, "no JSON object means no engine info — unproven, not parsed")
}

// TestProvisionFailure_RootlessEngineReadsBavail is the #5382 item-5 join: a
// rootless dockerd writes the data root as an ordinary user — the filesystem's
// root-reserved blocks are out of its reach — so the probe must consult the
// unprivileged fullness answer (Bavail), not the privileged one (Bfree). An
// engine proven rootful by SecurityOptions reads Bfree.
func TestProvisionFailure_RootlessEngineReadsBavail(t *testing.T) {
	host, err := os.Hostname()
	require.NoError(t, err)
	t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
	t.Setenv("DOCKER_CONTEXT", "")
	stubPeerDataRoot(t, 0, "8:1", peerTableOn("8:1"), peerTableOn("8:1"), nil)

	failRun := func(args []string) ([]byte, error) {
		if args[0] == "run" {
			return []byte("Error response from daemon: no space left on device\n"), fmt.Errorf("exit status 125")
		}
		return nil, fmt.Errorf("unexpected docker call: %v", args)
	}

	t.Run("rootless engine is probed with the unprivileged answer", func(t *testing.T) {
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/home/u/.local/share/docker", host, true), failRun)
		privCalls, unprivCalls := 0, 0
		oldPriv, oldUnpriv := statfsFull, statfsFullUnprivileged
		statfsFull = func(string) (bool, error) { privCalls++; return false, nil }
		statfsFullUnprivileged = func(string) (bool, error) { unprivCalls++; return true, nil }
		defer func() { statfsFull, statfsFullUnprivileged = oldPriv, oldUnpriv }()

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "rootless"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.True(t, errors.Is(err, syscall.ENOSPC),
			"a full rootless data root must carry ENOSPC evidence: %v", err)
		assert.Equal(t, 1, unprivCalls, "rootless engines are measured on Bavail")
		assert.Equal(t, 0, privCalls, "rootless engines must not consult the privileged answer")
	})

	t.Run("rootful engine is probed with the privileged answer", func(t *testing.T) {
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)
		privCalls, unprivCalls := 0, 0
		oldPriv, oldUnpriv := statfsFull, statfsFullUnprivileged
		statfsFull = func(string) (bool, error) { privCalls++; return true, nil }
		statfsFullUnprivileged = func(string) (bool, error) { unprivCalls++; return false, nil }
		defer func() { statfsFull, statfsFullUnprivileged = oldPriv, oldUnpriv }()

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "rootful"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.True(t, errors.Is(err, syscall.ENOSPC),
			"a full rootful data root must carry ENOSPC evidence: %v", err)
		assert.Equal(t, 1, privCalls, "rootful engines are measured on Bfree")
		assert.Equal(t, 0, unprivCalls, "rootful engines must not consult the unprivileged answer")
	})
}

// TestProvisionFailure_ProbesFullnessBeforeReap pins the item-4 ordering: the
// capacity evidence must be gathered while the failed container still exists —
// reaping it frees the partial layer data and can mask the very fullness that
// caused the failure. Provision tears down after the evidence joins.
func TestProvisionFailure_ProbesFullnessBeforeReap(t *testing.T) {
	host, err := os.Hostname()
	require.NoError(t, err)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
	t.Setenv("DOCKER_CONTEXT", "")
	stubPeerDataRoot(t, 0, "8:1", peerTableOn("8:1"), peerTableOn("8:1"), nil)
	repoRoot := initTempGitRepo(t)
	writeInRepoConfig(t, repoRoot, map[string]any{"backend": "docker", "docker": map[string]any{"image": "img:latest"}})
	defer SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil })()
	defer SetDockerSelfBinaryForTest("/tmp/test-af")()

	var events []string
	fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false),
		func(args []string) ([]byte, error) {
			events = append(events, "docker:"+args[0])
			switch {
			case args[0] == "run":
				return []byte(dockerCreatedID + "\n"), nil
			case args[0] == "exec" && containsArg(args, "git clone"):
				return []byte("fatal: no space left on device\n"), fmt.Errorf("exit status 128")
			case args[0] == "rm":
				return []byte(dockerCreatedID + "\n"), nil
			}
			return nil, nil
		})

	old := statfsFull
	statfsFull = func(string) (bool, error) { events = append(events, "statfs"); return true, nil }
	defer func() { statfsFull = old }()

	_, err = dockerRuntime{}.Provision(ProvisionSpec{RepoRoot: repoRoot, Title: "order", CloneURL: "file:///x"})
	require.Error(t, err)
	require.True(t, errors.Is(err, syscall.ENOSPC),
		"the clone's ENOSPC must surface as typed evidence: %v", err)
	statfsAt, rmAt := -1, -1
	for i, e := range events {
		if e == "statfs" && statfsAt < 0 {
			statfsAt = i
		}
		if e == "docker:rm" && rmAt < 0 {
			rmAt = i
		}
	}
	require.GreaterOrEqual(t, statfsAt, 0, "the data root was never probed: %v", events)
	require.GreaterOrEqual(t, rmAt, 0, "the failed container was never reaped: %v", events)
	assert.Less(t, statfsAt, rmAt,
		"the fullness probe must run BEFORE the failed container is reaped (its partial data frees on reap): %v", events)
}

// TestProvisionFailure_UnprivilegedWriterReadsBavail is the #5382-review
// metric-selection regression: on a ROOTFUL engine the reserved-block window
// (Bfree > 0, Bavail == 0) is full for an unprivileged writer but not for
// the daemon. The failed step's op class decides which counter answers —
// a non-root session exec joins the Bavail verdict, while `docker run`'s
// daemon-side write must not: inventing fullness for a writer that can still
// allocate would park a structural failure on the environment forever.
func TestProvisionFailure_UnprivilegedWriterReadsBavail(t *testing.T) {
	host, err := os.Hostname()
	require.NoError(t, err)
	t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
	t.Setenv("DOCKER_CONTEXT", "")
	stubPeerDataRoot(t, 0, "8:1", peerTableOn("8:1"), peerTableOn("8:1"), nil)

	// failExec kills the configureGit exec; failRun kills `docker run`.
	failExec := func(args []string) ([]byte, error) {
		if args[0] == "exec" && containsArg(args, "user.email") {
			return []byte("error: could not write config file: No space left on device\n"), fmt.Errorf("exit status 3")
		}
		if args[0] == "run" {
			return []byte(dockerCreatedID + "\n"), nil
		}
		return nil, nil
	}
	failRun := func(args []string) ([]byte, error) {
		if args[0] == "run" {
			return []byte("Error response from daemon: no space left on device\n"), fmt.Errorf("exit status 125")
		}
		return nil, nil
	}

	// reservedWindow installs the Bavail-only-exhausted statfs pair: the
	// privileged answer says "not full" (Bfree > 0 covers the reserved
	// blocks) while the unprivileged answer reports exhaustion.
	reservedWindow := func(t *testing.T) {
		t.Helper()
		oldPriv, oldUnpriv := statfsFull, statfsFullUnprivileged
		statfsFull = func(string) (bool, error) { return false, nil }
		statfsFullUnprivileged = func(string) (bool, error) { return true, nil }
		t.Cleanup(func() { statfsFull, statfsFullUnprivileged = oldPriv, oldUnpriv })
	}

	// containerIdentity answers the `docker inspect` the writer-privilege
	// check performs — the container's RESOLVED user, its userns mode, and
	// any mounts that could carry a writer's own credentials onto the
	// probed filesystem. `docker run --user`/`--userns=host` land in the
	// first two; the third is the overlay-bypass proof.
	containerIdentityDest := func(dest, user, usernsMode string, mounts ...string) func(args []string) ([]byte, error) {
		return func(args []string) ([]byte, error) {
			if args[0] == "inspect" {
				var ms strings.Builder
				for i, src := range mounts {
					if i > 0 {
						ms.WriteString(",")
					}
					fmt.Fprintf(&ms, `{"Type":"bind","Source":%q,"Destination":%q}`, src, dest)
				}
				return []byte(fmt.Sprintf(`{"Config":{"User":%q},"HostConfig":{"UsernsMode":%q},"Mounts":[%s]}`,
					user, usernsMode, ms.String())), nil
			}
			return failExec(args)
		}
	}
	containerIdentity := func(user, usernsMode string, mounts ...string) func(args []string) ([]byte, error) {
		return containerIdentityDest("/workspace", user, usernsMode, mounts...)
	}

	// stubSameFilesystem fakes the st_dev comparison — the mount source is
	// synthetic, so the "on the data-root filesystem" verdict is stubbed
	// rather than arranged as real mount geometry.
	stubSameFilesystem := func(t *testing.T, same bool) {
		t.Helper()
		old := sameFilesystem
		sameFilesystem = func(string, string) bool { return same }
		t.Cleanup(func() { sameFilesystem = old })
	}

	t.Run("account-owned session exec joins the Bavail verdict", func(t *testing.T) {
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false),
			containerIdentity("", "", "/var/lib/docker/af-account"))
		reservedWindow(t)
		stubSameFilesystem(t, true)
		p := &dockerProvisioner{
			image:         "img:latest",
			containerUser: "1000:1000", // a mounted account owned by uid 1000
			spec:          ProvisionSpec{Title: "acct", CloneURL: "file:///repo"},
		}
		_, err := p.provision()
		require.Error(t, err)
		assert.True(t, errors.Is(err, syscall.ENOSPC),
			"an exec writing as uid 1000 onto a data-root bind IS Bavail-bounded — its ENOSPC must surface: %v", err)
	})

	t.Run("daemon-side write does not borrow Bavail on rootful", func(t *testing.T) {
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)
		reservedWindow(t)
		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "daemon-write"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"docker run's writer is the rootful daemon — Bfree>0 means its writes can still allocate: %v", err)
	})

	t.Run("root-owned session exec does not borrow Bavail", func(t *testing.T) {
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failExec)
		reservedWindow(t)
		p := &dockerProvisioner{
			image:         "img:latest",
			containerUser: "0:0",
			spec:          ProvisionSpec{Title: "acct-root", CloneURL: "file:///repo"},
		}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"a uid-0 exec reaches the reserved blocks — Bavail=0 is not full for it: %v", err)
	})

	t.Run("container-resolved non-root user joins the Bavail verdict", func(t *testing.T) {
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false),
			containerIdentity("1000", "", "/var/lib/docker/worktree"))
		reservedWindow(t)
		stubSameFilesystem(t, true)
		p := &dockerProvisioner{
			image: "img:latest",
			spec:  ProvisionSpec{Title: "img-user", CloneURL: "file:///repo"},
		}
		_, err := p.provision()
		require.Error(t, err)
		assert.True(t, errors.Is(err, syscall.ENOSPC),
			"an exec under a non-root container user writing a data-root bind is Bavail-bounded: %v", err)
	})

	t.Run("exec without a data-root bind is mounter-credentialed", func(t *testing.T) {
		// Codex's overlay finding: a session exec writing the container's
		// own rootfs goes through overlayfs, which accesses the upper with
		// the DAEMON's mounter credentials — the reserved window is not
		// this writer's ENOSPC, and no bind means no counterexample.
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false),
			containerIdentity("1000", ""))
		reservedWindow(t)
		stubSameFilesystem(t, true)
		p := &dockerProvisioner{
			image: "img:latest",
			spec:  ProvisionSpec{Title: "overlay-only", CloneURL: "file:///repo"},
		}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"overlay-only writes ride the daemon's credentials — Bavail=0 is not this writer's ENOSPC: %v", err)
	})

	t.Run("a bind on another filesystem is not the probed one", func(t *testing.T) {
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false),
			containerIdentity("1000", "", "/mnt/other-fs"))
		reservedWindow(t)
		stubSameFilesystem(t, false)
		p := &dockerProvisioner{
			image: "img:latest",
			spec:  ProvisionSpec{Title: "other-fs", CloneURL: "file:///repo"},
		}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"the bind lives on a different filesystem — its writes cannot produce this ENOSPC: %v", err)
	})

	t.Run("`docker run --user 0` override is privileged", func(t *testing.T) {
		// Codex's example: a non-root image overridden by `--user 0` in
		// docker.run_args — the container's Config.User is "0", and the
		// image's own USER must not mislabel the exec.
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false),
			containerIdentity("0", ""))
		reservedWindow(t)
		p := &dockerProvisioner{
			image: "img:latest",
			spec:  ProvisionSpec{Title: "user-override", CloneURL: "file:///repo"},
		}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"a uid-0 exec via run_args --user reaches reserved blocks — Bavail=0 is not full for it: %v", err)
	})

	t.Run("unreadable container user fails closed to Bfree", func(t *testing.T) {
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false),
			func(args []string) ([]byte, error) {
				if args[0] == "inspect" {
					return nil, fmt.Errorf("Error: No such container")
				}
				return failExec(args)
			})
		reservedWindow(t)
		p := &dockerProvisioner{
			image: "img:latest",
			spec:  ProvisionSpec{Title: "img-unknown", CloneURL: "file:///repo"},
		}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"an unproven container user is not evidence of unprivileged writes: %v", err)
	})

	usernsInfo := func(t *testing.T) []byte {
		info := dockerInfoAnswer(t, "/var/lib/docker", host, false)
		var raw map[string]any
		require.NoError(t, json.Unmarshal(info, &raw))
		raw["SecurityOptions"] = []string{"name=seccomp,profile=builtin", "name=userns"}
		info, err := json.Marshal(raw)
		require.NoError(t, err)
		return info
	}

	t.Run("`root:root` container user is privileged", func(t *testing.T) {
		// The resolved user can carry a group — root:root is still uid 0,
		// and the reserved-block window is not its ENOSPC.
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false),
			containerIdentity("root:root", "", "/var/lib/docker/worktree"))
		reservedWindow(t)
		stubSameFilesystem(t, true)
		p := &dockerProvisioner{
			image: "img:latest",
			spec:  ProvisionSpec{Title: "root-group", CloneURL: "file:///repo"},
		}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"a uid-0 exec reaches the reserved blocks whatever group it carries: %v", err)
	})

	t.Run("userns remap makes even a uid-0 exec Bavail-bounded", func(t *testing.T) {
		// `docker info` reports name=userns when the engine remaps container
		// uids into a subordinate range — every in-container writer, uid 0
		// included, is an ordinary user on the data-root filesystem. The
		// bind still has to exist: remap or not, overlay writes ride the
		// daemon's credentials.
		fakeDockerCapacityProbe(t, usernsInfo(t), containerIdentity("0", "", "/var/lib/docker/worktree"))
		reservedWindow(t)
		stubSameFilesystem(t, true)
		p := &dockerProvisioner{
			image:         "img:latest",
			containerUser: "0:0",
			spec:          ProvisionSpec{Title: "userns", CloneURL: "file:///repo"},
		}
		_, err = p.provision()
		require.Error(t, err)
		assert.True(t, errors.Is(err, syscall.ENOSPC),
			"under userns remap a uid-0 exec writing a data-root bind is an unprivileged writer: %v", err)
	})

	t.Run("`--userns=host` opts the container out of remap", func(t *testing.T) {
		// The daemon-wide remap is a DEFAULT — HostConfig.UsernsMode=host on
		// this container cancels it, so a uid-0 exec is genuinely privileged
		// and the reserved window is not its ENOSPC.
		fakeDockerCapacityProbe(t, usernsInfo(t), containerIdentity("0", "host", "/var/lib/docker/worktree"))
		reservedWindow(t)
		stubSameFilesystem(t, true)
		p := &dockerProvisioner{
			image:         "img:latest",
			containerUser: "0:0",
			spec:          ProvisionSpec{Title: "userns-host", CloneURL: "file:///repo"},
		}
		_, err = p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"a container that opted out of remap writes as real uid 0: %v", err)
	})

	vfsInfo := func(t *testing.T) []byte {
		info := dockerInfoAnswer(t, "/var/lib/docker", host, false)
		var raw map[string]any
		require.NoError(t, json.Unmarshal(info, &raw))
		raw["Driver"] = "vfs"
		info, err := json.Marshal(raw)
		require.NoError(t, err)
		return info
	}

	t.Run("vfs driver makes every exec write self-credentialed", func(t *testing.T) {
		// vfs has no overlay mounter: the writable layer is plain
		// directories under the data root, so a non-root exec's write hits
		// the filesystem with its own fsuid whether or not a bind exists —
		// the Bavail window IS its ENOSPC (#5382 review).
		fakeDockerCapacityProbe(t, vfsInfo(t), containerIdentity("1000", ""))
		reservedWindow(t)
		p := &dockerProvisioner{
			image: "img:latest",
			spec:  ProvisionSpec{Title: "vfs-user", CloneURL: "file:///repo"},
		}
		_, err := p.provision()
		require.Error(t, err)
		assert.True(t, errors.Is(err, syscall.ENOSPC),
			"on vfs a non-root exec writes its own layer with its own creds — no bind needed: %v", err)
	})

	t.Run("a bind off the write roots is not bypass evidence", func(t *testing.T) {
		// A credential or cache bind that shares the data-root filesystem
		// but mounts somewhere the failed step never writes — /srv/cache —
		// proves nothing about where THIS exec wrote. Treating it as the
		// bypass would hand every exec failure the Bavail window (#5382
		// review).
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false),
			containerIdentityDest("/srv/cache", "1000", "", "/var/lib/docker/cache"))
		reservedWindow(t)
		stubSameFilesystem(t, true)
		p := &dockerProvisioner{
			image: "img:latest",
			spec:  ProvisionSpec{Title: "off-target-bind", CloneURL: "file:///repo"},
		}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"an unrelated bind sharing the fs is not evidence the step wrote through it: %v", err)
	})

	t.Run("a bind covering a write root counts", func(t *testing.T) {
		// The positive half of the destination check: a bind mounted AT
		// the workspace is exactly where the failed exec wrote.
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false),
			containerIdentity("1000", "", "/var/lib/docker/worktree"))
		reservedWindow(t)
		stubSameFilesystem(t, true)
		p := &dockerProvisioner{
			image: "img:latest",
			spec:  ProvisionSpec{Title: "on-target-bind", CloneURL: "file:///repo"},
		}
		_, err := p.provision()
		require.Error(t, err)
		assert.True(t, errors.Is(err, syscall.ENOSPC),
			"a data-root bind over the workspace is the bypass the session exec wrote through: %v", err)
	})
}

// TestProvisionFailure_ContainerdStoreRootIsProbed is the #5382-review
// containerd-image-store coverage: on Engine 29's default layout snapshots
// and the metadata bolt db live under the containerd root, so a daemon-side
// write can fail ENOSPC on a filesystem DockerRootDir doesn't describe. The
// conventional root is probed through the same co-location proof — and only
// when the engine reports the containerd snapshotter.
func TestProvisionFailure_ContainerdStoreRootIsProbed(t *testing.T) {
	host, err := os.Hostname()
	require.NoError(t, err)
	t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
	t.Setenv("DOCKER_CONTEXT", "")
	stubPeerDataRoot(t, 0, "8:1", peerTableOn("8:1"), peerTableOn("8:1"), nil)

	containerdInfo := func(t *testing.T) []byte {
		info := dockerInfoAnswer(t, "/var/lib/docker", host, false)
		var raw map[string]any
		require.NoError(t, json.Unmarshal(info, &raw))
		raw["DriverStatus"] = [][]string{{"driver-type", "io.containerd.snapshotter.v1"}}
		info, err := json.Marshal(raw)
		require.NoError(t, err)
		return info
	}
	failRun := func(args []string) ([]byte, error) {
		if args[0] == "run" {
			return []byte("Error response from daemon: no space left on device\n"), fmt.Errorf("exit status 125")
		}
		return nil, nil
	}

	t.Run("a full containerd root fails the same docker run", func(t *testing.T) {
		fakeDockerCapacityProbe(t, containerdInfo(t), failRun)
		old := statfsFull
		statfsFull = func(dir string) (bool, error) { return dir == dockerContainerdRoot, nil }
		t.Cleanup(func() { statfsFull = old })
		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "ctr-store"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.True(t, errors.Is(err, syscall.ENOSPC),
			"a full containerd store root must produce the same verdict as a full data root: %v", err)
		assert.Contains(t, err.Error(), dockerContainerdRoot,
			"the evidence must name the filesystem that is actually full")
	})

	t.Run("a non-containerd engine never probes the containerd root", func(t *testing.T) {
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)
		var probed []string
		old := statfsFull
		statfsFull = func(dir string) (bool, error) { probed = append(probed, dir); return true, nil }
		t.Cleanup(func() { statfsFull = old })
		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "ov2"}}
		_, err := p.provision()
		require.Error(t, err)
		require.True(t, errors.Is(err, syscall.ENOSPC))
		assert.NotContains(t, probed, dockerContainerdRoot,
			"an engine without the containerd image store has no second write root to probe")
	})
}

// TestProvisionFailure_LocalStepFailureGetsNoEvidence is the #5382-review
// false-full regression: the provision-boundary join must only run for
// failures that could have been a data-root write. An unparsable `docker
// port` reply (or the read-only port call itself failing) on a
// coincidentally-full root must NOT borrow ENOSPC — that would park a
// structural failure on the environment's unbounded retry budget.
func TestProvisionFailure_LocalStepFailureGetsNoEvidence(t *testing.T) {
	host, err := os.Hostname()
	require.NoError(t, err)
	t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
	t.Setenv("DOCKER_CONTEXT", "")
	stubPeerDataRoot(t, 0, "8:1", peerTableOn("8:1"), peerTableOn("8:1"), nil)

	cases := map[string]func(args []string) ([]byte, error){
		"port output unparsable": func(args []string) ([]byte, error) {
			switch {
			case args[0] == "run":
				return []byte(dockerCreatedID + "\n"), nil
			case args[0] == "exec" && containsArg(args, dockerBannerPath):
				return []byte(`{"addr":":8000","token":"t","title":"x"}`), nil
			case args[0] == "port":
				return []byte("not-a-port\n"), nil // succeeds, unparseable
			}
			return nil, nil
		},
		"port call fails": func(args []string) ([]byte, error) {
			switch {
			case args[0] == "run":
				return []byte(dockerCreatedID + "\n"), nil
			case args[0] == "exec" && containsArg(args, dockerBannerPath):
				return []byte(`{"addr":":8000","token":"t","title":"x"}`), nil
			case args[0] == "port":
				return nil, fmt.Errorf("exit status 1") // read-only round-trip
			}
			return nil, nil
		},
	}

	for name, dispatch := range cases {
		t.Run(name, func(t *testing.T) {
			fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), dispatch)
			old := statfsFull
			statfsFull = func(string) (bool, error) { return true, nil }
			defer func() { statfsFull = old }()

			p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "local-failure"}}
			_, err := p.provision()
			require.Error(t, err)
			assert.False(t, errors.Is(err, syscall.ENOSPC),
				"%s: a non-write failure on a full root must stay structural, not borrow ENOSPC: %v", name, err)
		})
	}
}

// TestDockerStepWriterUnprivileged pins the per-class privilege matrix the
// evidence path consults, for the cases that do not need a docker
// round-trip (container-inspect cases are covered at provision level).
func TestDockerStepWriterUnprivileged(t *testing.T) {
	p := &dockerProvisioner{}
	info := dockerEngineInfo{}

	assert.False(t, p.dockerStepWriterUnprivileged(info, dockerOpLocal))
	assert.False(t, p.dockerStepWriterUnprivileged(info, dockerOpDaemonWrite),
		"the daemon writes as its own uid — root on a rootful engine")
	assert.False(t, p.dockerStepWriterUnprivileged(dockerEngineInfo{userns: true}, dockerOpDaemonWrite),
		"userns remaps CONTAINER uids — the rootful daemon's own writes stay privileged")

	// Exec classes with no inspectable container fail closed: an
	// unprivileged writer only matters once a data-root bind proves its
	// writes bypass the overlay's mounter credentials.
	assert.False(t, p.dockerStepWriterUnprivileged(info, dockerOpSessionExec))
	assert.False(t, p.dockerStepWriterUnprivileged(info, dockerOpRootExec))
	p.containerUser = "1000:1000"
	assert.False(t, p.dockerStepWriterUnprivileged(info, dockerOpSessionExec),
		"no proven bind on the probed fs — overlay writes ride the daemon's creds")

	// The uid spelling the inspect layer can produce.
	for _, u := range []string{"", "0", "0:0", "root", "root:root"} {
		assert.False(t, dockerUserUnprivileged(u), "%q is uid 0", u)
	}
	assert.True(t, dockerUserUnprivileged("1000:1000"))
	assert.True(t, dockerUserUnprivileged("agent"))
}

// TestProvisionFailure_BannerTimeoutWriterClass pins the banner-timeout
// classification rules from #5382 review: (a) a timeout whose polls kept
// FAILING is the session writer's failure — the exec state is a daemon
// write, but the never-created banner belongs to the session user's
// redirect, so the Bavail window must be consulted via the session-exec
// class; and (b) a timeout whose polls all SUCCEEDED on an empty file is
// the same class — a readable banner proves the writer's redirect landed,
// not that the writer's own write could populate it.
func TestProvisionFailure_BannerTimeoutWriterClass(t *testing.T) {
	host, err := os.Hostname()
	require.NoError(t, err)
	t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
	t.Setenv("DOCKER_CONTEXT", "")
	stubPeerDataRoot(t, 0, "8:1", peerTableOn("8:1"), peerTableOn("8:1"), nil)
	oldT, oldI := dockerBannerPollTimeout, dockerBannerPollInterval
	dockerBannerPollTimeout, dockerBannerPollInterval = 60*time.Millisecond, 10*time.Millisecond
	defer func() { dockerBannerPollTimeout, dockerBannerPollInterval = oldT, oldI }()

	reservedWindow := func(t *testing.T) {
		t.Helper()
		oldPriv, oldUnpriv := statfsFull, statfsFullUnprivileged
		statfsFull = func(string) (bool, error) { return false, nil }
		statfsFullUnprivileged = func(string) (bool, error) { return true, nil }
		t.Cleanup(func() { statfsFull, statfsFullUnprivileged = oldPriv, oldUnpriv })
	}
	stubSameFilesystem := func(t *testing.T, same bool) {
		t.Helper()
		old := sameFilesystem
		sameFilesystem = func(string, string) bool { return same }
		t.Cleanup(func() { sameFilesystem = old })
	}

	t.Run("exec-failed banner timeout is the session writer's failure", func(t *testing.T) {
		// Every `cat` fails because the session user's redirect never
		// created the banner — the writer is unprivileged and a data-root
		// bind is mounted, so the reserved-block window IS its ENOSPC.
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false),
			func(args []string) ([]byte, error) {
				switch {
				case args[0] == "run":
					return []byte(dockerCreatedID + "\n"), nil
				case args[0] == "inspect":
					return []byte(`{"Config":{"User":"1000"},"HostConfig":{"UsernsMode":""},` +
						`"Mounts":[{"Type":"bind","Source":"/var/lib/docker/wt","Destination":"/workspace"}]}`), nil
				case args[0] == "exec":
					return nil, fmt.Errorf("exit status 1")
				}
				return nil, nil
			})
		reservedWindow(t)
		stubSameFilesystem(t, true)
		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "banner-writer"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.True(t, errors.Is(err, syscall.ENOSPC),
			"the banner timeout must consult the session writer's Bavail, not only the daemon's Bfree: %v", err)
	})

	t.Run("a clean-but-empty read still classifies writer-side", func(t *testing.T) {
		// Every poll's `cat` SUCCEEDS but the banner stays empty — the
		// startup shell's redirect can create the file while the
		// agent-server's own write dies ENOSPC (and RunAgentServer does
		// not propagate it). A readable file is not a populated one —
		// the timeout keeps the session-writer class (#5382 review).
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false),
			func(args []string) ([]byte, error) {
				switch {
				case args[0] == "run":
					return []byte(dockerCreatedID + "\n"), nil
				case args[0] == "inspect":
					return []byte(`{"Config":{"User":"1000"},"HostConfig":{"UsernsMode":""},` +
						`"Mounts":[{"Type":"bind","Source":"/var/lib/docker/wt","Destination":"/workspace"}]}`), nil
				case args[0] == "exec":
					return []byte("\n"), nil
				}
				return nil, nil
			})
		reservedWindow(t)
		stubSameFilesystem(t, true)
		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "banner-empty"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.True(t, errors.Is(err, syscall.ENOSPC),
			"an existing-but-empty banner still means the session writer could not populate it: %v", err)
	})
}

// TestAccountRuntimeVerify_ExecFailureCarriesWriteClass pins the #5382-review
// classification: the boundary check's `docker exec --user 0:0 cat
// /proc/self/mountinfo` records exec state under the data root — a
// daemon-side bookkeeping write the engine can fail with ENOSPC — so the
// error carries the daemon-write class for the provision boundary: it
// joins the fs verdict but can never borrow the reserved-block window
// (the cat itself writes nothing under the account bind).
func TestAccountRuntimeVerify_ExecFailureCarriesWriteClass(t *testing.T) {
	dir := t.TempDir()
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		switch {
		case args[0] == "inspect":
			return []byte(cleanInspect(dir)), nil
		case args[0] == "exec":
			return nil, fmt.Errorf("exit status 1")
		}
		return nil, fmt.Errorf("unexpected docker call: %v", args)
	})()
	p := &dockerProvisioner{
		spec:        ProvisionSpec{Title: "scoped", Account: sessionenv.Account{Agent: "codex", Name: "work", Dir: dir}},
		containerID: "cid",
	}
	err := p.verifyAccountRuntimeBoundary()
	require.Error(t, err)
	var se *dockerStepError
	require.True(t, errors.As(err, &se), "the exec failure must carry an operation class")
	assert.Equal(t, dockerOpDaemonWrite, se.op,
		"the mountinfo exec's state write is dockerd's bookkeeping, not a container-side write")
}

// TestAccountRuntimeVerify_ExecFailureNeverBorrowsBavail is the #5382-review
// scenario end to end: a userns-remapped engine, a session with an account
// bind, an image without `cat`, and a data root whose reserved-block window
// is open (Bavail==0, Bfree>0). The verify exec's only data-root writes are
// dockerd's own bookkeeping — privileged — so the Bavail window must NOT
// produce a verdict: a failure that is not a full disk must not wait on the
// environment.
func TestAccountRuntimeVerify_ExecFailureNeverBorrowsBavail(t *testing.T) {
	host, err := os.Hostname()
	require.NoError(t, err)
	dir := t.TempDir()
	t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
	t.Setenv("DOCKER_CONTEXT", "")
	stubPeerDataRoot(t, 0, "8:1", peerTableOn("8:1"), "", nil)

	info := dockerInfoAnswer(t, "/var/lib/docker", host, false)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(info, &raw))
	raw["SecurityOptions"] = []string{"name=seccomp,profile=builtin", "name=userns"}
	info, err = json.Marshal(raw)
	require.NoError(t, err)

	fakeDockerCapacityProbe(t, info, func(args []string) ([]byte, error) {
		switch args[0] {
		case "run":
			return []byte("cid\n"), nil
		case "inspect":
			return []byte(cleanInspect(dir)), nil
		case "exec": // the image has no cat — the exec bookkeeping fails
			return nil, fmt.Errorf("exit status 127")
		}
		return nil, fmt.Errorf("unexpected docker call: %v", args)
	})
	// Bavail==0, Bfree>0 — the reserved-block window, reachable only for an
	// unprivileged writer. A privileged (daemon) write sees the free blocks.
	oldPriv, oldUnpriv := statfsFull, statfsFullUnprivileged
	statfsFull = func(string) (bool, error) { return false, nil }
	statfsFullUnprivileged = func(string) (bool, error) { return true, nil }
	defer func() { statfsFull, statfsFullUnprivileged = oldPriv, oldUnpriv }()

	p := &dockerProvisioner{
		image: "img:latest",
		spec: ProvisionSpec{Title: "scoped",
			Account: sessionenv.Account{Agent: "codex", Name: "work", Dir: dir}},
	}
	_, err = p.provision()
	require.Error(t, err)
	assert.False(t, errors.Is(err, syscall.ENOSPC),
		"dockerd's own exec bookkeeping is a privileged write — the Bavail window is not its ENOSPC: %v", err)
}

// TestPrepareAccountUser_OwnerReadCarriesDaemonWriteClass pins the #5382
// finding: the `stat -c %u:%g` owner read is a read-only exec — an image
// missing `stat` fails without attempting an unprivileged write under the
// account bind. Its only data-root writes are dockerd's privileged exec
// bookkeeping, so the failure must carry the daemon-write class and never
// become eligible for the Bavail window.
func TestPrepareAccountUser_OwnerReadCarriesDaemonWriteClass(t *testing.T) {
	dir := t.TempDir()
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if args[0] == "exec" && strings.Contains(strings.Join(args, " "), "stat -c %u:%g") {
			return nil, fmt.Errorf("exit status 127") // the image has no stat
		}
		return nil, fmt.Errorf("unexpected docker call: %v", args)
	})()
	p := &dockerProvisioner{
		spec:        ProvisionSpec{Title: "scoped", Account: sessionenv.Account{Agent: "codex", Name: "work", Dir: dir}},
		containerID: "cid",
	}
	err := p.prepareAccountUser()
	require.Error(t, err)
	var se *dockerStepError
	require.True(t, errors.As(err, &se), "the owner-read failure must carry an operation class")
	assert.Equal(t, dockerOpDaemonWrite, se.op,
		"a failed owner READ attempted no unprivileged write — it is dockerd bookkeeping, not a session writer")
}

// TestPrepareAccountUser_OwnerParseIsLocal pins the other half: a stat
// output af cannot parse is af's own failure — nothing reached a writer,
// so the error stays local.
func TestPrepareAccountUser_OwnerParseIsLocal(t *testing.T) {
	dir := t.TempDir()
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if args[0] == "exec" && strings.Contains(strings.Join(args, " "), "stat -c %u:%g") {
			return []byte("not-a-uid-gid\n"), nil
		}
		return nil, fmt.Errorf("unexpected docker call: %v", args)
	})()
	p := &dockerProvisioner{
		spec:        ProvisionSpec{Title: "scoped", Account: sessionenv.Account{Agent: "codex", Name: "work", Dir: dir}},
		containerID: "cid",
	}
	err := p.prepareAccountUser()
	require.Error(t, err)
	var se *dockerStepError
	require.True(t, errors.As(err, &se), "the owner-parse failure must carry an operation class")
	assert.Equal(t, dockerOpLocal, se.op,
		"unparseable stat output is af's failure, never a filesystem verdict")
}

// TestAccountOwnerRead_NeverBorrowsBavail is the finding's scenario end to
// end: userns-remapped engine, account bind, an image without `stat`, and
// the reserved-block window open (Bavail==0, Bfree>0). The owner read is a
// daemon-side bookkeeping write, so the window must NOT produce a verdict.
func TestAccountOwnerRead_NeverBorrowsBavail(t *testing.T) {
	host, err := os.Hostname()
	require.NoError(t, err)
	dir := t.TempDir()
	t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
	t.Setenv("DOCKER_CONTEXT", "")
	stubPeerDataRoot(t, 0, "8:1", peerTableOn("8:1"), "", nil)

	info := dockerInfoAnswer(t, "/var/lib/docker", host, false)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(info, &raw))
	raw["SecurityOptions"] = []string{"name=seccomp,profile=builtin", "name=userns"}
	info, err = json.Marshal(raw)
	require.NoError(t, err)

	fakeDockerCapacityProbe(t, info, func(args []string) ([]byte, error) {
		switch {
		case args[0] == "run":
			return []byte("cid\n"), nil
		case args[0] == "inspect":
			return []byte(cleanInspect(dir)), nil
		case args[0] == "exec" && strings.HasSuffix(strings.Join(args, " "), "/proc/self/mountinfo"):
			return []byte(cleanMountinfoFor(dir)), nil // the boundary check passes
		case args[0] == "exec": // no `stat` in the image — the owner read fails
			return nil, fmt.Errorf("exit status 127")
		}
		return nil, fmt.Errorf("unexpected docker call: %v", args)
	})
	oldPriv, oldUnpriv := statfsFull, statfsFullUnprivileged
	statfsFull = func(string) (bool, error) { return false, nil }
	statfsFullUnprivileged = func(string) (bool, error) { return true, nil }
	defer func() { statfsFull, statfsFullUnprivileged = oldPriv, oldUnpriv }()

	p := &dockerProvisioner{
		image: "img:latest",
		spec: ProvisionSpec{Title: "scoped",
			Account: sessionenv.Account{Agent: "codex", Name: "work", Dir: dir}},
	}
	_, err = p.provision()
	require.Error(t, err)
	assert.False(t, errors.Is(err, syscall.ENOSPC),
		"a read-only exec failure cannot borrow the reserved-block window: %v", err)
}

// TestDockerOpWriteRoots_IncludesNonAccountHome pins that a root exec's
// write roots are scoped to the session that actually performs it: a
// non-account root exec writes the session home (dockerContainerHome,
// /root) plus /workspace, and an account root exec writes the account
// mounts (dockerAccountHome, dockerAccountRuntimeHome) plus /workspace.
// prepareNonAccountUser never writes /af-home or /af-account, so an
// unrelated bind or volume at either account path cannot make a
// non-account root exec borrow the Bavail window (#5424 review);
// symmetrically, prepareAccountUser never writes /root, so an unrelated
// /root bind cannot make an account root exec borrow the window.
// /workspace is a root-exec write root on both paths because each
// prepare step mkdir+chowns it as root.
func TestDockerOpWriteRoots_IncludesNonAccountHome(t *testing.T) {
	nonAccount := &dockerProvisioner{spec: ProvisionSpec{Title: "non-account"}}
	roots := nonAccount.dockerOpWriteRoots(dockerOpRootExec)
	assert.Contains(t, roots, dockerContainerHome,
		"a non-account root exec writes the session home (/root); it must be a root-exec write root so a /root bind can borrow the Bavail window")
	assert.Contains(t, roots, dockerWorkspaceDir,
		"a non-account root exec also mkdir+chowns /workspace; it must be a root-exec write root")
	assert.NotContains(t, roots, dockerAccountHome,
		"a non-account root exec never writes /af-account; an unrelated bind there must not borrow the Bavail window")
	assert.NotContains(t, roots, dockerAccountRuntimeHome,
		"a non-account root exec never writes /af-home; an unrelated bind there must not borrow the Bavail window")

	account := &dockerProvisioner{spec: ProvisionSpec{Title: "account",
		Account: sessionenv.Account{Agent: "codex", Name: "work", Dir: "/af-account"}}}
	accountRoots := account.dockerOpWriteRoots(dockerOpRootExec)
	assert.NotContains(t, accountRoots, dockerContainerHome,
		"an account root exec never writes /root; an unrelated /root bind must not borrow the Bavail window")
	assert.Contains(t, accountRoots, dockerWorkspaceDir,
		"an account root exec mkdir+chowns /workspace; it must be a root-exec write root")
	assert.Contains(t, accountRoots, dockerAccountHome,
		"an account root exec writes the mounted account home; it must be a root-exec write root")
	assert.Contains(t, accountRoots, dockerAccountRuntimeHome,
		"an account root exec writes the account runtime home; it must be a root-exec write root")
	assert.Contains(t, account.dockerOpWriteRoots(dockerOpSessionExec), dockerWorkspaceDir,
		"session execs still write the workspace")
}

// TestDockerOpWriteRoots_ChmodIsAfBinaryPath pins that copyAfBinary's root
// chmod is its own operation class whose single write root is the copied `af`
// binary path (/usr/local/bin/af) — not the home/workspace/account paths the
// prepare steps' root execs write. A chmod failure's capacity verdict may
// only be borrowed by a bind covering the binary, never by an unrelated bind
// at the home or workspace the chmod never touched; symmetrically, a bind at
// /usr/local/bin that a chmod failure DID write through is now recognized
// (#5424 review).
func TestDockerOpWriteRoots_ChmodIsAfBinaryPath(t *testing.T) {
	for _, p := range []*dockerProvisioner{
		{spec: ProvisionSpec{Title: "non-account"}},
		{spec: ProvisionSpec{Title: "account", Account: sessionenv.Account{Agent: "codex", Name: "work", Dir: "/af-account"}}},
	} {
		roots := p.dockerOpWriteRoots(dockerOpChmod)
		assert.Equal(t, []string{dockerAfBinaryPath}, roots,
			"the chmod writes a single path — the copied af binary — regardless of session kind; call=%v", roots)
		assert.NotContains(t, roots, dockerContainerHome,
			"the chmod never writes /root; an unrelated /root bind must not borrow the Bavail window for a chmod failure")
		assert.NotContains(t, roots, dockerWorkspaceDir,
			"the chmod never writes /workspace; an unrelated /workspace bind must not borrow the Bavail window for a chmod failure")
		assert.NotContains(t, roots, dockerAccountHome,
			"the chmod never writes /af-account; an unrelated bind there must not borrow the Bavail window for a chmod failure")
		assert.NotContains(t, roots, dockerAccountRuntimeHome,
			"the chmod never writes /af-home; an unrelated bind there must not borrow the Bavail window for a chmod failure")
	}
}
