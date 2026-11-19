// #7577 item 3's guard-and-verify tests: every program af runs as root in the
// container — resolved through the container's own symlinks — must sit on the
// root filesystem, judged by the staged binary's kernel mount-table read; and
// on the cp path every destination af writes must stay on the container's own
// filesystem. The fail-closed cases (unreadable inspect, an unanswerable
// probe) refuse rather than guess.
package session

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
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

// rootGuardProbe renders the in-image destination probe's NUL-framed answer:
// the resolved parents of the two af write destinations, the (empty)
// leaf-marker field, then one resolved destination per configured mount.
func rootGuardProbe(resA, resB string, resolvedMounts ...string) func(string) ([]byte, error) {
	return func(string) ([]byte, error) {
		var b strings.Builder
		b.WriteString(resA + "\n\x00" + resB + "\n\x00\x00")
		for _, d := range resolvedMounts {
			b.WriteString(d + "\n\x00")
		}
		return []byte(b.String()), nil
	}
}

// TestDockerWriteGuardRefusesConfiguredMounts is the cp path's pre-write
// check: a daemon-configured mount whose destination encloses — or sits
// beneath — a resolved af write target is refused with an actionable error,
// and no cp runs. Every configured destination is judged in both spellings —
// the configured path and the probe's in-image resolution — because Docker
// resolves a bind at a symlinked destination onto its real target.
func TestDockerWriteGuardRefusesConfiguredMounts(t *testing.T) {
	tests := []struct {
		name        string
		mounts      []string // .Mounts destinations
		tmpfs       []string // .HostConfig.Tmpfs keys
		resA        string   // probe-resolved af parent (default /usr/local/bin)
		resolved    []string // probe-resolved mount dests (default: configured)
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
			name:        "mount beneath a directory destination",
			mounts:      []string{"/usr/local/bin/af/inner"},
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
		{
			name:        "a symlinked configured dest resolves over the af directory",
			mounts:      []string{"/img/link"},
			resolved:    []string{"/usr/local/bin"},
			wantRefused: dockerAfBinaryPath,
		},
		{
			name:        "the af parent itself resolves onto a configured bind",
			mounts:      []string{"/mnt/host-bin"},
			resA:        "/mnt/host-bin",
			wantRefused: dockerAfBinaryPath,
		},
		{
			name:   "resolved parent with a trailing space is judged byte-exactly",
			mounts: []string{"/usr/local/bin "},
			resA:   "/usr/local/bin ",
			// A mount over "/usr/local/bin " covers "/usr/local/bin /af" — a
			// TrimSpace'd read would judge "/usr/local/bin/af" and miss it.
			wantRefused: dockerAfBinaryPath,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stagingTestEnv(t)
			var calls [][]string
			oldI := dockerInspectAnswer
			dockerInspectAnswer = func(string) ([]byte, error) {
				return inspectMountsDoc(t, tt.mounts, tt.tmpfs), nil
			}
			defer func() { dockerInspectAnswer = oldI }()
			if tt.resA != "" || tt.resolved != nil {
				resA := tt.resA
				if resA == "" {
					resA = "/usr/local/bin"
				}
				resolved := tt.resolved
				if resolved == nil {
					resolved = append(append([]string{}, tt.mounts...), tt.tmpfs...)
				}
				old := dockerStagingProbeAnswer
				dockerStagingProbeAnswer = rootGuardProbe(resA, "/etc", resolved...)
				defer func() { dockerStagingProbeAnswer = old }()
			}
			t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))

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

// TestDockerWriteGuardAllowsRootfs pins the allowed case: no configured mount
// touches a resolved af write target. The second row is the --tmpfs /tmp
// preservation the /etc policy path exists for: a private tmpfs over /tmp is a
// common hardening flag, and with the policy at /etc nothing af writes lands
// beneath it, so the fallback still delivers.
func TestDockerWriteGuardAllowsRootfs(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mounts []string
		tmpfs  []string
		resA   string
		policy bool
	}{
		{name: "plain rootfs"},
		{name: "a private tmpfs over /tmp elsewhere is irrelevant", tmpfs: []string{"/tmp"}, policy: true},
		{
			name:   "a trailing-spaced sibling is not under the unspaced mount",
			mounts: []string{"/usr/local/bin"},
			resA:   "/usr/local/bin ",
			// "/usr/local/bin/af" is under this mount; "/usr/local/bin /af"
			// is not — judging the resolved path byte-exactly is what lets
			// the write proceed.
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stagingTestEnv(t)
			var calls [][]string
			oldI := dockerInspectAnswer
			dockerInspectAnswer = func(string) ([]byte, error) {
				return inspectMountsDoc(t, tt.mounts, tt.tmpfs), nil
			}
			defer func() { dockerInspectAnswer = oldI }()
			if tt.resA != "" {
				old := dockerStagingProbeAnswer
				dockerStagingProbeAnswer = rootGuardProbe(tt.resA, "/etc", tt.mounts...)
				defer func() { dockerStagingProbeAnswer = old }()
			}
			t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))

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
	oldI := dockerInspectAnswer
	dockerInspectAnswer = func(string) ([]byte, error) {
		return inspectMountsDoc(t, []string{"/opt/elsewhere"}, nil), nil
	}
	defer func() { dockerInspectAnswer = oldI }()
	old := dockerStagingProbeAnswer
	dockerStagingProbeAnswer = func(string) ([]byte, error) {
		return []byte("/usr/local/bin\n\x00/etc\n\x00" + leafSymlinkMarker + dockerAfBinaryPath + "\n\x00/opt/elsewhere\n\x00"), nil
	}
	defer func() { dockerStagingProbeAnswer = old }()
	t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))

	p := &dockerProvisioner{
		image: "img:latest",
		afBin: filepath.Join(t.TempDir(), "missing-af"),
		spec:  ProvisionSpec{Title: "symlink", CloneURL: "file:///repo"},
	}
	_, err := p.provision()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink")
	assert.Contains(t, err.Error(), dockerAfBinaryPath)
	require.Nil(t, findDockerCall(calls, "cp"), "calls=%v", calls)
}

