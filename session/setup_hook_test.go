package session

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// setup_commands (#6667) run on the daemon's machine after a session's
// provisioning has established its transport — ssh command, agent-server,
// auth — and before post_worktree_commands and the agent start. These tests
// pin the runner's environment contract, its sequential/failure semantics,
// and the ssh route it hands the script.

// writeSetupCommandsConfig materializes a repo-local .agent-factory/config.toml
// carrying the given setup_commands TOML body.
func writeSetupCommandsConfig(t *testing.T, repoRoot, body string) {
	t.Helper()
	dir := filepath.Join(repoRoot, ".agent-factory")
	require.NoError(t, os.MkdirAll(dir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0644))
}

func localSetupStaging(t *testing.T, sessionID string) string {
	t.Helper()
	dir, err := config.GetConfigDir()
	require.NoError(t, err)
	return filepath.Join(dir, "af-setup", sessionID)
}

func TestSplitSSHDestination(t *testing.T) {
	tests := []struct {
		name      string
		sshCmd    string
		transport string
		dest      string
		ok        bool
	}{
		{
			name:      "simple command",
			sshCmd:    "ssh -p 22 dev@box",
			transport: "ssh -p 22",
			dest:      "dev@box",
			ok:        true,
		},
		{
			name:      "af-composed ssh backend command (quoted options, quoted dest)",
			sshCmd:    `ssh -F none -o 'User=dev' -p 2200 -o 'UserKnownHostsFile=/tmp/kh' -o BatchMode=yes -o GlobalKnownHostsFile=/dev/null 'real.example'`,
			transport: `ssh -F none -o 'User=dev' -p 2200 -o 'UserKnownHostsFile=/tmp/kh' -o BatchMode=yes -o GlobalKnownHostsFile=/dev/null`,
			dest:      "real.example",
			ok:        true,
		},
		{
			name:      "quoted path argument earlier in the command",
			sshCmd:    `ssh -i '/path with space/id_ed25519' -p 2222 dev@box`,
			transport: `ssh -i '/path with space/id_ed25519' -p 2222`,
			dest:      "dev@box",
			ok:        true,
		},
		{
			name:      "double-quoted destination",
			sshCmd:    `ssh -p 22 "dev@box"`,
			transport: "ssh -p 22",
			dest:      "dev@box",
			ok:        true,
		},
		{
			name:      "surrounding whitespace",
			sshCmd:    "  ssh -p 22 dev@box  ",
			transport: "ssh -p 22",
			dest:      "dev@box",
			ok:        true,
		},
		{name: "empty command", sshCmd: "", ok: false},
		{name: "no destination token", sshCmd: "ssh", ok: false},
		{name: "unbalanced quote in destination", sshCmd: "ssh 'dev@box", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport, dest, ok := splitSSHDestination(tt.sshCmd)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.transport, transport)
			assert.Equal(t, tt.dest, dest)
		})
	}
}

// The local-mode contract: the script runs in the project checkout on the
// daemon, AF_SETUP_DEST is a local staging dir af creates, and no ssh names
// are exported.
func TestSetupHookLocalEnvContract(t *testing.T) {
	repoRoot := initTempGitRepo(t)
	writeSetupCommandsConfig(t, repoRoot, `setup_commands = [
  "env | grep '^AF_' | sort > \"$AF_SETUP_DEST/env.out\"",
  "pwd > \"$AF_SETUP_DEST/cwd.out\"",
]
`)

	opts := InstanceOptions{ID: "sess-local-1", Title: "sess", Path: repoRoot}
	note := runSessionSetupHook(opts, repoRoot, BackendLocal, &LocalBackend{}, false)
	assert.Equal(t, "", note)

	staging := filepath.Join(localSetupStaging(t, "sess-local-1"), "payload")
	envOut, err := os.ReadFile(filepath.Join(staging, "env.out"))
	require.NoError(t, err)
	env := string(envOut)
	assert.Contains(t, env, "AF_SETUP_SESSION_ID=sess-local-1")
	assert.Contains(t, env, "AF_SETUP_SESSION=sess")
	assert.Contains(t, env, "AF_SETUP_SOURCE="+repoRoot)
	assert.Contains(t, env, "AF_SETUP_MODE=local")
	assert.Contains(t, env, "AF_SETUP_DEST="+staging)
	assert.NotContains(t, env, "AF_SSH")

	cwd, err := os.ReadFile(filepath.Join(staging, "cwd.out"))
	require.NoError(t, err)
	assert.Equal(t, repoRoot, strings.TrimSpace(string(cwd)),
		"setup commands run with the project checkout as working directory")
}

