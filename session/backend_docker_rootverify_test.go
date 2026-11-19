// #7577 item 3's guard-and-verify tests: every program af runs as root in the
// container — resolved inside the running container, component and hop alike
// — must sit on the root filesystem, judged by the staged binary's kernel
// mount-table read; and on the cp path every destination af writes must stay
// on the container's own filesystem. The host-side guard is a lexical
// failfast; the fail-closed cases (unreadable inspect) refuse rather than
// guess.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// inspectMountsDoc marshals the `docker inspect` answer the cp path's
// write-target guard reads: every configured mount destination (bind, volume
// or tmpfs alike) plus the HostConfig.Tmpfs keys, which .Mounts does not
// carry.
func inspectMountsDoc(t *testing.T, dests, tmpfs []string) []byte {
	t.Helper()
	mounts := make([]map[string]string, 0, len(dests))
	for _, d := range dests {
		mounts = append(mounts, map[string]string{"Type": "bind", "Source": "/host" + d, "Destination": d})
	}
	tmp := map[string]string{}
	for _, d := range tmpfs {
		tmp[d] = ""
	}
	doc, err := json.Marshal(map[string]any{
		"Mounts":     mounts,
		"HostConfig": map[string]any{"Tmpfs": tmp},
	})
	require.NoError(t, err)
	return doc
}

// TestDockerWriteGuardRefusesConfiguredMounts is the cp path's lexical
// failfast: a daemon-configured mount whose destination encloses an af write
// target on its literal spelling is refused before any cp — the same verdict
// the in-container verifier gives the resolved path, earlier and cheaper.
// Resolution itself is never judged here: only the trusted verifier inside
// the running container sees the mounts as applied.
func TestDockerWriteGuardRefusesConfiguredMounts(t *testing.T) {
	tests := []struct {
		name        string
		mounts      []string // .Mounts destinations
		tmpfs       []string // .HostConfig.Tmpfs keys
		wantRefused string
		policy      bool
	}{
		{
			name:        "bind over the af directory",
			mounts:      []string{"/usr/local/bin"},
			wantRefused: dockerAfBinaryPath,
		},
		{
			name:        "bind at the af file itself",
			mounts:      []string{dockerAfBinaryPath},
			wantRefused: dockerAfBinaryPath,
		},
		{
			name:        "bind over the policy directory",
			mounts:      []string{"/etc"},
			wantRefused: dockerPolicyPath,
			policy:      true,
		},
		{
			name:        "the whole container root",
			mounts:      []string{"/"},
			wantRefused: dockerAfBinaryPath,
		},
		{
			name:        "private tmpfs over the af directory",
			tmpfs:       []string{"/usr/local/bin"},
			wantRefused: dockerAfBinaryPath,
		},
		{
			name:        "tmpfs over the policy directory",
			tmpfs:       []string{"/etc"},
			wantRefused: dockerPolicyPath,
			policy:      true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := stagingTestEnv(t)
			var calls [][]string
			oldI := dockerInspectAnswer
			dockerInspectAnswer = func(string) ([]byte, error) {
				return inspectMountsDoc(t, tt.mounts, tt.tmpfs), nil
			}
			defer func() { dockerInspectAnswer = oldI }()
			t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))

			spec := ProvisionSpec{Title: "guarded", CloneURL: "file:///repo"}
			if tt.policy {
				spec.AgentPolicy = []byte(`{"hooks":{}}`)
			}
			forceCpFallback(t, home) // staging fails → the cp fallback runs
			p := &dockerProvisioner{
				image: "img:latest",
				afBin: stagingBinarySource(t),
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

// TestDockerWriteGuardAllowsRootfs pins the allowed case: no configured mount
// lexically encloses an af write target. Anything subtler — a mount that
// changes a resolved path, a destination with absent parents, a mount
// beneath the file itself — is deliberately left to the in-container
// verifier, which walks the real filesystem after all mounts are applied;
// the guard only stops the plainly hopeless writes before cp.
func TestDockerWriteGuardAllowsRootfs(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mounts []string
		tmpfs  []string
		policy bool
	}{
		{name: "plain rootfs"},
		{name: "a private tmpfs over /tmp elsewhere is irrelevant", tmpfs: []string{"/tmp"}, policy: true},
		{
			name:   "a sibling mount under the destination parent is allowed",
			mounts: []string{"/etc/ssl"},
			policy: true,
		},
		{
			name:   "a trailing-spaced mount is a different destination",
			mounts: []string{"/usr/local/bin "},
		},
		{
			name:   "a mount beneath the destination file covers nothing",
			mounts: []string{"/usr/local/bin/af/inner"},
		},
		{
			// #7607 review: a credential bind's destination parents may be
			// absent from the image — nothing for the lexical guard to
			// refuse, and the verifier resolves the tail in-container.
			name:   "a credential mount with absent parents is allowed",
			mounts: []string{"/root/.codex/auth.json"},
		},
		{
			// Resolved-path judgment is the verifier's: mounts that only a
			// post-mounts resolution can expose pass the guard and are
			// judged by the verdict exec (default fake answer: all OK).
			name:   "the account bind is not exempt",
			mounts: []string{dockerAccountHome},
		},
		{
			name:   "a mount that could retarget an intermediate link",
			mounts: []string{"/opt"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := stagingTestEnv(t)
			var calls [][]string
			oldI := dockerInspectAnswer
			dockerInspectAnswer = func(string) ([]byte, error) {
				return inspectMountsDoc(t, tt.mounts, tt.tmpfs), nil
			}
			defer func() { dockerInspectAnswer = oldI }()
			t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))
			forceCpFallback(t, home)

			spec := ProvisionSpec{Title: "allowed", CloneURL: "file:///repo"}
			if tt.policy {
				spec.AgentPolicy = []byte(`{"hooks":{}}`)
			}
			p := &dockerProvisioner{
				image: "img:latest",
				afBin: stagingBinarySource(t),
				spec:  spec,
			}
			_, err := p.provision()
			require.NoError(t, err, "calls=%v", calls)
			require.NotNil(t, findDockerCall(calls, "cp"),
				"an allowed target must reach the cp; calls=%v", calls)
		})
	}
}

