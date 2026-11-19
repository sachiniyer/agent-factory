package session

// Tests for #7577 item 2: af's payload (the af binary, the operator policy)
// reaches the container through af-owned read-only binds appended LAST in
// `docker run`, with a cp+chmod fallback that runs only behind
// verifyWriteTargets' mount-table proof.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stagingTestEnv prepares the environment every staging test needs: an AF home
// the staging dir lives under, an on-host engine endpoint (so bindMountRelabel
// consults this host's SELinux state rather than always relabeling), and a
// resolvable docker binary.
func stagingTestEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
	t.Setenv("DOCKER_CONTEXT", "")
	t.Cleanup(SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil }))
	old := dockerStagingInContainer
	dockerStagingInContainer = func() bool { return false }
	t.Cleanup(func() { dockerStagingInContainer = old })
	return home
}

// stagingBinarySource writes a real file for the provisioner's afBin — staging
// reads it, so it must exist (the older fixtures point at absent paths on
// purpose to force the cp fallback).
func stagingBinarySource(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(p, []byte("fake-af-binary"), 0o755))
	return p
}

// writeGuardTableAnswer renders verifyWriteTargets' NUL-framed answer: the
// resolved parent of each write destination (pwd -P's trailing newline kept,
// then the shell's '\0'), then a mountinfo whose "/" is overlay and whose
// extra mounts are (mountPoint, fstype) pairs.
func writeGuardTableAnswer(realA, realB string, mounts ...[2]string) []byte {
	var b strings.Builder
	b.WriteString(mountinfoLine(1, "/", "overlay") + "\n")
	for i, m := range mounts {
		b.WriteString(mountinfoLine(10+i, m[0], m[1]) + "\n")
	}
	return writeGuardAnswerRaw(realA, realB, b.String())
}

// stagingDockerAnswers installs a fake docker CLI that records every call into
// calls. `info` answers the {{json .}} snapshot with engineName (and a
// rootless SecurityOptions marker when rootless) and any other format with a
// bare engine id; `run`, `cp`, `rm` and `port` answer ordinarily. `exec`
// answers the image default-USER probe as root (so prepareNonAccountUser is a
// no-op), the mount-table read through mountAnswer (default: a plain
// rootfs-only table), and the banner poll with a live server. runHook, when
// set, replaces the ordinary `run` answer.
func stagingDockerAnswers(t *testing.T, calls *[][]string, engineName string, rootless bool, mountAnswer func(joined string) ([]byte, error), runHook func(joined string, runCount int) ([]byte, error)) func() {
	t.Helper()
	runCount := 0
	return SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string(nil), args...))
		joined := strings.Join(args, " ")
		switch args[0] {
		case "info":
			format := ""
			for i, a := range args {
				if a == "--format" && i+1 < len(args) {
					format = args[i+1]
				}
			}
			if strings.Contains(format, "json") {
				sec := []string{"name=seccomp,profile=builtin"}
				if rootless {
					sec = append(sec, "name=rootless")
				}
				blob, err := json.Marshal(map[string]any{
					"Name":            engineName,
					"DockerRootDir":   "/var/lib/docker",
					"SecurityOptions": sec,
				})
				require.NoError(t, err)
				return blob, nil
			}
			return []byte("staging-test-engine\n"), nil
		case "run":
			if containsArg(args, "--rm") {
				// The in-image resolution probe: resolved af parents, the
				// marker field, then one field per repo mount destination.
				if dockerStagingProbeAnswer != nil {
					return dockerStagingProbeAnswer(joined)
				}
				var b strings.Builder
				b.WriteString("/usr/local/bin\n\x00/etc\n\x00\x00")
				for i, a := range args {
					if a == "-c" && i+2 < len(args) {
						for _, d := range args[i+7:] {
							b.WriteString(d + "\n\x00")
						}
						break
					}
				}
				return []byte(b.String()), nil
			}
			runCount++
			if runHook != nil {
				return runHook(joined, runCount)
			}
			return []byte(dockerCreatedID + "\n"), nil
		case "cp", "rm":
			return nil, nil
		case "port":
			return []byte("127.0.0.1:49152\n"), nil
		case "exec":
			switch {
			case strings.Contains(joined, "id -u"):
				return []byte("0:0\n"), nil
			case strings.Contains(joined, "wc -c"):
				// The post-create delivery check: staged files delivered.
				return nil, nil
			case strings.Contains(joined, "/proc/self/mountinfo"):
				if mountAnswer != nil {
					return mountAnswer(joined)
				}
				return probeAnswer(args), nil
			case strings.Contains(joined, "cat "+dockerBannerPath):
				return []byte(`{"addr":":8000","token":"t","title":"staging"}` + "\n" + remoteProcAlive + "\n"), nil
			}
			return nil, nil
		}
		return nil, nil
	})
}

// dockerStagingProbeAnswer overrides the probe `docker run --rm` answer when
// non-nil — tests use it to feed symlinked parents or a failing probe.
var dockerStagingProbeAnswer func(joined string) ([]byte, error)

// findDockerCall returns the recorded docker call whose argv starts with
// subcommand and whose joined argv contains every needle, or nil.
func findDockerCall(calls [][]string, subcommand string, needles ...string) []string {
	for _, call := range calls {
		if len(call) == 0 || call[0] != subcommand {
			continue
		}
		joined := strings.Join(call, " ")
		ok := true
		for _, n := range needles {
			if !strings.Contains(joined, n) {
				ok = false
				break
			}
		}
		if ok {
			return call
		}
	}
	return nil
}

// stagingBindVolumes extracts the -v mount specs a `docker run` call carries.
func stagingBindVolumes(runCall []string) []string {
	var mounts []string
	for i, a := range runCall {
		if a == "-v" && i+1 < len(runCall) {
			mounts = append(mounts, runCall[i+1])
		}
	}
	return mounts
}