// The remote-mode contract: af creates the staging dir on the session's host
// through the session's own ssh command, and exports the ssh route in the
// AF_SSH/AF_SSH_DEST/AF_SSH_FULL shape move_cmd established.
func TestSetupHookRemoteEnvContractAndRoute(t *testing.T) {
	repoRoot := initTempGitRepo(t)
	writeSetupCommandsConfig(t, repoRoot, `setup_commands = [
  "env | grep '^AF_' | sort > env.out",
]
`)

	var ran []string
	prov := &sandboxProvisioner{
		sshCmd:     `ssh -F none -o 'User=dev' -p 2200 'real.example'`,
		sessionDir: "/remote/sess-dir",
		runCommandFn: func(_ time.Duration, script string, _ io.Reader, _ bool) ([]byte, error) {
			ran = append(ran, script)
			return nil, nil
		},
	}
	b := &sshBackend{provisioner: prov}

	opts := InstanceOptions{ID: "sess-remote-1", Title: "sess", Path: repoRoot, Host: "gpu-box"}
	note := runSessionSetupHook(opts, repoRoot, BackendSSH, b, false)
	assert.Equal(t, "", note)

	// The staging dir is created on the host before the script runs, through
	// the provisioner's own ssh transport.
	require.NotEmpty(t, ran, "expected the remote staging mkdir through the session ssh route")
	assert.Contains(t, ran[0], "mkdir")
	assert.Contains(t, ran[0], "/remote/sess-dir/af-setup")

	env, err := os.ReadFile(filepath.Join(repoRoot, "env.out"))
	require.NoError(t, err)
	s := string(env)
	assert.Contains(t, s, "AF_SETUP_MODE=remote")
	assert.Contains(t, s, "AF_SETUP_DEST=/remote/sess-dir/af-setup")
	assert.Contains(t, s, "AF_SETUP_HOST=gpu-box")
	assert.Contains(t, s, "AF_SSH_DEST=real.example")
	assert.Contains(t, s, "AF_SSH_FULL="+prov.sshCmd)
	// AF_SSH is the command WITHOUT the destination — the form rsync -e and
	// the sh -c ... "$@" command idiom both need.
	assert.Contains(t, s, "AF_SSH="+`ssh -F none -o 'User=dev' -p 2200`)
	assert.NotContains(t, s, "AF_SSH=ssh -F none -o 'User=dev' -p 2200 'real.example'")
}

// A free-form sandbox_ssh exports AF_SSH_FULL alone — even when its last
// token looks like a destination. The move_cmd contract: an operator's own
// quoting cannot be split honestly, so only the composed-for-`backend=ssh`
// transport earns AF_SSH/AF_SSH_DEST.
func TestSetupHookRemoteFreeFormTransport(t *testing.T) {
	repoRoot := initTempGitRepo(t)
	writeSetupCommandsConfig(t, repoRoot, `setup_commands = ["env | grep '^AF_' | sort > env.out"]
`)

	prov := &sandboxProvisioner{
		// Splittable-looking, but operator-composed — af never guesses.
		sshCmd:     `ssh -F none -o 'User=dev' -p 2200 'real.example'`,
		sessionDir: "/remote/sess-dir",
		runCommandFn: func(_ time.Duration, _ string, _ io.Reader, _ bool) ([]byte, error) {
			return nil, nil
		},
	}
	b := &sandboxBackend{provisioner: prov}

	opts := InstanceOptions{ID: "sess-remote-2", Title: "sess", Path: repoRoot}
	note := runSessionSetupHook(opts, repoRoot, BackendSandbox, b, false)
	assert.Equal(t, "", note)

	env, err := os.ReadFile(filepath.Join(repoRoot, "env.out"))
	require.NoError(t, err)
	s := string(env)
	assert.Contains(t, s, "AF_SSH_FULL="+prov.sshCmd)
	assert.NotContains(t, s, "AF_SSH_DEST=")
	assert.NotContains(t, s, "AF_SSH=ssh -F")
}

// An af-composed ssh command whose destination token cannot be isolated also
// degrades to AF_SSH_FULL alone — defensive, since composition always pins
// the destination last.
func TestSetupHookSSHBackendUnsplittableTransport(t *testing.T) {
	repoRoot := initTempGitRepo(t)
	writeSetupCommandsConfig(t, repoRoot, `setup_commands = ["env | grep '^AF_' | sort > env.out"]
`)

	prov := &sandboxProvisioner{
		sshCmd:     `ssh`, // degenerate: no destination token to split
		sessionDir: "/remote/sess-dir",
		runCommandFn: func(_ time.Duration, _ string, _ io.Reader, _ bool) ([]byte, error) {
			return nil, nil
		},
	}
	b := &sshBackend{provisioner: prov}

	opts := InstanceOptions{ID: "sess-remote-3", Title: "sess", Path: repoRoot}
	note := runSessionSetupHook(opts, repoRoot, BackendSSH, b, false)
	assert.Equal(t, "", note)

	env, err := os.ReadFile(filepath.Join(repoRoot, "env.out"))
	require.NoError(t, err)
	s := string(env)
	assert.Contains(t, s, "AF_SSH_FULL=ssh")
	assert.NotContains(t, s, "AF_SSH_DEST=")
	assert.NotContains(t, s, "AF_SSH=ssh")
}