// TestDockerVerifierRefusesResolvedMounts is the property's authority row:
// mounts whose destinations do not lexically enclose an af write target —
// but resolve onto one inside the running container, after every bind is
// applied — are caught by the staged binary's verifier and refused. One row
// per #7607-review shape: the account bind an image alias lands an af write
// inside (P1), and the intermediate symlink hop a mount retargets (P1);
// plus the symlink destination leaf `docker cp` would follow. The
// internal/dockerrootverify package's table tests pin the verdicts
// themselves; this file pins that a FAIL verdict refuses the provision.
func TestDockerVerifierRefusesResolvedMounts(t *testing.T) {
	for _, tt := range []struct {
		name    string
		mounts  []string
		verdict string
	}{
		{
			name:   "the destination resolves onto the account bind",
			mounts: []string{dockerAccountHome},
			verdict: "FAIL write " + dockerAfBinaryPath + ": the link /usr/local/bin -> " +
				dockerAccountHome + "/bin: " + dockerAccountHome + " is covered by the mount " + dockerAccountHome,
		},
		{
			name:   "a mount retargets an intermediate symlink hop",
			mounts: []string{"/opt"},
			verdict: "FAIL write " + dockerAfBinaryPath +
				": the link /usr/local/bin -> /opt/link: /opt is covered by the mount /opt",
		},
		{
			name:    "the destination leaf is a symlink in the image",
			verdict: "FAIL write " + dockerAfBinaryPath + ": the destination leaf is a symlink",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := stagingTestEnv(t)
			oldI := dockerInspectAnswer
			dockerInspectAnswer = func(string) ([]byte, error) {
				return inspectMountsDoc(t, tt.mounts, nil), nil
			}
			defer func() { dockerInspectAnswer = oldI }()
			oldV := dockerRootVerifyAnswer
			dockerRootVerifyAnswer = func(string) ([]byte, error) {
				return []byte(tt.verdict + "\n"), nil
			}
			defer func() { dockerRootVerifyAnswer = oldV }()
			var calls [][]string
			t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))
			forceCpFallback(t, home)

			p := &dockerProvisioner{
				image: "img:latest",
				afBin: stagingBinarySource(t),
				spec:  ProvisionSpec{Title: "resolved", CloneURL: "file:///repo"},
			}
			_, err := p.provision()
			require.Error(t, err, "calls=%v", calls)
			assert.Contains(t, err.Error(), "refusing to run root commands",
				"a FAIL verdict must refuse the provision: %v", err)
			require.Nil(t, findDockerCall(calls, "cp", dockerPolicyPath),
				"the refusal precedes the policy cp; calls=%v", calls)
		})
	}
}