// TestDockerWriteGuardUnreadableMountsRefuses: the guard is fail-closed — an
// inspect failure, output it cannot parse, or a resolution probe that cannot
// answer refuses the write rather than hoping the destination is safe.
func TestDockerWriteGuardUnreadableMountsRefuses(t *testing.T) {
	for _, tt := range []struct {
		name    string
		inspect func(string) ([]byte, error)
		probe   func(string) ([]byte, error)
	}{
		{name: "inspect fails", inspect: func(string) ([]byte, error) {
			return nil, fmt.Errorf("exit status 1")
		}},
		{name: "unparseable inspect answer", inspect: func(string) ([]byte, error) {
			return []byte("not a container description\n"), nil
		}},
		{name: "resolution probe fails", probe: func(string) ([]byte, error) {
			// A mount is configured, so the probe must answer — its failure
			// means the destinations' real paths are unproven.
			return nil, fmt.Errorf("exit status 1")
		}, inspect: func(string) ([]byte, error) {
			return []byte(`{"Mounts":[{"Type":"bind","Source":"/h","Destination":"/opt/x"}],"HostConfig":{"Tmpfs":{}}}` + "\n"), nil
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stagingTestEnv(t)
			var calls [][]string
			oldI := dockerInspectAnswer
			dockerInspectAnswer = tt.inspect
			defer func() { dockerInspectAnswer = oldI }()
			oldP := dockerStagingProbeAnswer
			dockerStagingProbeAnswer = tt.probe
			defer func() { dockerStagingProbeAnswer = oldP }()
			t.Cleanup(stagingDockerAnswers(t, &calls, "moby", false, nil, nil))

			p := &dockerProvisioner{
				image: "img:latest",
				afBin: filepath.Join(t.TempDir(), "missing-af"),
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

// --- probe adaptation stubs: symbols the fix adds, stubbed so this suite
// compiles against the OLD mechanism. The property tests must be RED here and
// GREEN with the fix. The test-hook vars the copied staging file declares
// (dockerInspectAnswer, dockerStagingProbeAnswer, dockerRootVerifyAnswer) are
// never consulted by master's mechanism — a test that sets them finds no
// effect, and that absence IS the red.
const dockerRootScriptPrefix = "export PATH=/usr/sbin:/usr/bin:/sbin:/bin; "

// dockerRootProgramPaths/Names mirror the fix's program inventory so the
// PinPATH test can assert the verifier's argv covers it; on master there is
// no verifier exec at all, which the test reports as missing.
var dockerRootProgramPaths = []string{dockerContainerShell, dockerContainerCat, "/usr/bin/stat", "/bin/stat"}
var dockerRootProgramNames = []string{"cat", "chmod", "chown", "find", "git", "id", "mkdir"}