// Commands run sequentially in declaration order, sharing the checkout.
func TestSetupHookSequentialOrder(t *testing.T) {
	repoRoot := initTempGitRepo(t)
	writeSetupCommandsConfig(t, repoRoot, `setup_commands = [
  "echo one > \"$AF_SETUP_DEST/order\"",
  "echo two >> \"$AF_SETUP_DEST/order\"",
  "echo three >> \"$AF_SETUP_DEST/order\"",
]
`)

	opts := InstanceOptions{ID: "sess-order", Title: "sess", Path: repoRoot}
	note := runSessionSetupHook(opts, repoRoot, BackendLocal, &LocalBackend{}, false)
	assert.Equal(t, "", note)

	order, err := os.ReadFile(filepath.Join(localSetupStaging(t, "sess-order"), "payload", "order"))
	require.NoError(t, err)
	assert.Equal(t, "one\ntwo\nthree\n", string(order))
}

// A failing command is recorded, never fatal — the post_worktree_commands
// contract. The note names the entry, the exit status, and the retained
// full-output log; the remaining entries still run.
func TestSetupHookFailureIsRecordedNotFatal(t *testing.T) {
	repoRoot := initTempGitRepo(t)
	writeSetupCommandsConfig(t, repoRoot, `setup_commands = [
  "echo before > \"$AF_SETUP_DEST/ran\"",
  "echo some-failure-output >&2; exit 3",
  "echo after >> \"$AF_SETUP_DEST/ran\"",
]
`)

	opts := InstanceOptions{ID: "sess-fail", Title: "sess", Path: repoRoot}
	note := runSessionSetupHook(opts, repoRoot, BackendLocal, &LocalBackend{}, false)
	require.NotEqual(t, "", note, "a failing entry must produce a session-visible note")
	assert.Contains(t, note, "exit status 3")
	assert.Contains(t, note, "full output:")

	// The note's log path exists and holds the command's output.
	const marker = "full output: "
	idx := strings.Index(note, marker)
	require.NotEqual(t, -1, idx)
	logPath := strings.TrimSpace(note[idx+len(marker):])
	out, err := os.ReadFile(logPath)
	require.NoError(t, err, "the retained hook log must name the command's output")
	assert.Contains(t, string(out), "some-failure-output")

	// A failure does not stop later entries — matching post_worktree_commands.
	ran, err := os.ReadFile(filepath.Join(localSetupStaging(t, "sess-fail"), "payload", "ran"))
	require.NoError(t, err)
	assert.Equal(t, "before\nafter\n", string(ran))
}

// A command that hangs is bounded so create cannot wedge forever; the timeout
// is recorded the same way an exit failure is.
func TestSetupHookCommandTimeout(t *testing.T) {
	prev := setupCommandTimeout
	setupCommandTimeout = 200 * time.Millisecond
	t.Cleanup(func() { setupCommandTimeout = prev })

	repoRoot := initTempGitRepo(t)
	writeSetupCommandsConfig(t, repoRoot, `setup_commands = ["sleep 60"]
`)

	opts := InstanceOptions{ID: "sess-timeout", Title: "sess", Path: repoRoot}
	note := runSessionSetupHook(opts, repoRoot, BackendLocal, &LocalBackend{}, false)
	require.NotEqual(t, "", note)
	assert.Contains(t, note, "timed out")
}

// --here sessions attach the user's own tree — no setup hook, same rule as
// post_worktree_commands skipping external worktrees.
func TestSetupHookSkippedForInPlace(t *testing.T) {
	repoRoot := initTempGitRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	writeSetupCommandsConfig(t, repoRoot, `setup_commands = ["touch `+marker+`"]
`)

	opts := InstanceOptions{ID: "sess-inplace", Title: "sess", Path: repoRoot, InPlace: true}
	note := runSessionSetupHook(opts, repoRoot, BackendLocal, &LocalBackend{}, false)
	assert.Equal(t, "", note)
	_, err := os.Stat(marker)
	assert.True(t, os.IsNotExist(err), "in-place sessions must not run setup_commands")
}

// No configured commands → no work, no staging dir, no note.
func TestSetupHookNoCommands(t *testing.T) {
	repoRoot := initTempGitRepo(t)
	opts := InstanceOptions{ID: "sess-none", Title: "sess", Path: repoRoot}
	note := runSessionSetupHook(opts, repoRoot, BackendLocal, &LocalBackend{}, false)
	assert.Equal(t, "", note)
	_, err := os.Stat(localSetupStaging(t, "sess-none"))
	assert.True(t, os.IsNotExist(err))
}