// TestDockerWriteGuardUnreadableMountsRefuses: the guard is fail-closed — an
// inspect failure or output it cannot parse refuses the write rather than
// hoping the destination is safe.
func TestDockerWriteGuardUnreadableMountsRefuses(t *testing.T) {
	for _, tt := range []struct {
		name    string
		inspect func(string) ([]byte, error)
	}{
		{name: "inspect fails", inspect: func(string) ([]byte, error) {
			return nil, fmt.Errorf("exit status 1")
		}},
		{name: "unparseable inspect answer", inspect: func(string) ([]byte, error) {
			return []byte("not a container description\n"), nil
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := stagingTestEnv(t)
			var calls [][]string
			oldI := dockerInspectAnswer
			dockerInspectAnswer = tt.inspect
			defer func() { dockerInspectAnswer = oldI }()
			t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))
			forceCpFallback(t, home)

			p := &dockerProvisioner{
				image: "img:latest",
				afBin: stagingBinarySource(t),
				spec:  ProvisionSpec{Title: "closed", CloneURL: "file:///repo"},
			}
			_, err := p.provision()
			require.Error(t, err)
			require.Nil(t, findDockerCall(calls, "cp"), "calls=%v", calls)
		})
	}
}

// TestDockerStagingExecScriptsPinPATH is item 3's PATH property: a plain
// session's run_args may set `-e PATH=/evil:…`, and docker exec inherits the
// container's configured env — so EVERY `sh -c` script af execs must pin PATH
// before naming any external program, and the root-exec verifier must ask
// after every PATH-resolved name a script uses, so a repo-controlled
// directory can supply none of them.
func TestDockerStagingExecScriptsPinPATH(t *testing.T) {
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

	var scripts int
	for _, call := range calls {
		if len(call) == 0 || call[0] != "exec" {
			continue
		}
		for i, a := range call {
			if a == "-c" && i+1 < len(call) {
				scripts++
				// The agent-server wrap is the one unpinned script: an
				// exported PATH would leak into the long-lived server's
				// environment and the workload would inherit the pin. It
				// names no PATH-resolved program — the af path is absolute.
				if strings.Contains(call[i+1], "__af-agent-server-env-exec") {
					continue
				}
				assert.True(t, strings.HasPrefix(call[i+1], dockerRootScriptPrefix),
					"every root exec script must pin PATH first: %v", call)
			}
		}
	}
	assert.Greater(t, scripts, 0, "the provision must exec at least one script; calls=%v", calls)

	// The verifier runs as direct argv — no shell, so PATH names nothing —
	// and asks after every program a root script or absolute exec uses:
	// the absolute targets plus every name the pinned PATH resolves.
	verify := findDockerCall(calls, "exec", "__docker-root-verify")
	require.NotNil(t, verify, "calls=%v", calls)
	joined := strings.Join(verify, " ")
	for _, prog := range dockerRootProgramPaths {
		assert.Contains(t, joined, "prog="+prog, "the verifier must check %s: %v", prog, verify)
	}
	for _, name := range dockerRootProgramNames {
		assert.Contains(t, joined, " prog="+name, "the verifier must check PATH-resolved %q: %v", name, verify)
	}
}

// TestDockerRootVerifyRefusesMountedProgram is item 3's mount property: a
// docker.run_args bind shadowing a program af execs as root — a hostile file
// at /bin/cat, which runc mounts onto the resolved /usr/bin/cat — is caught by
// the staged binary's verifier reading the kernel mount table, and the
// session refuses before any root script runs. A FAIL verdict is a refusal,
// not a delivery miss: no cp retry runs around it.
func TestDockerRootVerifyRefusesMountedProgram(t *testing.T) {
	stagingTestEnv(t)
	verifyCalls := 0
	old := dockerRootVerifyAnswer
	dockerRootVerifyAnswer = func(string) ([]byte, error) {
		verifyCalls++
		return []byte("FAIL prog /bin/cat: resolves to /usr/bin/cat, covered by mount /usr/bin/cat\n"), nil
	}
	defer func() { dockerRootVerifyAnswer = old }()

	var calls [][]string
	t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))
	p := &dockerProvisioner{
		image:   "img:latest",
		afBin:   stagingBinarySource(t),
		runArgs: []string{"-v", "/host/evil-cat:/bin/cat"},
		spec:    ProvisionSpec{Title: "evilcat", CloneURL: "file:///repo"},
	}
	_, err := p.provision()
	require.Error(t, err, "calls=%v", calls)
	assert.Contains(t, err.Error(), "/usr/bin/cat", "the refusal must name the covering mount: %v", err)
	assert.Equal(t, 1, verifyCalls, "a FAIL verdict ends the provision — no retry")
	require.Nil(t, findDockerCall(calls, "cp"), "a refusal must not take the cp fallback; calls=%v", calls)
}

