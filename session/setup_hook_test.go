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
	note := runSessionSetupHook(opts, repoRoot, BackendLocal, &LocalBackend{})
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
	note := runSessionSetupHook(opts, repoRoot, BackendSSH, b)
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
	note := runSessionSetupHook(opts, repoRoot, BackendSandbox, b)
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
	note := runSessionSetupHook(opts, repoRoot, BackendSSH, b)
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
	note := runSessionSetupHook(opts, repoRoot, BackendLocal, &LocalBackend{})
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
	note := runSessionSetupHook(opts, repoRoot, BackendLocal, &LocalBackend{})
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
	note := runSessionSetupHook(opts, repoRoot, BackendLocal, &LocalBackend{})
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
	note := runSessionSetupHook(opts, repoRoot, BackendLocal, &LocalBackend{})
	assert.Equal(t, "", note)
	_, err := os.Stat(marker)
	assert.True(t, os.IsNotExist(err), "in-place sessions must not run setup_commands")
}

// No configured commands → no work, no staging dir, no note.
func TestSetupHookNoCommands(t *testing.T) {
	repoRoot := initTempGitRepo(t)
	opts := InstanceOptions{ID: "sess-none", Title: "sess", Path: repoRoot}
	note := runSessionSetupHook(opts, repoRoot, BackendLocal, &LocalBackend{})
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
	note := runSessionSetupHook(opts, repoRoot, BackendDocker, &dockerBackend{})
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
	note := runSessionSetupHook(opts, repoRoot, BackendLocal, &LocalBackend{})
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
	note := runSessionSetupHook(opts, repoRoot, BackendLocal, &LocalBackend{})
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
		"chown -R "+dockerNonRootImageUser+" "+shellQuote("/af-setup"))
}

// A root-default image leaves containerUser unset: the session already runs
// as root, so the root-owned payload is readable as copied — no chown, and
// no mount-table read either.
func TestTransferDockerSetupPayloadRootSessionSkipsChown(t *testing.T) {
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(dockerSetupPayloadStub(&calls)))
	p := &dockerProvisioner{containerID: "cid123"}
	assert.Equal(t, "", transferDockerSetupPayload(p, t.TempDir()))

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

// A staging dir that itself sits inside a mount is not af's to re-own: the
// chown is skipped outright and the session row gets a note saying why.
func TestTransferDockerSetupPayloadSkipsMountedStagingDir(t *testing.T) {
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(dockerSetupPayloadStub(&calls, "/af-setup")))
	p := &dockerProvisioner{containerID: "cid123", containerUser: dockerNonRootImageUser}
	note := transferDockerSetupPayload(p, t.TempDir())

	require.NotEqual(t, "", note)
	assert.Contains(t, note, "inside the mount")
	assert.Nil(t, findExecCall(calls, "chown"),
		"a staging dir inside a host mount must never be chowned; calls=%v", calls)
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