// A remote provision without an ssh route (docker/hook, or the explicit
// caller seam) still runs the same script — mode remote, no AF_SSH names.
func TestSetupHookRemoteWithoutSSHRoute(t *testing.T) {
	repoRoot := initTempGitRepo(t)
	writeSetupCommandsConfig(t, repoRoot, `setup_commands = ["env | grep '^AF_' | sort > env.out"]
`)

	opts := InstanceOptions{ID: "sess-docker", Title: "sess", Path: repoRoot}
	note := runSessionSetupHook(opts, repoRoot, BackendDocker, &dockerBackend{}, false)
	assert.Equal(t, "", note)

	env, err := os.ReadFile(filepath.Join(repoRoot, "env.out"))
	require.NoError(t, err)
	s := string(env)
	assert.Contains(t, s, "AF_SETUP_MODE=remote")
	assert.NotContains(t, s, "AF_SSH")
	assert.NotContains(t, s, "AF_SETUP_DEST=")
}

// The hook env must honor the session's approved passthrough names — the same
// default-deny boundary post-worktree hooks run under.
func TestSetupHookPassthroughEnvironment(t *testing.T) {
	repoRoot := initTempGitRepo(t)
	writeSetupCommandsConfig(t, repoRoot, `setup_commands = ["echo ${AF_SETUP_TEST_TOKEN:-missing} > \"$AF_SETUP_DEST/token\""]
`)

	t.Setenv("AF_SETUP_TEST_TOKEN", "sekrit")
	opts := InstanceOptions{
		ID: "sess-pass", Title: "sess", Path: repoRoot,
		SessionEnvPassthrough: []string{"AF_SETUP_TEST_TOKEN"},
	}
	note := runSessionSetupHook(opts, repoRoot, BackendLocal, &LocalBackend{}, false)
	assert.Equal(t, "", note)

	got, err := os.ReadFile(filepath.Join(localSetupStaging(t, "sess-pass"), "payload", "token"))
	require.NoError(t, err)
	assert.Equal(t, "sekrit", strings.TrimSpace(string(got)),
		"an approved passthrough name must reach the setup script")
}

// A name that is NOT approved stays filtered — same deny-by-default rule.
func TestSetupHookPassthroughDeniesUnapproved(t *testing.T) {
	repoRoot := initTempGitRepo(t)
	writeSetupCommandsConfig(t, repoRoot, `setup_commands = ["echo ${AF_SETUP_UNAPPROVED_TOKEN:-missing} > \"$AF_SETUP_DEST/token\""]
`)

	t.Setenv("AF_SETUP_UNAPPROVED_TOKEN", "sekrit")
	opts := InstanceOptions{ID: "sess-deny", Title: "sess", Path: repoRoot}
	note := runSessionSetupHook(opts, repoRoot, BackendLocal, &LocalBackend{}, false)
	assert.Equal(t, "", note)

	got, err := os.ReadFile(filepath.Join(localSetupStaging(t, "sess-deny"), "payload", "token"))
	require.NoError(t, err)
	assert.Equal(t, "missing", strings.TrimSpace(string(got)),
		"an unapproved name must not reach the setup script")
}

// --- docker payload transfer ownership + daemon-side payload lifecycle ---

// dockerSetupPayloadStub answers the payload transfer's docker calls: the
// mount-table read gets the resolved staging dir plus a mountinfo built from
// mountPoints, everything else succeeds silently.
func dockerSetupPayloadStub(calls *[][]string, mountPoints ...string) func(context.Context, []string, ...string) ([]byte, error) {
	return func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string(nil), args...))
		if isMountTableExec(args) {
			lines := []string{"/af-setup", mountinfoLine(1, "/", "overlay")}
			for i, mp := range mountPoints {
				lines = append(lines, mountinfoLine(10+i, mp, "ext4"))
			}
			return []byte(strings.Join(lines, "\n") + "\n"), nil
		}
		return nil, nil
	}
}

// The docker transfer must hand the staged payload to the session's own
// user: docker cp lands it root-owned while keeping the daemon's modes, so a
// 0600 file stays 0600 — unreadable by the non-root user a BYO image runs
// its agent-server as, and by the overlay copy it performs.
func TestTransferDockerSetupPayloadChownsToSessionUser(t *testing.T) {
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(dockerSetupPayloadStub(&calls)))
	p := &dockerProvisioner{containerID: "cid123", containerUser: dockerNonRootImageUser}
	assert.Equal(t, "", transferDockerSetupPayload(p, t.TempDir()))

	chown := findExecCall(calls, "--user 0:0", "chown -R")
	require.NotNil(t, chown,
		"the staged payload must be chowned to the session user as root; calls=%v", calls)
	assert.Contains(t, strings.Join(chown, " "),
		"chown -R -h "+dockerNonRootImageUser+" "+shellQuote("/af-setup"),
		"the handoff must act on payload symlinks themselves, never a link's target; call=%v", chown)
}