// TestDockerStagingBindsDeliverThePayload is the item-2 property: with a
// stageable payload and a shareable engine, `docker run` mounts the af-owned
// staged files read-only at the destinations af used to `cp`+`chmod` — the
// binds land LAST in argv, after docker.run_args, and no cp or chmod runs at
// all.
func TestDockerStagingBindsDeliverThePayload(t *testing.T) {
	home := stagingTestEnv(t)
	var calls [][]string
	t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))

	p := &dockerProvisioner{
		image:   "img:latest",
		afBin:   stagingBinarySource(t),
		runArgs: []string{"-v", "/repo/chosen:/repo-chosen"},
		spec: ProvisionSpec{
			Title:       "staged",
			CloneURL:    "file:///repo",
			AgentPolicy: []byte(`{"hooks":{}}`),
		},
	}
	_, err := p.provision()
	require.NoError(t, err, "calls=%v", calls)

	run := findDockerCall(calls, "run", "-d")
	require.NotNil(t, run, "no docker run call; calls=%v", calls)
	entryIdx := -1
	for i, a := range run {
		if a == "--entrypoint" {
			entryIdx = i
		}
	}
	require.Positive(t, entryIdx, "docker run must carry --entrypoint; argv=%v", run)

	mounts := stagingBindVolumes(run)
	var afMount, policyMount string
	for _, m := range mounts {
		switch {
		case strings.Contains(m, ":"+dockerAfBinaryPath+":"):
			afMount = m
		case strings.Contains(m, ":"+dockerPolicyPath+":"):
			policyMount = m
		}
	}
	require.NotEmpty(t, afMount, "no bind at %s in argv %v", dockerAfBinaryPath, run)
	require.NotEmpty(t, policyMount, "no bind at %s in argv %v", dockerPolicyPath, run)

	// The source is af-owned: inside the per-session staging dir under the AF
	// home, carrying the modes the container must see so no chmod follows.
	stagingParent := filepath.Join(home, dockerStagingDirName)
	require.True(t, strings.HasPrefix(afMount, stagingParent+string(os.PathSeparator)),
		"the af bind source must live in the af-owned staging dir: %q", afMount)
	require.True(t, strings.HasPrefix(policyMount, stagingParent+string(os.PathSeparator)),
		"the policy bind source must live in the af-owned staging dir: %q", policyMount)

	// Read-only either way: plain "ro", or "ro,z" where bindMountRelabel
	// asks SELinux to share the source label.
	for _, m := range []string{afMount, policyMount} {
		require.Truef(t, strings.HasSuffix(m, ":ro") || strings.HasSuffix(m, ":ro,z"),
			"the staged bind must be read-only: %q", m)
	}

	// Ordering: the staged binds are the LAST mount arguments — after every
	// run_args entry — so nothing repo-controlled follows them.
	repoIdx := -1
	for i, a := range run {
		if a == "/repo/chosen:/repo-chosen" {
			repoIdx = i
		}
	}
	require.Positive(t, repoIdx, "the run_args mount must precede the staged binds; argv=%v", run)
	for i, a := range run {
		if strings.HasPrefix(a, stagingParent) {
			require.Greater(t, i, repoIdx, "staged bind %q must come after run_args", a)
		}
	}

	// The cp fallback never ran: no `docker cp`, and no chmod exec — the
	// staged modes are what the container serves.
	require.Nil(t, findDockerCall(calls, "cp"), "the bind path must not cp; calls=%v", calls)
	require.Nil(t, findDockerCall(calls, "exec", "chmod"), "the bind path must not chmod; calls=%v", calls)

	// Modes are staged exactly: 0755 for the binary, 0644 for the policy.
	afStaged := strings.SplitN(afMount, ":", 2)[0]
	st, err := os.Stat(afStaged)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), st.Mode().Perm(), "the staged af binary must be exec'able")
	policyStaged := strings.SplitN(policyMount, ":", 2)[0]
	st, err = os.Stat(policyStaged)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), st.Mode().Perm(), "the staged policy must be world-readable")
}

// TestDockerStagingRootlessStillBinds proves the rootless case: a rootless
// dockerd runs as the daemon's own user, so the af-home staging dir is
// readable and the bind path still applies.
func TestDockerStagingRootlessStillBinds(t *testing.T) {
	stagingTestEnv(t)
	var calls [][]string
	t.Cleanup(stagingDockerAnswers(t, &calls, "moby", true, nil, nil))

	p := &dockerProvisioner{
		image: "img:latest",
		afBin: stagingBinarySource(t),
		spec:  ProvisionSpec{Title: "rootless", CloneURL: "file:///repo"},
	}
	_, err := p.provision()
	require.NoError(t, err, "calls=%v", calls)

	run := findDockerCall(calls, "run", "-d")
	require.NotNil(t, run)
	require.NotEmpty(t, findDockerCall(calls, "run", ":"+dockerAfBinaryPath+":"),
		"a rootless engine reads the daemon-owned staging dir — the bind path applies; argv=%v", run)
	require.Nil(t, findDockerCall(calls, "cp"), "calls=%v", calls)
}

// TestDockerStagingDuplicateDestinationIsActionable: a docker-level duplicate
// — two mounts claiming the same in-container path — is rewritten into an
// actionable error rather than surfaced as docker's raw text. (An af
// destination's duplicate never reaches docker anymore: the enclosing-mount
// check refuses it pre-flight — covered above.)
func TestDockerStagingDuplicateDestinationIsActionable(t *testing.T) {
	stagingTestEnv(t)
	var calls [][]string
	t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil,
		func(string, int) ([]byte, error) {
			return []byte("docker: Error response from daemon: Duplicate mount point: /opt/dup\n"),
				fmt.Errorf("exit status 125")
		}))

	p := &dockerProvisioner{
		image:   "img:latest",
		afBin:   stagingBinarySource(t),
		runArgs: []string{"-v", "/repo/a:/opt/dup", "-v", "/repo/b:/opt/dup"},
		spec:    ProvisionSpec{Title: "dup", CloneURL: "file:///repo"},
	}
	_, err := p.provision()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate mount destination")
	assert.Contains(t, err.Error(), "run_args")
	require.Nil(t, findDockerCall(calls, "cp"), "a duplicate refusal must not fall through to cp; calls=%v", calls)
}

// TestDockerStagingDesktopUnshareableFallsBackToCp is the Docker Desktop
// boundary: on a docker-desktop engine whose file-sharing roots exclude the
// staging dir, no bind is installed and the payload goes through `docker cp`
// — sourced from the staged files, so the preserved modes still need no chmod
// — gated by the write-target mount-table check.
func TestDockerStagingDesktopUnshareableFallsBackToCp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shared-roots boundary is POSIX")
	}
	home := stagingTestEnv(t)
	oldGOOS, oldRoots := dockerStagingGOOS, dockerStagingSharedRoots
	dockerStagingGOOS = "darwin"
	dockerStagingSharedRoots = []string{"/Users"}
	defer func() { dockerStagingGOOS, dockerStagingSharedRoots = oldGOOS, oldRoots }()

	var calls [][]string
	t.Cleanup(stagingDockerAnswers(t, &calls, "docker-desktop", false, nil, nil))

	p := &dockerProvisioner{
		image: "img:latest",
		afBin: stagingBinarySource(t),
		spec:  ProvisionSpec{Title: "desktop", CloneURL: "file:///repo"},
	}
	_, err := p.provision()
	require.NoError(t, err, "calls=%v", calls)

	run := findDockerCall(calls, "run", "-d")
	require.NotNil(t, run)
	require.Nil(t, findDockerCall(calls, "run", ":"+dockerAfBinaryPath+":"),
		"an unshareable staging dir must not be bound; argv=%v", run)

	// The cp ran from the STAGED copy — under the AF home's staging dir — so
	// the 0755 mode is preserved and no chmod follows.
	cp := findDockerCall(calls, "cp")
	require.NotNil(t, cp, "the fallback must still docker cp; calls=%v", calls)
	require.True(t, strings.HasPrefix(cp[1], filepath.Join(home, dockerStagingDirName)),
		"cp must source the staged file, not afBin directly: %v", cp)
	require.Nil(t, findDockerCall(calls, "exec", "chmod"),
		"a staged-source cp preserves mode — no chmod; calls=%v", calls)

	// The mount-table guard ran before the cp.
	require.NotNil(t, findDockerCall(calls, "exec", "/proc/self/mountinfo"),
		"the write-target guard must read the mount table first; calls=%v", calls)
}

