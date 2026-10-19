package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")

	// Each failingStep names a docker argv shape emitted by one provision
	// step; matching it makes the fake fail that call. The matchers read argv
	// the way the steps emit it: the detached exec carries a literal "-d"
	// flag, the banner poll carries a literal "cat" plus the banner path —
	// the path alone also appears inside startAgentServer's redirect script.
	failingSteps := map[string]func(args []string) bool{
		"docker run":         func(args []string) bool { return args[0] == "run" },
		"configureGit":       func(args []string) bool { return args[0] == "exec" && containsArg(args, "user.email") },
		"cloneWorkspace":     func(args []string) bool { return args[0] == "exec" && containsArg(args, "git clone") },
		"copyAfBinary cp":    func(args []string) bool { return args[0] == "cp" },
		"copyAfBinary chmod": func(args []string) bool { return args[0] == "exec" && containsArg(args, "chmod") },
		"startAgentServer":   func(args []string) bool { return args[0] == "exec" && exactArg(args, "-d") },
		"readBanner": func(args []string) bool {
			return args[0] == "exec" && exactArg(args, "cat") && containsArg(args, dockerBannerPath)
		},
		"publishedPort": func(args []string) bool { return args[0] == "port" },
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
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")

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

// TestProvisionFailure_DataRootNamespaceGate is the #5382 item-6 regression:
// the statfs verdict is only meaningful when the engine's reported data root
// is the SAME filesystem object af would probe. A remote or SSH-forwarded
// engine (a tcp:// endpoint — even on loopback, where `ssh -L` parks one)
// reports a daemon-side path in another host's namespace; a local Unix socket
// does not prove a shared mount namespace either — Docker Desktop's VM or an
// af container holding the host socket sees the daemon's /var/lib/docker as a
// different filesystem. Only an endpoint proven on this host AND a daemon
// hostname matching af's own is co-location evidence; anything less must
// yield no verdict — unknown is not-proven, never "full".
func TestProvisionFailure_DataRootNamespaceGate(t *testing.T) {
	host, err := os.Hostname()
	require.NoError(t, err)

	failRun := func(args []string) ([]byte, error) {
		if args[0] == "run" {
			return []byte("Error response from daemon: no space left on device\n"), fmt.Errorf("exit status 125")
		}
		return nil, fmt.Errorf("unexpected docker call: %v", args)
	}

	t.Run("tcp loopback endpoint is not namespace proof", func(t *testing.T) {
		// `ssh -L 2375:host:2375` puts a REMOTE daemon's port on this host's
		// loopback — the reported root is on the remote filesystem, and a
		// local statfs of the same path is a verdict about the wrong
		// filesystem.
		t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:2375")
		t.Setenv("DOCKER_CONTEXT", "")
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)

		statfsCalls := 0
		old := statfsFull
		statfsFull = func(string) (bool, error) { statfsCalls++; return true, nil }
		defer func() { statfsFull = old }()

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "tcp-loopback"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"a tcp endpoint cannot prove the data root shares af's mount namespace — no ENOSPC may be invented: %v", err)
		assert.Equal(t, 0, statfsCalls, "the data root must not be probed through a tcp endpoint")
	})

	t.Run("daemon hostname mismatch is not namespace proof", func(t *testing.T) {
		// A Unix socket only proves the daemon answers on this machine —
		// Docker Desktop's VM and an af container bind-mounted the host
		// socket both answer locally while their DockerRootDir lives in the
		// VM's/container's OTHER mount namespace. The daemon's own hostname
		// disagreeing with af's is the observable seam of that split.
		t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
		t.Setenv("DOCKER_CONTEXT", "")
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", "af-test-foreign-daemon", false), failRun)

		statfsCalls := 0
		old := statfsFull
		statfsFull = func(string) (bool, error) { statfsCalls++; return true, nil }
		defer func() { statfsFull = old }()

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "foreign-ns"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"a daemon in another mount namespace must not get a full verdict from af's filesystem: %v", err)
		assert.Equal(t, 0, statfsCalls, "a mismatched daemon hostname must not reach statfs")
	})

	t.Run("local endpoint plus matching hostname probes", func(t *testing.T) {
		t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
		t.Setenv("DOCKER_CONTEXT", "")
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)

		old := statfsFull
		statfsFull = func(string) (bool, error) { return true, nil }
		defer func() { statfsFull = old }()

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "co-located"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.True(t, errors.Is(err, syscall.ENOSPC),
			"a co-located engine whose data root is full must still classify environmental: %v", err)
	})

	t.Run("a root af cannot stat is not full", func(t *testing.T) {
		t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
		t.Setenv("DOCKER_CONTEXT", "")
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)

		old := statfsFull
		statfsFull = func(string) (bool, error) {
			return false, &os.PathError{Op: "statfs", Path: "/var/lib/docker", Err: syscall.ENOENT}
		}
		defer func() { statfsFull = old }()

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "unstatable"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"an unstatable data root cannot produce a full verdict: %v", err)
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
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")
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