// A root-default image leaves containerUser unset: the session already runs
// as root, so the root-owned payload is readable as copied — no chown. The
// staging-dir proof still runs for it: a root write is a root write, and the
// check gates the cp, not just the chown.
func TestTransferDockerSetupPayloadRootSessionSkipsChown(t *testing.T) {
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(dockerSetupPayloadStub(&calls)))
	p := &dockerProvisioner{containerID: "cid123"}
	assert.Equal(t, "", transferDockerSetupPayload(p, t.TempDir()))

	assert.NotNil(t, findExecCall(calls, "/proc/self/mountinfo"),
		"the staging-dir proof must run before the first root write, image user or not; calls=%v", calls)
	assert.Nil(t, findExecCall(calls, "chown"),
		"a session running as root needs no ownership handoff; calls=%v", calls)
}

// A mount point beneath the staging dir is pruned from the recursive chown —
// through a host bind there it would re-own the operator's host files, the
// same hazard the session-HOME chown guards (#5424).
func TestTransferDockerSetupPayloadChownPrunesNestedMounts(t *testing.T) {
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(dockerSetupPayloadStub(&calls, "/af-setup/creds")))
	p := &dockerProvisioner{containerID: "cid123", containerUser: dockerNonRootImageUser}
	assert.Equal(t, "", transferDockerSetupPayload(p, t.TempDir()))

	chown := findExecCall(calls, "--user 0:0", "find", "chown")
	require.NotNil(t, chown,
		"a nested mount must switch the handoff to the find -prune form; calls=%v", calls)
	joined := strings.Join(chown, " ")
	assert.Contains(t, joined, "-path "+shellQuote("/af-setup/creds"),
		"the mount point under the staging dir must be pruned; call=%v", chown)
	assert.Contains(t, joined, "-prune -o -exec chown -h "+dockerNonRootImageUser,
		"everything else under the staging dir is still chowned; call=%v", chown)
	assert.NotContains(t, joined, "chown -R",
		"the prune form must not also run a bare recursive chown; call=%v", chown)
}

// A staging dir that itself sits inside a mount is not af's to write into:
// a docker cp there lands root-owned files on the operator's host mount. The
// proof refuses the whole transfer before the first write, and the session
// row gets a note saying why.
func TestTransferDockerSetupPayloadSkipsMountedStagingDir(t *testing.T) {
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(dockerSetupPayloadStub(&calls, "/af-setup")))
	p := &dockerProvisioner{containerID: "cid123", containerUser: dockerNonRootImageUser}
	note := transferDockerSetupPayload(p, t.TempDir())

	require.NotEqual(t, "", note)
	assert.Contains(t, note, "inside the mount")
	assert.False(t, dockerCopyRan(calls),
		"a staging dir inside a host mount must never receive the payload copy; calls=%v", calls)
	assert.Nil(t, findExecCall(calls, "chown"),
		"a staging dir inside a host mount must never be chowned; calls=%v", calls)
}

// dockerCopyRan reports whether the recorded docker calls include the
// payload copy — the first root write into the staging dir the
// staging-dir proof is there to gate.
func dockerCopyRan(calls [][]string) bool {
	for _, call := range calls {
		if len(call) > 0 && call[0] == "cp" {
			return true
		}
	}
	return false
}

// The root writes may act only on exactly the staging dir af created: a
// symlinked staging path resolves to wherever it points, so any resolved
// target other than the mkdir'd path refuses the whole transfer — "/"
// included, as the worst case, not a special case — before the first write.
func TestTransferDockerSetupPayloadRefusesResolvedMismatch(t *testing.T) {
	for _, resolved := range []string{"/", "/elsewhere", "/af-setup/real"} {
		var calls [][]string
		t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string(nil), args...))
			if isMountTableExec(args) {
				return []byte(resolved + "\n" + mountinfoLine(1, "/", "overlay") + "\n"), nil
			}
			return nil, nil
		}))
		p := &dockerProvisioner{containerID: "cid123", containerUser: dockerNonRootImageUser}
		note := transferDockerSetupPayload(p, t.TempDir())
		require.NotEqual(t, "", note, "resolved target %q must refuse the transfer", resolved)
		assert.Contains(t, note, "not the staging dir")
		assert.False(t, dockerCopyRan(calls),
			"resolved target %q must refuse before the payload copy runs", resolved)
		assert.Nil(t, findExecCall(calls, "chown"),
			"a mismatched resolved target must never be chowned; calls=%v", calls)
	}
}