// TestDockerStagingSharingFailureRetriesOnceWithCp: when `docker run` still
// refuses the bind source — a Desktop sharing refusal the upfront check could
// not see — the provisioner retries once without the binds and delivers via
// cp instead of failing the session.
func TestDockerStagingSharingFailureRetriesOnceWithCp(t *testing.T) {
	stagingTestEnv(t)
	var calls [][]string
	t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil,
		func(_ string, runCount int) ([]byte, error) {
			if runCount == 1 {
				// Docker created the container, then refused the bind at
				// start — an id line and an error, the create-then-fail
				// shape runContainer already handles (#2008).
				return []byte("docker: Error response from daemon: Mounts denied:\r\nThe path /x is not shared from the host\n" + dockerCreatedID + "\n"),
					fmt.Errorf("exit status 125")
			}
			return []byte(dockerCreatedID + "\n"), nil
		}))

	p := &dockerProvisioner{
		image: "img:latest",
		afBin: stagingBinarySource(t),
		spec:  ProvisionSpec{Title: "share-retry", CloneURL: "file:///repo"},
	}
	_, err := p.provision()
	require.NoError(t, err, "calls=%v", calls)

	var runs [][]string
	for _, c := range calls {
		if len(c) > 0 && c[0] == "run" {
			runs = append(runs, c)
		}
	}
	require.Len(t, runs, 2, "exactly one retry is expected; calls=%v", calls)
	require.Empty(t, stagingBindVolumes(runs[1]),
		"the retry must not carry the binds the engine refused; argv=%v", runs[1])
	require.NotNil(t, findDockerCall(calls, "rm"),
		"the refused container must be removed before the retry; calls=%v", calls)
	require.NotNil(t, findDockerCall(calls, "cp"),
		"the retried provision delivers via cp; calls=%v", calls)
}

// TestDockerWriteGuardRefusesHostMounts is option A's fallback check: on the
// cp path, a write target enclosed by — or covering — ANY mount other than the
// container rootfs is refused with an actionable error, and no cp runs. tmpfs
// rows are refused on purpose: a host tmpfs bind (-v /dev/shm:/dst) reports the
// same fstype as a private --tmpfs, so the check cannot tell them apart — and
// a write through the host bind escapes the container.
func TestDockerWriteGuardRefusesHostMounts(t *testing.T) {
	tests := []struct {
		name        string
		realA       string
		realB       string
		mounts      [][2]string
		wantRefused string
		policy      bool
	}{
		{
			name:        "bind over the af directory",
			realA:       "/usr/local/bin",
			realB:       "/usr/local/bin",
			mounts:      [][2]string{{"/usr/local/bin", "ext4"}},
			wantRefused: dockerAfBinaryPath,
		},
		{
			name:        "bind at the af file itself",
			realA:       "/usr/local/bin",
			realB:       "/usr/local/bin",
			mounts:      [][2]string{{"/usr/local/bin/af", "ext4"}},
			wantRefused: dockerAfBinaryPath,
		},
		{
			name:        "bind over the policy directory",
			realA:       "/usr/local/bin",
			realB:       "/etc",
			mounts:      [][2]string{{"/etc", "ext4"}},
			wantRefused: dockerPolicyPath,
			policy:      true,
		},
		{
			name:        "symlinked parent resolving onto a bind",
			realA:       "/mnt/host-bin",
			realB:       "/usr/local/bin",
			mounts:      [][2]string{{"/mnt/host-bin", "ext4"}},
			wantRefused: dockerAfBinaryPath,
		},
		{
			name:        "mount beneath a directory destination",
			realA:       "/usr/local/bin",
			realB:       "/usr/local/bin",
			mounts:      [][2]string{{"/usr/local/bin/af/inner", "nfs"}},
			wantRefused: dockerAfBinaryPath,
		},
		{
			name:        "host tmpfs bind over the af directory",
			realA:       "/usr/local/bin",
			realB:       "/usr/local/bin",
			mounts:      [][2]string{{"/usr/local/bin", "tmpfs"}},
			wantRefused: dockerAfBinaryPath,
		},
		{
			name:        "tmpfs bind at the af file itself",
			realA:       "/usr/local/bin",
			realB:       "/usr/local/bin",
			mounts:      [][2]string{{"/usr/local/bin/af", "tmpfs"}},
			wantRefused: dockerAfBinaryPath,
		},
		{
			name:        "tmpfs mount over the policy directory",
			realA:       "/usr/local/bin",
			realB:       "/etc",
			mounts:      [][2]string{{"/etc", "tmpfs"}},
			wantRefused: dockerPolicyPath,
			policy:      true,
		},
		{
			name:        "tmpfs mount beneath a directory destination",
			realA:       "/usr/local/bin",
			realB:       "/usr/local/bin",
			mounts:      [][2]string{{"/usr/local/bin/af/inner", "tmpfs"}},
			wantRefused: dockerAfBinaryPath,
		},
		{
			name:  "resolved parent with a trailing space is judged byte-exactly",
			realA: "/usr/local/bin ",
			realB: "/usr/local/bin",
			// A mount over "/usr/local/bin " covers "/usr/local/bin /af" — a
			// TrimSpace'd read would judge "/usr/local/bin/af" and miss it.
			mounts:      [][2]string{{"/usr/local/bin ", "tmpfs"}},
			wantRefused: dockerAfBinaryPath,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stagingTestEnv(t)
			var calls [][]string
			answer := writeGuardTableAnswer(tt.realA, tt.realB, tt.mounts...)
			t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false,
				func(string) ([]byte, error) { return answer, nil }, nil))

			spec := ProvisionSpec{Title: "guarded", CloneURL: "file:///repo"}
			if tt.policy {
				spec.AgentPolicy = []byte(`{"hooks":{}}`)
			}
			p := &dockerProvisioner{
				image: "img:latest",
				afBin: filepath.Join(t.TempDir(), "missing-af"), // staging fails → the cp fallback runs
				spec:  spec,
			}
			_, err := p.provision()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "refusing to write "+tt.wantRefused,
				"the refusal must name the destination: %v", err)
			assert.Contains(t, err.Error(), "run_args",
				"the refusal must tell the operator where the mount came from: %v", err)
			require.Nil(t, findDockerCall(calls, "cp"),
				"a refused target must not be written; calls=%v", calls)
		})
	}
}

// TestDockerWriteGuardAllowsRootfs pins the allowed case: every write target
// resolves under "/" itself. The second row is the --tmpfs /tmp preservation
// the policy path exists for: a private tmpfs over /tmp is a common hardening
// flag, and with the policy at /etc nothing af writes lands beneath it, so the
// fallback still delivers.
func TestDockerWriteGuardAllowsRootfs(t *testing.T) {
	for _, tt := range []struct {
		name   string
		realA  string
		mounts [][2]string
		policy bool
	}{
		{name: "plain rootfs"},
		{name: "a private tmpfs over /tmp elsewhere is irrelevant", mounts: [][2]string{{"/tmp", "tmpfs"}}, policy: true},
		{
			name:  "a trailing-spaced sibling is not under the unspaced mount",
			realA: "/usr/local/bin ",
			// "/usr/local/bin/af" is under this mount; "/usr/local/bin /af"
			// is not — judging the resolved path byte-exactly is what lets
			// the write proceed.
			mounts: [][2]string{{"/usr/local/bin", "tmpfs"}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stagingTestEnv(t)
			var calls [][]string
			realA, realB := tt.realA, "/usr/local/bin"
			if realA == "" {
				realA = "/usr/local/bin"
			}
			if tt.policy {
				realB = "/etc"
			}
			answer := writeGuardTableAnswer(realA, realB, tt.mounts...)
			t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false,
				func(string) ([]byte, error) { return answer, nil }, nil))

			spec := ProvisionSpec{Title: "allowed", CloneURL: "file:///repo"}
			if tt.policy {
				spec.AgentPolicy = []byte(`{"hooks":{}}`)
			}
			p := &dockerProvisioner{
				image: "img:latest",
				afBin: filepath.Join(t.TempDir(), "missing-af"),
				spec:  spec,
			}
			_, err := p.provision()
			require.NoError(t, err, "calls=%v", calls)
			require.NotNil(t, findDockerCall(calls, "cp"),
				"an allowed target must reach the cp; calls=%v", calls)
		})
	}
}

