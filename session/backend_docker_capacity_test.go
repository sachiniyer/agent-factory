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
		// A banner read whose every `docker exec` fails — the daemon-side
		// exec state is itself a data-root write, so the fs verdict applies.
		"readBanner": func(args []string) bool {
			return args[0] == "exec" && exactArg(args, "cat") && containsArg(args, dockerBannerPath)
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
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")

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

	t.Run("account-owned session exec joins the Bavail verdict", func(t *testing.T) {
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failExec)
		reservedWindow(t)
		p := &dockerProvisioner{
			image:         "img:latest",
			containerUser: "1000:1000", // a mounted account owned by uid 1000
			spec:          ProvisionSpec{Title: "acct", CloneURL: "file:///repo"},
		}
		_, err := p.provision()
		require.Error(t, err)
		assert.True(t, errors.Is(err, syscall.ENOSPC),
			"an exec writing as uid 1000 IS Bavail-bounded — its ENOSPC must surface: %v", err)
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

	t.Run("image USER non-root joins the Bavail verdict", func(t *testing.T) {
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false),
			func(args []string) ([]byte, error) {
				if args[0] == "image" {
					return []byte("1000\n"), nil // the image declares USER 1000
				}
				return failExec(args)
			})
		reservedWindow(t)
		p := &dockerProvisioner{
			image: "img:latest",
			spec:  ProvisionSpec{Title: "img-user", CloneURL: "file:///repo"},
		}
		_, err := p.provision()
		require.Error(t, err)
		assert.True(t, errors.Is(err, syscall.ENOSPC),
			"an exec under a non-root image USER is Bavail-bounded: %v", err)
	})

	t.Run("unreadable image USER fails closed to Bfree", func(t *testing.T) {
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false),
			func(args []string) ([]byte, error) {
				if args[0] == "image" {
					return nil, fmt.Errorf("Error: No such image")
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
			"an unproven image USER is not evidence of unprivileged writes: %v", err)
	})

	t.Run("userns remap makes even a uid-0 exec Bavail-bounded", func(t *testing.T) {
		// `docker info` reports name=userns when the engine remaps container
		// uids into a subordinate range — every in-container writer, uid 0
		// included, is an ordinary user on the data-root filesystem.
		info := dockerInfoAnswer(t, "/var/lib/docker", host, false)
		var raw map[string]any
		require.NoError(t, json.Unmarshal(info, &raw))
		raw["SecurityOptions"] = []string{"name=seccomp,profile=builtin", "name=userns"}
		info, err = json.Marshal(raw)
		require.NoError(t, err)
		fakeDockerCapacityProbe(t, info, failExec)
		reservedWindow(t)
		p := &dockerProvisioner{
			image:         "img:latest",
			containerUser: "0:0",
			spec:          ProvisionSpec{Title: "userns", CloneURL: "file:///repo"},
		}
		_, err = p.provision()
		require.Error(t, err)
		assert.True(t, errors.Is(err, syscall.ENOSPC),
			"under userns remap a uid-0 exec is an unprivileged writer: %v", err)
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
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")

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
// evidence path consults, including the classes no provision-level test can
// reach cheaply (a uid-0 exec under an account session).
func TestDockerStepWriterUnprivileged(t *testing.T) {
	p := &dockerProvisioner{}
	info := dockerEngineInfo{}

	assert.False(t, p.dockerStepWriterUnprivileged(info, dockerOpLocal))
	assert.False(t, p.dockerStepWriterUnprivileged(info, dockerOpDaemonWrite),
		"the daemon writes as its own uid — root on a rootful engine")
	assert.False(t, p.dockerStepWriterUnprivileged(info, dockerOpRootExec),
		"uid 0 reaches reserved blocks on a rootful engine")
	assert.False(t, p.dockerStepWriterUnprivileged(dockerEngineInfo{userns: true}, dockerOpDaemonWrite),
		"userns remaps CONTAINER uids — the rootful daemon's own writes stay privileged")
	assert.True(t, p.dockerStepWriterUnprivileged(dockerEngineInfo{userns: true}, dockerOpRootExec),
		"under userns, in-container uid 0 maps to a subordinate uid")

	p.containerUser = "1000:1000"
	assert.True(t, p.dockerStepWriterUnprivileged(info, dockerOpSessionExec))
	p.containerUser = "0:0"
	assert.False(t, p.dockerStepWriterUnprivileged(info, dockerOpSessionExec))
	assert.True(t, p.dockerStepWriterUnprivileged(dockerEngineInfo{userns: true}, dockerOpSessionExec),
		"userns remap subordinates every container uid, including the account owner's")
}
