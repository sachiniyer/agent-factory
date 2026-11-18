package session

// Tests for #7577 item 2: af's payload (the af binary, the operator policy)
// reaches the container through af-owned read-only binds appended LAST in
// `docker run`, with a cp+chmod fallback that runs only behind
// verifyWriteTargets' mount-table proof.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// writeGuardTableAnswer renders the mount-table read's answer in the two-dir
// form verifyWriteTargets parses: the resolved parent of each write
// destination, then a mountinfo whose "/" is overlay and whose extra mounts
// are (mountPoint, fstype) pairs.
func writeGuardTableAnswer(realA, realB string, mounts ...[2]string) []byte {
	lines := []string{realA, realB, mountinfoLine(1, "/", "overlay")}
	for i, m := range mounts {
		lines = append(lines, mountinfoLine(10+i, m[0], m[1]))
	}
	return []byte(strings.Join(lines, "\n") + "\n")
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
			case strings.Contains(joined, "/proc/self/mountinfo"):
				if mountAnswer != nil {
					return mountAnswer(joined)
				}
				return mountTableAnswer(), nil
			case strings.Contains(joined, "cat "+dockerBannerPath):
				return []byte(`{"addr":":8000","token":"t","title":"staging"}` + "\n" + remoteProcAlive + "\n"), nil
			}
			return nil, nil
		}
		return nil, nil
	})
}

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

	run := findDockerCall(calls, "run")
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

	run := findDockerCall(calls, "run")
	require.NotNil(t, run)
	require.NotEmpty(t, findDockerCall(calls, "run", ":"+dockerAfBinaryPath+":"),
		"a rootless engine reads the daemon-owned staging dir — the bind path applies; argv=%v", run)
	require.Nil(t, findDockerCall(calls, "cp"), "calls=%v", calls)
}

// TestDockerStagingDuplicateDestinationIsActionable: a run_args mount already
// claiming an af destination makes docker refuse the pair as a duplicate; the
// error must name the destination and the fix rather than surface docker's
// raw text.
func TestDockerStagingDuplicateDestinationIsActionable(t *testing.T) {
	stagingTestEnv(t)
	var calls [][]string
	t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil,
		func(string, int) ([]byte, error) {
			return []byte("docker: Error response from daemon: Duplicate mount point: " + dockerAfBinaryPath + "\n"),
				fmt.Errorf("exit status 125")
		}))

	p := &dockerProvisioner{
		image:   "img:latest",
		afBin:   stagingBinarySource(t),
		runArgs: []string{"-v", "/repo/evil:" + dockerAfBinaryPath},
		spec:    ProvisionSpec{Title: "dup", CloneURL: "file:///repo"},
	}
	_, err := p.provision()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate mount destination")
	assert.Contains(t, err.Error(), dockerAfBinaryPath)
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

	run := findDockerCall(calls, "run")
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
// cp path, a write target enclosed by — or covering — a mount that is neither
// the container rootfs nor a tmpfs is refused with an actionable error, and
// no cp runs.
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
			realB:       "/tmp",
			mounts:      [][2]string{{"/usr/local/bin", "ext4"}},
			wantRefused: dockerAfBinaryPath,
		},
		{
			name:        "bind at the af file itself",
			realA:       "/usr/local/bin",
			realB:       "/tmp",
			mounts:      [][2]string{{"/usr/local/bin/af", "ext4"}},
			wantRefused: dockerAfBinaryPath,
		},
		{
			name:        "bind over the policy directory",
			realA:       "/usr/local/bin",
			realB:       "/tmp",
			mounts:      [][2]string{{"/tmp", "ext4"}},
			wantRefused: dockerPolicyPath,
			policy:      true,
		},
		{
			name:        "symlinked parent resolving onto a bind",
			realA:       "/mnt/host-bin",
			realB:       "/tmp",
			mounts:      [][2]string{{"/mnt/host-bin", "ext4"}},
			wantRefused: dockerAfBinaryPath,
		},
		{
			name:        "mount beneath a directory destination",
			realA:       "/usr/local/bin",
			realB:       "/tmp",
			mounts:      [][2]string{{"/usr/local/bin/af/inner", "nfs"}},
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

// TestDockerWriteGuardAllowsRootfsAndTmpfs pins the two allowed cases: the
// destination resolving under "/" itself, and a destination on a tmpfs —
// whose contents never reach a host file.
func TestDockerWriteGuardAllowsRootfsAndTmpfs(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mounts [][2]string
	}{
		{name: "plain rootfs"},
		{name: "tmpfs over the af directory", mounts: [][2]string{{"/usr/local/bin", "tmpfs"}}},
		{name: "tmpfs at the af file", mounts: [][2]string{{"/usr/local/bin/af", "tmpfs"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stagingTestEnv(t)
			var calls [][]string
			answer := writeGuardTableAnswer("/usr/local/bin", "/tmp", tt.mounts...)
			t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false,
				func(string) ([]byte, error) { return answer, nil }, nil))

			p := &dockerProvisioner{
				image: "img:latest",
				afBin: filepath.Join(t.TempDir(), "missing-af"),
				spec:  ProvisionSpec{Title: "allowed", CloneURL: "file:///repo"},
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
	answer := append(writeGuardTableAnswer("/usr/local/bin", "/tmp"),
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