// TestDockerWriteGuardRefusesSymlinkLeaf: a destination whose final component
// is a symlink is refused — `docker cp` follows it off the checked path.
func TestDockerWriteGuardRefusesSymlinkLeaf(t *testing.T) {
	stagingTestEnv(t)
	var calls [][]string
	answer := append(writeGuardTableAnswer("/usr/local/bin", "/usr/local/bin"),
		[]byte(leafSymlinkMarker+dockerAfBinaryPath+"\n")...)
	t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false,
		func(string) ([]byte, error) { return answer, nil }, nil))

	p := &dockerProvisioner{
		image: "img:latest",
		afBin: filepath.Join(t.TempDir(), "missing-af"),
		spec:  ProvisionSpec{Title: "symlink", CloneURL: "file:///repo"},
	}
	_, err := p.provision()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "symbolic link")
	assert.Contains(t, err.Error(), dockerAfBinaryPath)
	require.Nil(t, findDockerCall(calls, "cp"), "calls=%v", calls)
}

// TestDockerWriteGuardUnreadableTableRefuses: the check is fail-closed — an
// exec failure or an answer it cannot parse refuses the write rather than
// hoping the destination is safe.
func TestDockerWriteGuardUnreadableTableRefuses(t *testing.T) {
	for _, tt := range []struct {
		name   string
		answer func(string) ([]byte, error)
	}{
		{name: "exec fails", answer: func(string) ([]byte, error) {
			return nil, fmt.Errorf("exit status 1")
		}},
		{name: "unparseable answer", answer: func(string) ([]byte, error) {
			return []byte("not a mount table\n"), nil
		}},
		{name: "double-slash mount point in the table", answer: func(string) ([]byte, error) {
			// "//host" is not a shape a kernel mountinfo emits; the parser
			// must refuse it rather than prefix-match the cleaned "/host".
			return writeGuardTableAnswer("/usr/local/bin", "/usr/local/bin",
				[2]string{"//host", "ext4"}), nil
		}},
		{name: "resolved parent with a leading space refuses as non-absolute", answer: func(string) ([]byte, error) {
			return writeGuardTableAnswer(" /usr/local/bin", "/usr/local/bin"), nil
		}},
		{name: "empty resolved parent", answer: func(string) ([]byte, error) {
			return writeGuardTableAnswer("", "/usr/local/bin"), nil
		}},
		{name: "missing NUL framing", answer: func(string) ([]byte, error) {
			// The old newline-delimited shape has no NUL fields to split on.
			return mountTableAnswer(), nil
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stagingTestEnv(t)
			var calls [][]string
			t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, tt.answer, nil))

			p := &dockerProvisioner{
				image: "img:latest",
				afBin: filepath.Join(t.TempDir(), "missing-af"),
				spec:  ProvisionSpec{Title: "closed", CloneURL: "file:///repo"},
			}
			_, err := p.provision()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "mount table")
			require.Nil(t, findDockerCall(calls, "cp"), "calls=%v", calls)
		})
	}
}

// TestDockerStagingFallbackChmodsTheUnstagedPayload: when staging itself
// fails — no staged files at all — the pre-#7577 path runs whole: cp of the
// daemon binary and the root chmod that makes it exec'able, still behind the
// mount-table guard.
func TestDockerStagingFallbackChmodsTheUnstagedPayload(t *testing.T) {
	stagingTestEnv(t)
	var calls [][]string
	t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))

	p := &dockerProvisioner{
		image: "img:latest",
		afBin: filepath.Join(t.TempDir(), "missing-af"),
		spec:  ProvisionSpec{Title: "unstaged", CloneURL: "file:///repo"},
	}
	_, err := p.provision()
	require.NoError(t, err, "calls=%v", calls)

	cp := findDockerCall(calls, "cp", dockerAfBinaryPath)
	require.NotNil(t, cp, "the fallback must still docker cp; calls=%v", calls)
	require.Equal(t, p.afBin, cp[1], "the unstaged fallback cp's the daemon's own binary")
	require.NotNil(t, findDockerCall(calls, "exec", "chmod +x"),
		"an unstaged cp lands root-owned — the chmod is required; calls=%v", calls)
	require.NotNil(t, findDockerCall(calls, "exec", "/proc/self/mountinfo"),
		"the mount-table guard runs ahead of the fallback cp too; calls=%v", calls)
}

// TestDockerStagingDirRemovedOnReap: the staging dir is af-owned scratch; it
// is removed on teardown whether or not a container ever ran.
func TestDockerStagingDirRemovedOnReap(t *testing.T) {
	home := stagingTestEnv(t)
	var calls [][]string
	t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))

	p := &dockerProvisioner{
		image: "img:latest",
		afBin: stagingBinarySource(t),
		spec:  ProvisionSpec{Title: "reaped", CloneURL: "file:///repo"},
	}
	_, err := p.provision()
	require.NoError(t, err, "calls=%v", calls)

	stagingParent := filepath.Join(home, dockerStagingDirName)
	entries, err := os.ReadDir(stagingParent)
	require.NoError(t, err)
	require.Len(t, entries, 1, "one staging dir per provision")

	require.NoError(t, p.reap())
	_, err = os.Stat(filepath.Join(stagingParent, entries[0].Name()))
	assert.True(t, os.IsNotExist(err), "the staging dir must be removed on reap")
	assert.Empty(t, p.stagingDir, "reap clears the stored path so cleanup is not repeated")
}

// TestDockerStagingRelabelsForSELinux: the staged binds are af-installed
// mounts, so they take the same bindMountRelabel mode as the credential and
// account binds — "ro,z" on an enforcing host, "ro" off it.
func TestDockerStagingRelabelsForSELinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("SELinux is a linux LSM")
	}
	dir := t.TempDir()
	enforce := filepath.Join(dir, "enforce")
	require.NoError(t, os.WriteFile(enforce, []byte("1\n"), 0o644))
	setSELinuxEnforcePaths(t, enforce)

	stagingTestEnv(t)
	var calls [][]string
	t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))

	p := &dockerProvisioner{
		image: "img:latest",
		afBin: stagingBinarySource(t),
		spec:  ProvisionSpec{Title: "selinux", CloneURL: "file:///repo"},
	}
	_, err := p.provision()
	require.NoError(t, err, "calls=%v", calls)

	run := findDockerCall(calls, "run", ":"+dockerAfBinaryPath+":ro,z")
	require.NotNil(t, run,
		"an enforcing host must relabel the staged bind (ro,z); calls=%v", calls)
}