// TestDockerRootVerifyUndeliveredRetriesCp: an exec that produced no verdicts
// means the staged binds did not deliver af's binary — the documented
// visibility false-positive (#7577 item 2 R2) — and the one-shot cp retry
// delivers it.
func TestDockerRootVerifyUndeliveredRetriesCp(t *testing.T) {
	stagingTestEnv(t)
	verifyCalls := 0
	old := dockerRootVerifyAnswer
	dockerRootVerifyAnswer = func(string) ([]byte, error) {
		verifyCalls++
		if verifyCalls == 1 {
			return nil, fmt.Errorf("exit status 126")
		}
		return []byte("OK\n"), nil
	}
	defer func() { dockerRootVerifyAnswer = old }()

	var calls [][]string
	t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))
	p := &dockerProvisioner{
		image: "img:latest",
		afBin: stagingBinarySource(t),
		spec:  ProvisionSpec{Title: "undelivered", CloneURL: "file:///repo"},
	}
	_, err := p.provision()
	require.NoError(t, err, "calls=%v", calls)
	assert.Equal(t, 2, verifyCalls, "the cp-delivered binary must verify too")

	var runs [][]string
	for _, c := range calls {
		if len(c) > 0 && c[0] == "run" && containsArg(c, "-d") {
			runs = append(runs, c)
		}
	}
	require.Len(t, runs, 2, "exactly one retry is expected; calls=%v", calls)
	require.Empty(t, stagingBindVolumes(runs[1]),
		"the retry must not carry the undelivered binds; argv=%v", runs[1])
	require.NotNil(t, findDockerCall(calls, "cp"),
		"the retried provision delivers via cp; calls=%v", calls)
}

// TestDockerRootVerifyExecFailureKeepsSessionExecClass is the #7607-review
// P2: a verifier exec that cannot run on the cp path — a full docker data
// root refusing exec-state creation — is a session-exec failure, so its
// ENOSPC reaches the capacity classifier as errno evidence instead of
// consuming the structural retry budget a local refusal would.
func TestDockerRootVerifyExecFailureKeepsSessionExecClass(t *testing.T) {
	host, herr := os.Hostname()
	require.NoError(t, herr)
	stubPeerDataRoot(t, 0, "8:1", peerTableOn("8:1"), peerTableOn("8:1"), nil)
	old1, old2 := statfsFull, statfsFullUnprivileged
	statfsFull = func(string) (bool, error) { return true, nil }
	statfsFullUnprivileged = func(string) (bool, error) { return true, nil }
	defer func() { statfsFull, statfsFullUnprivileged = old1, old2 }()
	old3 := sameFilesystem
	sameFilesystem = func(string, string) bool { return true }
	defer func() { sameFilesystem = old3 }()
	oldV := dockerRootVerifyAnswer
	dockerRootVerifyAnswer = func(string) ([]byte, error) {
		return []byte("exec: Cannot run program: no space left on device\n"), fmt.Errorf("exit status 1")
	}
	defer func() { dockerRootVerifyAnswer = oldV }()
	fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false),
		func(args []string) ([]byte, error) {
			switch {
			case args[0] == "run":
				return dockerRunOrProbeAnswer(args)
			case args[0] == "exec" && containsArg(args, "__docker-root-verify"):
				return dockerRootVerifyAnswer(strings.Join(args, " "))
			}
			return nil, nil
		})
	home := stagingTestEnv(t)
	forceCpFallback(t, home)
	p := &dockerProvisioner{
		image: "img:latest",
		afBin: stagingBinarySource(t),
		spec:  ProvisionSpec{Title: "verify-enospc", CloneURL: "file:///repo"},
	}
	_, err := p.provision()
	require.Error(t, err)
	assert.True(t, errors.Is(err, syscall.ENOSPC),
		"the verify exec's ENOSPC must classify as a session-exec failure: %v", err)
}