// A failed ownership handoff is a recorded, session-visible note like every
// other transfer failure — never a silent unreadable payload.
func TestTransferDockerSetupPayloadChownFailureIsRecorded(t *testing.T) {
	stub := dockerSetupPayloadStub(&[][]string{})
	t.Cleanup(SetDockerExecForTest(func(ctx context.Context, env []string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "chown") {
			return nil, fmt.Errorf("chown: operation not permitted")
		}
		return stub(ctx, env, args...)
	}))
	p := &dockerProvisioner{containerID: "cid123", containerUser: dockerNonRootImageUser}
	note := transferDockerSetupPayload(p, t.TempDir())
	require.NotEqual(t, "", note)
	assert.Contains(t, note, dockerNonRootImageUser)
}

// A settled Instance.Kill is the permanent-deletion path every backend
// funnels through — local sessions via localAgentServer→teardownTabs,
// off-box ones via remoteAgentServer.Kill (docker reaps through its
// runtimeTeardown closure, never teardownTabs). Either way the daemon-side
// payload's only handle is the record the kill deletes, so a settled kill
// must reclaim it wherever the session ran.
func TestKillDiscardsLocalSetupPayloadOnSettledKill(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	staging := localSetupStaging(t, "sess-kill")
	require.NoError(t, os.MkdirAll(filepath.Join(staging, "payload"), 0755))

	i := &Instance{ID: "sess-kill", Title: "sess", agentSrv: &stubAgentServer{}}
	require.NoError(t, i.Kill())
	_, err := os.Stat(staging)
	assert.True(t, os.IsNotExist(err),
		"a settled kill must remove the daemon-side setup payload")
}

// An unknown teardown outcome keeps the record as the retry's only handle —
// and the payload with it, for the attempt that eventually settles.
func TestKillKeepsLocalSetupPayloadOnUnknownOutcome(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	staging := localSetupStaging(t, "sess-kill-unknown")
	require.NoError(t, os.MkdirAll(filepath.Join(staging, "payload"), 0755))

	srv := &stubAgentServer{killErr: fmt.Errorf("%w: reap timed out", ErrWorkspaceStateUnknown)}
	i := &Instance{ID: "sess-kill-unknown", Title: "sess", agentSrv: srv}
	require.ErrorIs(t, i.Kill(), ErrWorkspaceStateUnknown)
	_, err := os.Stat(staging)
	assert.NoError(t, err, "an unsettled kill must keep the payload for the retry")
}

// Archive keeps the row and its payload: ArchiveSandbox drives the
// AgentServer's Kill directly for push-then-teardown — never the
// record-deleting Instance.Kill — so the staging dir survives for the
// restore's rebuild.
func TestArchiveSandboxKeepsLocalSetupPayload(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	staging := localSetupStaging(t, "sess-arch")
	require.NoError(t, os.MkdirAll(filepath.Join(staging, "payload"), 0755))

	as := &stubAgentServer{branch: "root/s"}
	i := newStubbedSandboxInstance(as)
	i.ID = "sess-arch"
	_, err := i.ArchiveSandbox()
	require.NoError(t, err)
	assert.Equal(t, 1, as.killCalls)
	_, statErr := os.Stat(staging)
	assert.NoError(t, statErr, "archive must keep the payload for the restore's rebuild")
}

// A workspace-preserving stop is NOT a deletion: the record and the tree
// stay for a reattach, so teardownTabs — reached directly rather than
// through Instance.Kill — must leave the payload alone even though its mode
// is teardownKill{keepWorkspace: true}.
func TestWorkspacePreservingStopKeepsLocalSetupPayload(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	staging := localSetupStaging(t, "sess-preserve")
	require.NoError(t, os.MkdirAll(filepath.Join(staging, "payload"), 0755))

	inst := &Instance{ID: "sess-preserve", Title: "sess", backend: &LocalBackend{}}
	srv := &localAgentServer{inst: inst}
	require.NoError(t, srv.StopPreservingWorkspace())
	_, err := os.Stat(staging)
	assert.NoError(t, err,
		"a workspace-preserving stop must keep the payload for the preserved tree")
}

// --- payload-dir id guard (#6674 review) ---

// The only safe session id for payload arithmetic is a single path element
// other than "." and "..": every other spelling resolves out of af-setup —
// "" and "." onto the shared root itself, ".." and "a/.." into the AF home
// or above it — and the deletion path must never compute such a name.
func TestLocalSetupPayloadSessionDirGuard(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	home, err := config.GetConfigDir()
	require.NoError(t, err)

	for _, id := range []string{"", ".", "..", "/", "a/..", "a/b", filepath.Join("a", "..", "..", "x"), "/abs/path", filepath.Join(home, "other"), "\x00", "a\x00b"} {
		dir, ok := localSetupPayloadSessionDir(id)
		assert.False(t, ok, "id %q must not name a payload dir (got %q)", id, dir)
		assert.Empty(t, dir)
	}
	// A normal minted id is a direct child and nothing else.
	id := "9d9d9d9d-1111-4222-8333-444455556666"
	dir, ok := localSetupPayloadSessionDir(id)
	require.True(t, ok)
	assert.Equal(t, filepath.Join(home, "af-setup", id), dir)
	assert.Equal(t, "af-setup", filepath.Base(filepath.Dir(dir)))
}