// TestDockerStagingModesIgnoreTheUmask: os.WriteFile applies the daemon's
// umask, so under 0077 an unpinned stage would land af at 0700 and the policy
// at 0600 — a non-root container user could neither exec the binary nor read
// the policy, the exact regression the removed in-container chmod used to
// repair. Staging pins both modes explicitly.
func TestDockerStagingModesIgnoreTheUmask(t *testing.T) {
	original := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(original) })

	home := stagingTestEnv(t)
	var calls [][]string
	t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))

	p := &dockerProvisioner{
		image: "img:latest",
		afBin: stagingBinarySource(t),
		spec: ProvisionSpec{
			Title:       "umasked",
			CloneURL:    "file:///repo",
			AgentPolicy: []byte(`{"hooks":{}}`),
		},
	}
	_, err := p.provision()
	require.NoError(t, err, "calls=%v", calls)

	dirs, err := os.ReadDir(filepath.Join(home, dockerStagingDirName))
	require.NoError(t, err)
	require.Len(t, dirs, 1)
	afStaged := filepath.Join(home, dockerStagingDirName, dirs[0].Name(), "af")
	st, err := os.Stat(afStaged)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), st.Mode().Perm(), "a 0077 umask must not strip the binary's exec bits")
	st, err = os.Stat(filepath.Join(home, dockerStagingDirName, dirs[0].Name(), "af-policy.json"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), st.Mode().Perm(), "a 0077 umask must not strip the policy's read bits")
}

// TestDockerStagingDirReapedWhenRunNeverCreated: a `docker run` refusal that
// creates no container — the duplicate-mount error is exactly this shape —
// still has to clear the staged payload. reapProvisionFailure's old early
// return skipped reap() entirely, leaking one af-binary copy per failed
// provision.
func TestDockerStagingDirReapedWhenRunNeverCreated(t *testing.T) {
	home := stagingTestEnv(t)
	var calls [][]string
	t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil,
		func(string, int) ([]byte, error) {
			return nil, fmt.Errorf("exit status 125: duplicate mount point")
		}))

	p := &dockerProvisioner{
		image: "img:latest",
		afBin: stagingBinarySource(t),
		spec:  ProvisionSpec{Title: "norun", CloneURL: "file:///repo"},
	}
	_, err := p.provision()
	require.Error(t, err, "calls=%v", calls)
	// The production pair: dockerRuntime.Provision wraps a failed provision()
	// in reapProvisionFailure — the inner method alone runs no cleanup.
	require.Error(t, p.reapProvisionFailure(err))

	entries, rerr := os.ReadDir(filepath.Join(home, dockerStagingDirName))
	require.NoError(t, rerr)
	assert.Empty(t, entries, "a run that never created a container must still clear the staging dir")
	assert.Empty(t, p.stagingDir)
}

// TestDockerStagingColonHomeUsesTheCpFallback: an AF home path carrying ':' or
// ',' cannot be encoded — ':' splits a -v spec's fields, ',' splits
// --mount's (dockerAccountMount's rule). The staged files keep their correct
// modes, so the guarded cp path delivers them rather than emit a mount Docker
// would misread.
func TestDockerStagingColonHomeUsesTheCpFallback(t *testing.T) {
	home := filepath.Join(t.TempDir(), "co:l")
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
	t.Setenv("DOCKER_CONTEXT", "")
	t.Cleanup(SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil }))
	old := dockerStagingInContainer
	dockerStagingInContainer = func() bool { return false }
	t.Cleanup(func() { dockerStagingInContainer = old })

	var calls [][]string
	t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))

	p := &dockerProvisioner{
		image: "img:latest",
		afBin: stagingBinarySource(t),
		spec:  ProvisionSpec{Title: "colon", CloneURL: "file:///repo"},
	}
	_, err := p.provision()
	require.NoError(t, err, "calls=%v", calls)

	assert.False(t, p.payloadViaBinds, "a ':' or ',' staging path cannot use binds")
	require.NotNil(t, findDockerCall(calls, "cp"),
		"the unencodable path must fall back to cp; calls=%v", calls)
	require.NotNil(t, findDockerCall(calls, "exec", "/proc/self/mountinfo"),
		"the mount-table guard still gates the cp; calls=%v", calls)
	run := findDockerCall(calls, "run", "-d")
	require.NotNil(t, run, "calls=%v", calls)
	for _, m := range stagingBindVolumes(run) {
		assert.NotContains(t, m, home, "a colon-bearing source must not reach a -v spec: %q", m)
	}
}

// TestDockerStagingContainerizedAfFallsBackToCp: a unix socket mounted into
// af's own container is the HOST's dockerd — the staging dir lives in af's
// container filesystem where dockerd cannot see it, and a missing -v source
// is created as a directory rather than refused. The bind path must not be
// selected; the payload goes through the guarded cp fallback instead.
func TestDockerStagingContainerizedAfFallsBackToCp(t *testing.T) {
	stagingTestEnv(t)
	old := dockerStagingInContainer
	dockerStagingInContainer = func() bool { return true }
	defer func() { dockerStagingInContainer = old }()

	var calls [][]string
	t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))

	p := &dockerProvisioner{
		image: "img:latest",
		afBin: stagingBinarySource(t),
		spec:  ProvisionSpec{Title: "incontainer", CloneURL: "file:///repo"},
	}
	_, err := p.provision()
	require.NoError(t, err, "calls=%v", calls)

	assert.False(t, p.payloadViaBinds, "a containerized daemon's staging dir is invisible to host dockerd")
	run := findDockerCall(calls, "run", "-d")
	require.NotNil(t, run, "calls=%v", calls)
	assert.Empty(t, stagingBindVolumes(run), "no staged binds may reach the run argv: %v", run)
	require.NotNil(t, findDockerCall(calls, "cp"), "calls=%v", calls)
	require.NotNil(t, findDockerCall(calls, "exec", "/proc/self/mountinfo"),
		"the mount-table guard still gates the cp; calls=%v", calls)
}

// TestDockerStagingVolumesFromFallsBackToCp: --volumes-from inherits the
// donor's mounts at the donor's own container paths, which af cannot enumerate
// pre-flight — a donor binding a host dir over /etc would turn af's staged
// policy bind into a root-owned af-policy.json stub in that host dir. Inherited
// mounts select cp delivery, where the mount-table guard reads the donor's
// real mounts inside the container.
func TestDockerStagingVolumesFromFallsBackToCp(t *testing.T) {
	stagingTestEnv(t)
	var calls [][]string
	t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))

	p := &dockerProvisioner{
		image:   "img:latest",
		afBin:   stagingBinarySource(t),
		runArgs: []string{"--volumes-from", "donor"},
		spec:    ProvisionSpec{Title: "donor", CloneURL: "file:///repo", AgentPolicy: []byte(`{"hooks":{}}`)},
	}
	_, err := p.provision()
	require.NoError(t, err, "calls=%v", calls)

	assert.False(t, p.payloadViaBinds, "--volumes-from destinations cannot be enumerated for the enclosing-mount guard")
	run := findDockerCall(calls, "run", "-d")
	require.NotNil(t, run, "calls=%v", calls)
	assert.Empty(t, stagingBindVolumes(run), "no staged binds may reach the run argv: %v", run)
	require.NotNil(t, findDockerCall(calls, "cp"), "calls=%v", calls)
	require.NotNil(t, findDockerCall(calls, "exec", "/proc/self/mountinfo"),
		"the mount-table guard still gates the cp; calls=%v", calls)
}

// TestDockerStagingExecScriptsPinPATH: a plain session's run_args may set
// `-e PATH=/evil:…`, and docker exec inherits the container's configured env —
// every in-container script this PR adds must pin PATH before naming any
// external program, so a repo-controlled directory cannot supply the `cat`,
// `wc` or `tr` a root exec runs.
func TestDockerStagingExecScriptsPinPATH(t *testing.T) {
	const pinned = "export PATH=/usr/sbin:/usr/bin:/sbin:/bin"
	scriptAfterC := func(args []string) string {
		t.Helper()
		for i, a := range args {
			if a == "-c" && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	t.Run("mount-table read", func(t *testing.T) {
		stagingTestEnv(t)
		old := dockerStagingInContainer
		dockerStagingInContainer = func() bool { return true }
		defer func() { dockerStagingInContainer = old }()
		var calls [][]string
		t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))
		p := &dockerProvisioner{
			image:   "img:latest",
			afBin:   stagingBinarySource(t),
			runArgs: []string{"-e", "PATH=/evil/nowhere"},
			spec:    ProvisionSpec{Title: "path", CloneURL: "file:///repo"},
		}
		_, err := p.provision()
		require.NoError(t, err, "calls=%v", calls)
		exec := findDockerCall(calls, "exec", "/proc/self/mountinfo")
		require.NotNil(t, exec, "calls=%v", calls)
		assert.Contains(t, scriptAfterC(exec), pinned,
			"the mount-table read must pin PATH before cat runs")
	})
	t.Run("delivery check", func(t *testing.T) {
		stagingTestEnv(t)
		var calls [][]string
		t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))
		p := &dockerProvisioner{
			image:   "img:latest",
			afBin:   stagingBinarySource(t),
			runArgs: []string{"-e", "PATH=/evil/nowhere"},
			spec:    ProvisionSpec{Title: "path", CloneURL: "file:///repo"},
		}
		_, err := p.provision()
		require.NoError(t, err, "calls=%v", calls)
		exec := findDockerCall(calls, "exec", "wc -c")
		require.NotNil(t, exec, "calls=%v", calls)
		assert.Contains(t, scriptAfterC(exec), pinned,
			"the delivery check must pin PATH before wc/tr run")
	})
}

// TestDockerStagingProxiedEndpointFallsBackToCp: a tcp:// loopback endpoint
// passes the non-account locality gate (reachable) but carries no proof the
// daemon is on this host — `ssh -L 2375:host:2375` makes a remote engine
// answer on 127.0.0.1 while resolving binds on the remote host. Without
// dockerEndpointOnThisHost, the bind path is skipped for the cp fallback.
func TestDockerStagingProxiedEndpointFallsBackToCp(t *testing.T) {
	stagingTestEnv(t)
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:2375")

	var calls [][]string
	t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))

	p := &dockerProvisioner{
		image: "img:latest",
		afBin: stagingBinarySource(t),
		spec:  ProvisionSpec{Title: "proxied", CloneURL: "file:///repo"},
	}
	_, err := p.provision()
	require.NoError(t, err, "calls=%v", calls)

	assert.False(t, p.payloadViaBinds, "a tcp endpoint carries no on-this-host proof")
	run := findDockerCall(calls, "run", "-d")
	require.NotNil(t, run, "calls=%v", calls)
	assert.Empty(t, stagingBindVolumes(run), "no staged binds may reach the run argv: %v", run)
	require.NotNil(t, findDockerCall(calls, "cp"), "calls=%v", calls)
}

// TestDockerStagingRetryAbortKeepsTheContainer: when the sharing-refused run
// DID create a container and `docker rm -f` fails, the retry must not run —
// clearing the handle would orphan a container no record can reach (the orphan
// sweeper spares a live session's slug). The failed provision keeps the
// handle so reapProvisionFailure owns the container.
func TestDockerStagingRetryAbortKeepsTheContainer(t *testing.T) {
	home := stagingTestEnv(t)
	var calls [][]string
	runCount := 0
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		switch args[0] {
		case "info":
			return []byte("staging-test-engine\n"), nil
		case "run":
			runCount++
			if runCount == 1 {
				return []byte("docker: Error response from daemon: Mounts denied:\r\nThe path /x is not shared from the host\n" + dockerCreatedID + "\n"),
					fmt.Errorf("exit status 125")
			}
			return []byte(dockerCreatedID + "\n"), nil
		case "rm":
			return nil, fmt.Errorf("exit status 1")
		}
		return nil, nil
	}))

	p := &dockerProvisioner{
		image: "img:latest",
		afBin: stagingBinarySource(t),
		spec:  ProvisionSpec{Title: "abort", CloneURL: "file:///repo"},
	}
	_, err := p.provision()
	require.Error(t, err, "the failed cleanup must surface rather than run a second container; calls=%v", calls)
	// The production pair: dockerRuntime.Provision wraps a failed provision()
	// in reapProvisionFailure — which now owns the refused container's rm.
	require.Error(t, p.reapProvisionFailure(err))
	assert.Equal(t, 1, runCount, "no retry may run while the first container is untracked")
	require.NotNil(t, findDockerCall(calls, "rm"),
		"the refused container's cleanup must be attempted; calls=%v", calls)
	assert.Equal(t, dockerCreatedID, p.containerID,
		"the handle must stay bound so the record can still reap it")

	// R5: the staging dir stays while `docker rm` has not confirmed the
	// container gone — the record's StagingDir still names a real directory.
	entries, rerr := os.ReadDir(filepath.Join(home, dockerStagingDirName))
	require.NoError(t, rerr)
	assert.Len(t, entries, 1, "the staging dir must outlive an unproven container reap")
}

// TestDockerStagingDirRestoredReap: a teardown rebuilt after a daemon restart
// re-roots the recorded staging base name under the af-owned staging root and
// removes it with the container. A record naming anything else — a full path,
// a parent hop, a name provision never minted — is refused, and the teardown
// touches nothing.
func TestDockerStagingDirRestoredReap(t *testing.T) {
	home := stagingTestEnv(t)
	var calls [][]string
	t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))

	staged := filepath.Join(home, dockerStagingDirName, Slugify("restored")+"-deadbeef")
	require.NoError(t, os.MkdirAll(staged, 0o700))

	data := &RuntimeCleanupData{Docker: &DockerRuntimeCleanupData{
		ContainerID: dockerCreatedID,
		EngineID:    "staging-test-engine",
		StagingDir:  filepath.Base(staged),
	}}
	backend, teardown, err := restoreRuntimeCleanupWithAccount("restored", "docker", data, restoredAccount{})
	require.NoError(t, err)
	// The record the restored backend persists must keep the staging name —
	// a session surviving a second restart can then reach the dir again.
	require.Equal(t, filepath.Base(staged),
		backend.(*dockerBackend).cleanup.StagingDir,
		"the re-persisted cleanup handle must keep the staging dir name")
	require.NoError(t, teardown())
	_, err = os.Stat(staged)
	assert.True(t, os.IsNotExist(err), "the restored teardown must remove the recorded staging dir")

	for _, bogus := range []string{"..", "../escape", filepath.Join(home, "elsewhere"), "other-title-deadbeef"} {
		data := &RuntimeCleanupData{Docker: &DockerRuntimeCleanupData{
			ContainerID: dockerCreatedID,
			EngineID:    "staging-test-engine",
			StagingDir:  bogus,
		}}
		_, teardown, err := restoreRuntimeCleanupWithAccount("restored", "docker", data, restoredAccount{})
		require.NoError(t, err)
		require.NoError(t, teardown())
	}
	_, err = os.Stat(filepath.Join(home, dockerStagingDirName))
	require.NoError(t, err, "a refused staging name must remove nothing")
}