// A refused id deletes NOTHING — the shared af-setup root, the AF home and
// their other sessions' payloads all survive. This is the corrupted-record
// path: persisted ids load unvalidated, so the guard lives at the removal.
func TestDiscardLocalSetupPayloadRefusesMalformedIDs(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	home, err := config.GetConfigDir()
	require.NoError(t, err)
	// A sibling session's payload under the shared root — the casualty a
	// naive join would take with it.
	sibling := filepath.Join(home, "af-setup", "sess-innocent", "payload")
	require.NoError(t, os.MkdirAll(sibling, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(sibling, "keep"), []byte("x"), 0644))

	for _, id := range []string{"", ".", "..", "/", "a/..", "a/b", filepath.Join("a", "..", ".."), "/abs/path", "\x00", "a\x00b"} {
		i := &Instance{ID: id, Title: "sess"}
		i.discardLocalSetupPayload()
		_, statErr := os.Stat(filepath.Join(sibling, "keep"))
		assert.NoError(t, statErr, "id %q must not remove the shared af-setup root", id)
		_, statErr = os.Stat(home)
		assert.NoError(t, statErr, "id %q must not remove the AF home", id)
	}
	// The shared root itself still stands.
	_, statErr := os.Stat(filepath.Join(home, "af-setup"))
	assert.NoError(t, statErr)
}

// A symlinked af-setup ROOT is refused the same way the sweep refuses it:
// the per-session discard must not delete through the link either, or it
// would empty the shared target one entry at a time.
func TestDiscardLocalSetupPayloadRefusesSymlinkedRoot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	home, err := config.GetConfigDir()
	require.NoError(t, err)
	target := t.TempDir()
	victim := filepath.Join(target, "sess-mine", "payload")
	require.NoError(t, os.MkdirAll(victim, 0755))
	require.NoError(t, os.Symlink(target, filepath.Join(home, "af-setup")))

	i := &Instance{ID: "sess-mine", Title: "sess"}
	i.discardLocalSetupPayload()
	_, statErr := os.Stat(victim)
	assert.NoError(t, statErr, "a linked af-setup root must be refused — the linked target's tree is not af's to delete")
}

// A failed create that leaves no record reclaims its payload at the exit:
// the hook ran inside the factory, so every error return below it strands
// the staged tree with no owner. The Instance return disarms the discard.
func TestNewInstanceFailedCreateReclaimsSetupPayload(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := initTempGitRepo(t)
	var staged string
	restore := SetProvisionResultFactoryForTest(func(opts InstanceOptions, absPath string, kind BackendKind) (ProvisionResult, error) {
		dir, ok := localSetupPayloadSessionDir(opts.ID)
		require.True(t, ok)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "payload"), 0755))
		staged = dir
		return ProvisionResult{
			Backend:  &dockerBackend{},
			Endpoint: &AgentServerEndpoint{URL: "://invalid", Token: "tok"},
		}, nil
	})
	defer restore()

	_, err := NewInstance(InstanceOptions{Title: "s", Path: repoRoot, Program: "claude"})
	require.Error(t, err)
	require.NotEmpty(t, staged)
	_, statErr := os.Stat(staged)
	assert.True(t, os.IsNotExist(statErr),
		"a create that fails after the hook ran must reclaim the staged payload it can no longer own")
}

// A successful create keeps its payload — the Instance return disarms the
// failed-create discard.
func TestNewInstanceKeepsSetupPayloadOnSuccess(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := initTempGitRepo(t)
	var staged string
	restore := SetProvisionResultFactoryForTest(func(opts InstanceOptions, absPath string, kind BackendKind) (ProvisionResult, error) {
		dir, ok := localSetupPayloadSessionDir(opts.ID)
		require.True(t, ok)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "payload"), 0755))
		staged = dir
		return ProvisionResult{Backend: &LocalBackend{}}, nil
	})
	defer restore()

	_, err := NewInstance(InstanceOptions{Title: "s", Path: repoRoot, Program: "claude"})
	require.NoError(t, err)
	_, statErr := os.Stat(staged)
	assert.NoError(t, statErr, "a live session keeps its staged payload for rebuilds")
}