// TestDockerStagingEnclosingRunArgsMountRefused is R1: a run_args mount whose
// destination encloses (or equals) an af staging destination does NOT collide
// as a duplicate — the engine would create a root-owned stub in the host
// source directory for af's file bind, and the stub persists. It is refused
// before `docker run` with an actionable error naming the entry.
func TestDockerStagingEnclosingRunArgsMountRefused(t *testing.T) {
	refused := []struct {
		name     string
		runArgs  []string
		want     string
		encloses bool
	}{
		{"bind over /usr/local/bin", []string{"-v", "/host/dir:/usr/local/bin"}, "-v /host/dir:/usr/local/bin", true},
		{"bind over /etc", []string{"--volume=/host/dir:/etc"}, "/etc", true},
		{"--mount flag", []string{"--mount", "type=bind,source=/h,target=/usr"}, "--mount", true},
		{"tmpfs", []string{"--tmpfs", "/usr/local"}, "/usr/local", true},
		{"root bind", []string{"-v", "/h:/"}, "-v /h:/", true},
		{"exact file dest", []string{"-v", "/h/f:" + dockerAfBinaryPath}, dockerAfBinaryPath, true},
		{"attached -v value", []string{"-v/host/dir:/usr/local/bin"}, "/usr/local/bin", true},
		{"clustered -v", []string{"-dv/host:/etc"}, "-dv/host:/etc", true},
		{"ambiguous cluster refused", []string{"-xv/host:/usr/local"}, "combined short option", false},
		{"--mount quoted csv", []string{"--mount", `type=bind,src=/host/dir,"target=/usr/local/bin"`}, "/usr/local/bin", true},
		{"--mount uppercase key", []string{"--mount", "type=bind,src=/h,TARGET=/etc"}, "/etc", true},
	}
	for _, tt := range refused {
		t.Run("refused/"+tt.name, func(t *testing.T) {
			home := stagingTestEnv(t)
			var calls [][]string
			t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))
			p := &dockerProvisioner{
				image:   "img:latest",
				afBin:   stagingBinarySource(t),
				runArgs: tt.runArgs,
				spec:    ProvisionSpec{Title: "enc", CloneURL: "file:///repo", AgentPolicy: []byte(`{"hooks":{}}`)},
			}
			_, err := p.provision()
			require.Error(t, err)
			if tt.encloses {
				assert.Contains(t, err.Error(), "encloses")
			}
			assert.Contains(t, err.Error(), tt.want)
			assert.Contains(t, err.Error(), "run_args")
			// A preflight config refusal must classify local so environmental
			// evidence (a full data root's ENOSPC) cannot borrow it.
			var se *dockerStepError
			require.ErrorAs(t, err, &se)
			assert.Equal(t, dockerOpLocal, se.op)
			require.Nil(t, findDockerCall(calls, "run", "-d"), "no session container may be started; calls=%v", calls)
			// The staged dir the refusal left behind still reaps cleanly.
			require.Error(t, p.reapProvisionFailure(err))
			entries, rerr := os.ReadDir(filepath.Join(home, dockerStagingDirName))
			require.NoError(t, rerr)
			assert.Empty(t, entries)
		})
	}
	allowed := []struct {
		name    string
		runArgs []string
	}{
		{"sibling under /usr/local/bin", []string{"-v", "/host/dir:/usr/local/bin/other"}},
		{"beneath the file destination", []string{"-v", "/host/dir:" + dockerAfBinaryPath + "/sub"}},
		{"workspace mount", []string{"-v", "/host:/workspace"}},
	}
	for _, tt := range allowed {
		t.Run("allowed/"+tt.name, func(t *testing.T) {
			stagingTestEnv(t)
			var calls [][]string
			t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))
			p := &dockerProvisioner{
				image:   "img:latest",
				afBin:   stagingBinarySource(t),
				runArgs: tt.runArgs,
				spec:    ProvisionSpec{Title: "ok", CloneURL: "file:///repo"},
			}
			_, err := p.provision()
			require.NoError(t, err, "calls=%v", calls)
		})
	}
}

// TestDockerStagingSymlinkedParentMountRefused: the image symlinks a
// destination parent (/usr/local/bin -> /opt/bin) and a repo bind mounts the
// RESOLVED parent — lexically unrelated, but runc resolves the af file bind
// through the symlink and creates the stub inside the host dir. The probe
// answers the real parents, and the mount is refused against them.
func TestDockerStagingSymlinkedParentMountRefused(t *testing.T) {
	stagingTestEnv(t)
	var calls [][]string
	old := dockerStagingProbeAnswer
	dockerStagingProbeAnswer = func(string) ([]byte, error) {
		return []byte("/opt/bin\n\x00/etc\n\x00\x00/opt/bin\n\x00"), nil
	}
	defer func() { dockerStagingProbeAnswer = old }()
	t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))
	p := &dockerProvisioner{
		image:   "img:latest",
		afBin:   stagingBinarySource(t),
		runArgs: []string{"-v", "/host/dir:/opt/bin"},
		spec:    ProvisionSpec{Title: "lnk", CloneURL: "file:///repo"},
	}
	_, err := p.provision()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "encloses")
	assert.Contains(t, err.Error(), "/opt/bin/af",
		"the refusal must name the RESOLVED af destination")
	require.Nil(t, findDockerCall(calls, "run", "-d"), "calls=%v", calls)
}

// TestDockerStagingMountDestSymlinkRefused is the inverse case: the image
// symlinks /opt -> /usr/local, so a repo mount at /opt/bin effectively lands
// at /usr/local/bin — enclosing the resolved af destination. The probe
// resolves the repo mount's own destination and the refusal names it.
func TestDockerStagingMountDestSymlinkRefused(t *testing.T) {
	stagingTestEnv(t)
	var calls [][]string
	old := dockerStagingProbeAnswer
	dockerStagingProbeAnswer = func(string) ([]byte, error) {
		return []byte("/usr/local/bin\n\x00/etc\n\x00\x00/usr/local/bin\n\x00"), nil
	}
	defer func() { dockerStagingProbeAnswer = old }()
	t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))
	p := &dockerProvisioner{
		image:   "img:latest",
		afBin:   stagingBinarySource(t),
		runArgs: []string{"-v", "/host/dir:/opt/bin"},
		spec:    ProvisionSpec{Title: "lnk2", CloneURL: "file:///repo"},
	}
	_, err := p.provision()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "encloses")
	require.Nil(t, findDockerCall(calls, "run", "-d"), "calls=%v", calls)
}

// TestDockerStagingProbeFailureFallsBackToCp: when the in-image resolution
// cannot be established (probe error, unreadable answer, or a symlinked
// destination leaf), the relationship to repo mounts cannot be established —
// bind delivery is dropped for the guarded cp fallback rather than guessed.
func TestDockerStagingProbeFailureFallsBackToCp(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(string) ([]byte, error)
	}{
		{"probe error", func(string) ([]byte, error) { return nil, errors.New("no shell in image") }},
		{"garbage answer", func(string) ([]byte, error) { return []byte("???"), nil }},
		{"symlinked leaf", func(string) ([]byte, error) {
			return []byte("/usr/local/bin\n\x00/etc\n\x00" + leafSymlinkMarker + "/usr/local/bin/af\n\x00/data\n\x00"), nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stagingTestEnv(t)
			var calls [][]string
			old := dockerStagingProbeAnswer
			dockerStagingProbeAnswer = tc.answer
			defer func() { dockerStagingProbeAnswer = old }()
			t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))
			p := &dockerProvisioner{
				image:   "img:latest",
				afBin:   stagingBinarySource(t),
				runArgs: []string{"-v", "/host/dir:/data"},
				spec:    ProvisionSpec{Title: "np", CloneURL: "file:///repo"},
			}
			_, err := p.provision()
			require.NoError(t, err, "calls=%v", calls)
			assert.False(t, p.payloadViaBinds, "unresolvable destinations must not use binds")
			run := findDockerCall(calls, "run", "-d")
			require.NotNil(t, run, "calls=%v", calls)
			for _, m := range stagingBindVolumes(run) {
				assert.NotContains(t, m, p.stagingDir,
					"no staged bind may reach the run argv: %v", run)
			}
			require.NotNil(t, findDockerCall(calls, "cp"), "calls=%v", calls)
		})
	}
}