// A valid id removes only its own directory — the sibling payload under the
// same root is untouched.
func TestDiscardLocalSetupPayloadRemovesOnlyItsOwn(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	home, err := config.GetConfigDir()
	require.NoError(t, err)
	mine := filepath.Join(home, "af-setup", "sess-mine", "payload")
	sibling := filepath.Join(home, "af-setup", "sess-innocent", "payload")
	require.NoError(t, os.MkdirAll(mine, 0755))
	require.NoError(t, os.MkdirAll(sibling, 0755))

	i := &Instance{ID: "sess-mine", Title: "sess"}
	i.discardLocalSetupPayload()
	_, statErr := os.Stat(filepath.Join(home, "af-setup", "sess-mine"))
	assert.True(t, os.IsNotExist(statErr), "the session's own staging dir must be removed")
	_, statErr = os.Stat(sibling)
	assert.NoError(t, statErr, "a sibling session's payload must survive")
}

// --- hook ordering (#6674 review) ---

// The adopt gate is the hook's own skip, not a call-site convention: an
// adopted runtime kept its prior state and may already have launched its
// agent, and a root ownership handoff must never run over a tree an
// unprivileged principal could have written. adopted=true runs nothing —
// no commands, no staging dir — even with setup_commands configured.
func TestSetupHookSkippedForAdoptedRuntime(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := initTempGitRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	writeSetupCommandsConfig(t, repoRoot, `setup_commands = ["touch `+marker+`"]
`)

	opts := InstanceOptions{ID: "sess-adopt", Title: "sess", Path: repoRoot}
	note := runSessionSetupHook(opts, repoRoot, BackendLocal, &LocalBackend{}, true)
	assert.Equal(t, "", note)
	_, err := os.Stat(marker)
	assert.True(t, os.IsNotExist(err), "an adopted runtime must not run setup_commands")
	_, err = os.Stat(localSetupStaging(t, "sess-adopt"))
	assert.True(t, os.IsNotExist(err), "an adopted runtime must not even stage a payload")
}

// The restore caller must reach the adopted skip too: reprovisionRemoteOpts
// passes adopt != nil, and a regression there (passing false) replays the
// hook over a workspace whose agent may already have run — and whose staged
// tree an unprivileged principal could already have written. The prior
// create's failure note must also survive the adopted bind: an adopt
// provisions nothing new, so its older note is still the truth.
func TestReprovisionAdoptSkipsSetupHookAndKeepsPriorNote(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := initTempGitRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	writeSetupCommandsConfig(t, repoRoot, `setup_commands = ["touch `+marker+`"]
`)

	var calls [][]string
	t.Cleanup(SetDockerExecForTest(dockerSetupPayloadStub(&calls)))
	freshBackend := &dockerBackend{provisioner: &dockerProvisioner{
		containerID: "fresh", containerUser: dockerNonRootImageUser}}
	rt := fakeRuntime{res: ProvisionResult{
		Backend:  freshBackend,
		Endpoint: &AgentServerEndpoint{URL: "http://127.0.0.1:9", Token: "tok"},
		Teardown: func() error { return nil },
	}}
	restoreRuntime := SetRuntimeForTest(BackendDocker, func() Runtime { return rt })
	defer restoreRuntime()

	i := &Instance{
		ID:              "sess-adopted",
		Title:           "s",
		Path:            repoRoot,
		Branch:          "root/s",
		backend:         newInertSandboxBackend("docker"),
		setupHookFailed: "entry 1 failed",
	}
	require.NoError(t, i.reprovisionRemoteOpts(
		&AdoptSpec{SessionDir: "/remote/af-sessions/sess-adopted", Branch: "root/s"}, false))

	_, err := os.Stat(marker)
	assert.True(t, os.IsNotExist(err),
		"an adopted restore must not replay setup_commands")
	_, err = os.Stat(localSetupStaging(t, "sess-adopted"))
	assert.True(t, os.IsNotExist(err),
		"an adopted restore must not even stage a payload")
	assert.Empty(t, calls,
		"an adopted restore must not run the docker payload transfer either; calls=%v", calls)
	assert.Equal(t, "entry 1 failed", i.SetupHookFailed(),
		"an adopted restore keeps the note the original create recorded")
}

// The hook runs inside provision and provision only — never after an agent
// launch, where the session's own principal may already have written the
// staged tree a root chown then runs over. The whole production call-site
// set is pinned here: both approved sites sit inside provision paths that
// precede every Launch their caller drives, so a new caller (a launch-time
// convenience, a resume hook, a late adopt) fails this test until someone
// re-justifies the ordering knowingly.
func TestSetupHookCallSitesPinnedToProvision(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	sites := map[string]int{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		require.NoError(t, err)
		for _, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, "runSessionSetupHook(") &&
				!strings.Contains(line, "func runSessionSetupHook(") {
				sites[name]++
			}
		}
	}
	assert.Equal(t, map[string]int{"instance_factory.go": 1, "archive_sandbox.go": 1}, sites,
		"runSessionSetupHook may only be invoked inside provision — both sites precede every Launch their caller drives; a post-launch caller must re-justify the ordering, not slip in")
}