// TestDockerStagingDeliveryConfirmFailsBackToCp is R2: when the post-create
// delivery check reports the staged files did not arrive — a unix:// endpoint
// with a different filesystem view the upfront proof cannot see — the
// provisioner takes the same one-shot retry as a sharing refusal: rm the
// mis-delivered container, run again without binds, deliver by cp.
func TestDockerStagingDeliveryConfirmFailsBackToCp(t *testing.T) {
	stagingTestEnv(t)
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		joined := strings.Join(args, " ")
		switch args[0] {
		case "info":
			return []byte("confirm-engine\n"), nil
		case "run":
			return []byte(dockerCreatedID + "\n"), nil
		case "cp", "rm":
			return nil, nil
		case "port":
			return []byte("127.0.0.1:49152\n"), nil
		case "exec":
			switch {
			case strings.Contains(joined, "wc -c"):
				// Delivery check: the staged af did not arrive as a
				// regular file of the staged size.
				return []byte(""), fmt.Errorf("exit status 1")
			case strings.Contains(joined, "id -u"):
				return []byte("0:0\n"), nil
			case strings.Contains(joined, "/proc/self/mountinfo"):
				return probeAnswer(args), nil
			case strings.Contains(joined, "cat "+dockerBannerPath):
				return []byte(`{"addr":":8000","token":"t","title":"staging"}` + "\n" + remoteProcAlive + "\n"), nil
			}
			return nil, nil
		}
		return nil, nil
	}))

	p := &dockerProvisioner{
		image: "img:latest",
		afBin: stagingBinarySource(t),
		spec:  ProvisionSpec{Title: "confirm", CloneURL: "file:///repo"},
	}
	_, err := p.provision()
	require.NoError(t, err, "calls=%v", calls)

	var runs [][]string
	for _, c := range calls {
		if len(c) > 0 && c[0] == "run" {
			runs = append(runs, c)
		}
	}
	require.Len(t, runs, 2, "exactly one retry is expected; calls=%v", calls)
	require.Empty(t, stagingBindVolumes(runs[1]),
		"the retry must not carry the binds that did not deliver; argv=%v", runs[1])
	require.NotNil(t, findDockerCall(calls, "rm"),
		"the mis-delivered container must be removed before the retry; calls=%v", calls)
	require.NotNil(t, findDockerCall(calls, "cp"),
		"the retried provision delivers via cp; calls=%v", calls)
}

// TestDockerStagingDirRemovedOnlyAfterRmSucceeds is R5: the staged dir goes
// only once the container is known gone. A `docker rm` failure or an engine
// check refusal leaves it — the cleanup record still needs it — while a
// successful rm and the never-created path both clear it.
func TestDockerStagingDirRemovedOnlyAfterRmSucceeds(t *testing.T) {
	newStaged := func(t *testing.T) string {
		home := t.TempDir()
		t.Setenv("AGENT_FACTORY_HOME", home)
		dir := filepath.Join(home, dockerStagingDirName, Slugify("reap")+"-0badc0de")
		require.NoError(t, os.MkdirAll(dir, 0o700))
		return dir
	}
	newP := func(dir, engineID string, verify bool) *dockerProvisioner {
		return &dockerProvisioner{
			containerID:        dockerCreatedID,
			engineID:           engineID,
			verifyEngineOnReap: verify,
			stagingDir:         dir,
			spec:               ProvisionSpec{Title: "reap"},
		}
	}

	t.Run("rm failure keeps the dir", func(t *testing.T) {
		dir := newStaged(t)
		t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
			if args[0] == "rm" {
				return []byte("Error response from daemon: rm failed"), fmt.Errorf("exit status 1")
			}
			return []byte("engine\n"), nil
		}))
		p := newP(dir, "engine", false)
		require.Error(t, p.reap())
		_, err := os.Stat(dir)
		require.NoError(t, err, "the staging dir must stay while the container's fate is unproven")
	})

	t.Run("engine check refusal keeps the dir", func(t *testing.T) {
		dir := newStaged(t)
		t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
			if args[0] == "info" {
				return []byte("other-engine\n"), nil
			}
			return nil, fmt.Errorf("unexpected call %v", args)
		}))
		p := newP(dir, "engine", true)
		require.Error(t, p.reap())
		_, err := os.Stat(dir)
		require.NoError(t, err, "the staging dir must stay while the engine check is unproven")
	})

	t.Run("rm success removes the dir", func(t *testing.T) {
		dir := newStaged(t)
		t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
			return nil, nil
		}))
		p := newP(dir, "engine", false)
		require.NoError(t, p.reap())
		_, err := os.Stat(dir)
		assert.True(t, os.IsNotExist(err), "a confirmed-gone container releases the staging dir")
	})
}

// TestRestoredDockerStagingDirMintedShape is R7: only the exact provision-minted
// name — Slugify(title)+"-"+8 lowercase hex — is restored; every other shape,
// prefix-adjacent or not, is refused.
func TestRestoredDockerStagingDirMintedShape(t *testing.T) {
	home := stagingTestEnv(t)
	slug := Slugify("My Session")
	tests := []struct {
		recorded string
		want     bool
	}{
		{slug + "-1a2b3c4d", true},
		{slug + "-1a2b3c4", false},   // 7 hex
		{slug + "-1a2b3c4d5", false}, // 9 hex
		{slug + "-1a2b3c4g", false},  // non-hex
		{slug + "-1A2B3C4D", false},  // uppercase — not minted
		{"other-1a2b3c4d", false},    // wrong slug
		{slug + "x-1a2b3c4d", false}, // slug prefix, not exact
		{"../" + slug + "-1a2b3c4d", false},
		{slug + "-1a2b3c4d/extra", false},
	}
	for _, tt := range tests {
		got := restoredDockerStagingDir("My Session", tt.recorded)
		if tt.want {
			assert.Equal(t, filepath.Join(home, dockerStagingDirName, tt.recorded), got, "recorded %q", tt.recorded)
		} else {
			assert.Empty(t, got, "recorded %q must be refused", tt.recorded)
		}
	}
}

// TestDockerReadOnlyMountpointErrorRewritten is R8: runc's mountpoint-creation
// failure under a read-only parent bind is rewritten into an actionable error
// rather than surfaced raw.
func TestDockerReadOnlyMountpointErrorRewritten(t *testing.T) {
	raw := "docker: Error response from daemon: error creating mountpoint '" + dockerAfBinaryPath + "': mkdir " + dockerAfBinaryPath + ": read-only file system."
	err := dockerReadOnlyMountpointError(raw, fmt.Errorf("exit status 126"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read-only bind")
	assert.Contains(t, err.Error(), dockerAfBinaryPath)
	assert.Contains(t, err.Error(), "run_args")
	assert.Nil(t, dockerReadOnlyMountpointError("docker: some other failure", nil))
}
