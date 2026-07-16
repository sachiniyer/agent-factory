package session

import (
	"bytes"
	"errors"
	"fmt"
	stdlog "log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func TestMain(m *testing.M) {
	// #837: fail the package loudly if any test touches the real config.json.
	verifyRealConfig := testguard.ConfigTripwire()
	// #1056: fail loudly if a test leaks an af_ session onto the ambient tmux
	// server, and default the whole package into a sandboxed
	// AGENT_FACTORY_HOME so stray config/state/log writes land in a temp dir
	// instead of the developer's real one. Sandbox AFTER the tripwires
	// snapshot the real environment, BEFORE logging resolves its file path.
	verifyTmux := testguard.TmuxTripwire()
	restoreHome := testguard.SandboxHome()
	// #1122: default the whole package onto a private tmux server so a test
	// that forgets IsolateTmux can never create or sweep sessions on the
	// developer's real server.
	restoreTmux := testguard.SandboxTmux()
	log.Initialize(false)
	code := m.Run()
	log.Close()
	restoreTmux()
	restoreHome()
	if err := verifyRealConfig(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	if err := verifyTmux(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

// --- Backend interface compliance ---

func TestLocalBackendType(t *testing.T) {
	b := &LocalBackend{}
	assert.Equal(t, "local", b.Type())
}

func TestHookBackendType(t *testing.T) {
	b := &HookBackend{}
	assert.Equal(t, "remote", b.Type())
}

// TestBackendCapabilities pins each backend's capability descriptor (#1592
// Phase 1). This is the interface-compliance / parity contract: the daemon and
// UI branch on these bits instead of Type()=="remote", so a regression that
// silently flips a capability (e.g. a new backend forgetting Archive) is caught
// here rather than in a lifecycle gate.
//
// The off-box runtimes (docker/ssh/hook) reach parity on every capability EXCEPT
// TabManagement, which is false (#1874): the agent-server's tab surface is data
// plane only, so every Add*Tab path still needs a daemon-side worktree they do
// not have. See TestSandboxBackendsDoNotAdvertiseTabManagement, which pairs this
// bit with proof the spawn paths really do reject them.
func TestBackendCapabilities(t *testing.T) {
	localCaps := Capabilities{
		Workspace:        WorkspaceLocalWorktree,
		Archive:          true,
		Recover:          true,
		TabManagement:    true,
		TerminalTab:      true,
		InteractiveInput: true,
	}

	// Every off-box runtime shares one descriptor: a remote workspace with
	// Archive/Recover via push/pull-branch (no ErrRecoverUnsupported, no locality
	// special-case) and no user-managed tabs. Shared so a new sandbox runtime
	// cannot quietly drift from its siblings.
	sandboxCaps := Capabilities{
		Workspace:        WorkspaceRemote,
		Archive:          true,
		Recover:          true,
		TabManagement:    false,
		TerminalTab:      true,
		InteractiveInput: true,
	}

	tests := []struct {
		name string
		b    Backend
		want Capabilities
	}{
		{
			name: "local worktree runtime — full parity",
			b:    &LocalBackend{},
			want: localCaps,
		},
		{
			name: "fake backend mirrors local parity",
			b:    NewFakeBackend(),
			want: localCaps,
		},
		{
			// #1592 Phase 4 PR7 migrated the remote-hook backend to
			// provision-and-expose, reaching the same descriptor as docker/ssh: its
			// old terminal_cmd-gated TerminalTab and ErrRecoverUnsupported are gone.
			name: "remote hook runtime — remote parity, no tab management",
			b:    &HookBackend{},
			want: sandboxCaps,
		},
		{
			name: "docker sandbox runtime — remote parity, no tab management",
			b:    &dockerBackend{},
			want: sandboxCaps,
		},
		{
			name: "ssh sandbox runtime — remote parity, no tab management",
			b:    &sshBackend{},
			want: sandboxCaps,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.b.Capabilities())
		})
	}
}

// complianceBackends returns one instance of every Backend implementation a
// registered runtime can produce, keyed by its BackendKind. It is the single
// list the parity matrix runs against; TestBackendComplianceCoversRegistry
// asserts it stays in lockstep with runtimeRegistry, so a NEW backend added to
// the registry cannot dodge the matrix by being omitted here.
func complianceBackends() map[BackendKind]Backend {
	return map[BackendKind]Backend{
		BackendLocal:  &LocalBackend{},
		BackendHook:   &HookBackend{},
		BackendDocker: &dockerBackend{},
		BackendSSH:    &sshBackend{},
	}
}

// TestBackendComplianceCoversRegistry guards the parity matrix's completeness:
// every runtime the registry can resolve MUST have a compliance backend in the
// matrix, and vice versa. Without this, a future backend registered in
// runtimeRegistry but never added to complianceBackends would silently escape
// the "every backend implements every capability" assertion below.
func TestBackendComplianceCoversRegistry(t *testing.T) {
	compliance := complianceBackends()
	for kind := range runtimeRegistry {
		if _, ok := compliance[kind]; !ok {
			t.Errorf("backend %q is registered in runtimeRegistry but missing from the compliance matrix (complianceBackends); add it so it is held to the parity contract", kind)
		}
	}
	for kind := range compliance {
		if _, ok := runtimeRegistry[kind]; !ok {
			t.Errorf("backend %q is in the compliance matrix but not registered in runtimeRegistry (stale entry?)", kind)
		}
	}
}

// TestBackendCapabilityParity is the epic end-state assertion (#1592 Phase 4,
// §5.2): every backend implements every optional capability — no backend gates an
// operation off behind a "not supported" descriptor bit, and none returns an
// ErrRecoverUnsupported/ErrArchiveUnsupported sentinel (both are DELETED; a
// reference would fail to compile). The old locality special-case — a remote
// backend advertising Archive/Recover=false and returning
// ErrRecoverUnsupported — is gone. Attach/preview/archive/restore all route
// through the uniform agent-server + push/pull-branch contract, so this table
// asserts those bits are TRUE for every registered backend. A regression that
// flips any of them false (re-introducing a locality gap) fails here.
//
// TabManagement is NOT in this table (#1874). It is the one capability the epic
// has not actually delivered off-box: the agent-server's tab surface is data
// plane only (Subscribe/Input/Resize address an EXISTING tab), so there is no
// create/close tab RPC and every Add*Tab path still requires a daemon-side git
// worktree the sandbox runtimes do not have. It sat here as an ASPIRATION
// asserted as fact — the table forced the bit true, the bit made clients offer
// tab affordances, and every one of them failed. The honest per-runtime split is
// pinned by TestBackendCapabilities and by
// TestSandboxBackendsDoNotAdvertiseTabManagement, which pairs each bit with proof
// the spawn paths agree with it. When tab creation routes through the
// agent-server, TabManagement returns to this table and those tests flip in the
// same change.
//
// Attach is NOT in this table either: #1860 deleted the bit. It was true for every
// backend and read by no dispatch, because attach is not gated per-backend at
// all — the client dials the daemon's WS PTY stream for every runtime. A
// capability that cannot be false has nothing to assert parity over.
//
// This runs everywhere (host, no docker/ssh needed): it asserts the descriptor
// each backend SELF-REPORTS. The availability-gated docker/ssh round-trips
// (make backend-docker-roundtrip / backend-ssh-roundtrip) additionally prove the
// REAL backends SERVICE the same matrix over the wire — see
// integration/backend_capability_matrix_test.go.
func TestBackendCapabilityParity(t *testing.T) {
	// The optional capabilities every backend must service. Workspace is NOT
	// here — it is a locality descriptor (local worktree vs off-box), not an
	// operation, and the client dispatches attach/preview on it. Parity is about
	// the OPERATIONS: no backend may report it cannot do one.
	optional := []struct {
		name string
		get  func(Capabilities) bool
	}{
		{"Archive", func(c Capabilities) bool { return c.Archive }},
		{"Recover", func(c Capabilities) bool { return c.Recover }},
		{"TerminalTab", func(c Capabilities) bool { return c.TerminalTab }},
		{"InteractiveInput", func(c Capabilities) bool { return c.InteractiveInput }},
	}

	for kind, b := range complianceBackends() {
		t.Run(string(kind), func(t *testing.T) {
			caps := b.Capabilities()
			for _, oc := range optional {
				assert.Truef(t, oc.get(caps),
					"backend %q must service capability %s at full parity (#1592 Phase 4 end-state: no locality special-case, no unsupported sentinel)",
					kind, oc.name)
			}
		})
	}
}

// TestLocalBackendServicesTabManagement keeps the local runtime's TabManagement
// bit pinned now that it is out of the parity table above. Without this, a
// regression flipping the LOCAL bit false — which WOULD be a real capability
// loss, unlike the sandbox runtimes' honest false — would fail no test.
func TestLocalBackendServicesTabManagement(t *testing.T) {
	for kind, b := range complianceBackends() {
		if b.Capabilities().Workspace != WorkspaceLocalWorktree {
			continue
		}
		assert.Truef(t, b.Capabilities().TabManagement,
			"backend %q has a local worktree and must service TabManagement", kind)
	}
}

// TestLocalBackendKillBestEffort_TmuxFails is a regression test for issue
// #478. When the tmux teardown fails, Kill must still clear in-memory state
// and return nil so the caller can finish removing the session from the
// persisted instances.json. The failure is surfaced as a WarningLog entry
// (including the instance title) for diagnosis.
func TestLocalBackendKillBestEffort_TmuxFails(t *testing.T) {
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			// has-session reports the session still present, so the failed
			// kill-session is a GENUINE teardown failure rather than the
			// idempotent already-gone no-op (#967). Everything else fails.
			if strings.Contains(c.String(), "has-session") {
				return nil
			}
			return errors.New("kill failed")
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) {
			return nil, nil
		},
	}
	ts := tmux.NewTmuxSessionWithDeps("best-effort-tmux", "bash", nil, cmdExec)
	inst := &Instance{
		Title:   "best-effort-tmux",
		backend: &LocalBackend{},
		started: true,
		Tabs:    []*Tab{newAgentTab(ts)},
	}

	buf := captureWarningLog(t)

	require.NoError(t, inst.Kill(), "tmux cleanup failure must not block deletion")
	assert.False(t, inst.Started(), "started flag should be cleared")
	assert.Nil(t, inst.tmuxLocked(), "tmux pointer should be cleared so a retry is a clean no-op")

	logged := buf.String()
	assert.Contains(t, logged, "best-effort-tmux", "warning must include instance title for correlation in agent-factory.log")
	assert.Contains(t, logged, "tmux cleanup for tab")

	require.NoError(t, inst.Kill(), "second kill on a cleared instance must be a no-op")
}

// TestLocalBackendKillBestEffort_WorktreeFails covers the issue #478
// guarantee: when worktree cleanup genuinely fails, Kill logs a warning and
// returns nil so the caller can remove the row from the sidebar and the
// persisted record. The original #478 scenario (path exists but is no longer
// a working tree) now self-heals via the #802 ownership check — see
// TestLocalBackendKill_RecoversStaleWorktreeDir — so this test provokes a
// failure that still surfaces: the stored repo path is not a git repo, every
// git command fails, and `git worktree list` being unreadable means Cleanup
// cannot establish ownership and must NOT delete the directory.
func TestLocalBackendKillBestEffort_WorktreeFails(t *testing.T) {
	notARepo := filepath.Join(t.TempDir(), "not-a-repo")
	require.NoError(t, os.MkdirAll(notARepo, 0755))

	stalePath := filepath.Join(t.TempDir(), "stale-worktree")
	require.NoError(t, os.MkdirAll(stalePath, 0755))

	gw, err := git.NewGitWorktreeFromStorage(notARepo, stalePath, "issue-478", "issue-478-branch", "", false, false)
	require.NoError(t, err)

	inst := &Instance{
		Title:       "issue-478",
		backend:     &LocalBackend{},
		started:     true,
		gitWorktree: gw,
	}

	buf := captureWarningLog(t)

	require.NoError(t, inst.Kill(), "worktree cleanup failure must not block deletion")
	assert.False(t, inst.Started())
	assert.Nil(t, inst.gitWorktree, "git worktree pointer should be cleared")

	logged := buf.String()
	assert.Contains(t, logged, "issue-478", "warning must include instance title")
	assert.Contains(t, logged, "git worktree cleanup failed")
	assert.Contains(t, logged, "not a git repository", "warning should preserve the underlying git error so users can diagnose")

	// Safety: with ownership unknown (worktree list unreadable), the
	// directory must be left alone.
	_, statErr := os.Stat(stalePath)
	assert.NoError(t, statErr, "Cleanup must not delete a directory whose git ownership it cannot establish")
}

// TestLocalBackendKill_RecoversStaleWorktreeDir pins the #802 behavior change
// to the original #478 scenario: the stored path exists on disk but git does
// not register it as a worktree (`worktree remove` fails, `worktree list`
// omits it). Instead of surfacing "is not a working tree" and leaking the
// directory, Kill now removes it and completes cleanly.
func TestLocalBackendKill_RecoversStaleWorktreeDir(t *testing.T) {
	repoRoot := initTempGitRepo(t)

	stalePath := filepath.Join(t.TempDir(), "stale-worktree")
	require.NoError(t, os.MkdirAll(stalePath, 0755))

	gw, err := git.NewGitWorktreeFromStorage(repoRoot, stalePath, "stale-dir", "stale-dir-branch", "", false, false)
	require.NoError(t, err)

	inst := &Instance{
		Title:       "stale-dir",
		backend:     &LocalBackend{},
		started:     true,
		gitWorktree: gw,
	}

	buf := captureWarningLog(t)

	require.NoError(t, inst.Kill())
	assert.Nil(t, inst.gitWorktree)

	_, statErr := os.Stat(stalePath)
	assert.True(t, os.IsNotExist(statErr),
		"Kill must remove a leftover directory git no longer registers as a worktree (#802)")
	assert.NotContains(t, buf.String(), "git worktree cleanup failed",
		"recovered cleanup should not warn")
}

// TestLocalBackendKillBestEffort_BothFail covers the multi-component failure
// case: both tmux and worktree cleanup blow up, and Kill should still return
// nil with a warning per component.
func TestLocalBackendKillBestEffort_BothFail(t *testing.T) {
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			// has-session reports present so the failed kill-session is a
			// genuine teardown failure, not the #967 idempotent no-op.
			if strings.Contains(c.String(), "has-session") {
				return nil
			}
			return errors.New("kill failed")
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) {
			return nil, nil
		},
	}
	ts := tmux.NewTmuxSessionWithDeps("both-fail", "bash", nil, cmdExec)

	// Non-repo path: every git command fails, so the worktree cleanup error
	// surfaces (ownership unknown — see TestLocalBackendKillBestEffort_WorktreeFails).
	notARepo := filepath.Join(t.TempDir(), "not-a-repo")
	require.NoError(t, os.MkdirAll(notARepo, 0755))
	stalePath := filepath.Join(t.TempDir(), "stale")
	require.NoError(t, os.MkdirAll(stalePath, 0755))
	gw, err := git.NewGitWorktreeFromStorage(notARepo, stalePath, "both-fail", "both-fail-branch", "", false, false)
	require.NoError(t, err)

	inst := &Instance{
		Title:       "both-fail",
		backend:     &LocalBackend{},
		started:     true,
		Tabs:        []*Tab{newAgentTab(ts)},
		gitWorktree: gw,
	}

	buf := captureWarningLog(t)

	require.NoError(t, inst.Kill())
	assert.Nil(t, inst.tmuxLocked())
	assert.Nil(t, inst.gitWorktree)

	logged := buf.String()
	assert.Contains(t, logged, "tmux cleanup for tab")
	assert.Contains(t, logged, "git worktree cleanup failed")
	assert.Equal(t, 2, strings.Count(logged, `kill "both-fail":`), "title should appear in both component warnings")
}

// captureWarningLog redirects log.WarningLog to a buffer for the duration of
// the test and returns the buffer. Restoration happens via t.Cleanup.
func captureWarningLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := log.WarningLog
	log.WarningLog = stdlog.New(&buf, "WARNING: ", 0)
	t.Cleanup(func() { log.WarningLog = orig })
	return &buf
}

// initTempGitRepo initializes an empty git repo in a temp directory and
// returns its absolute path. Used by best-effort Kill tests that need a
// real repo path for git worktree commands to dispatch against.
func initTempGitRepo(t *testing.T) string {
	t.Helper()
	repoRoot := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(repoRoot, 0755))
	cmd := exec.Command("git", "init")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return repoRoot
}

// --- Instance workspace capability ---

// TestInstanceCapabilitiesWorkspace pins Instance.Capabilities().Workspace,
// including the nil-backend contract (a backend-less instance reads as local
// full parity, not the zero value) that the deleted IsRemote() helper used to
// nil-guard (#1592 Phase 1 PR3).
func TestInstanceCapabilitiesWorkspace(t *testing.T) {
	t.Run("local backend", func(t *testing.T) {
		i := &Instance{backend: &LocalBackend{}}
		assert.Equal(t, WorkspaceLocalWorktree, i.Capabilities().Workspace)
	})
	t.Run("hook backend", func(t *testing.T) {
		i := &Instance{backend: &HookBackend{}}
		assert.Equal(t, WorkspaceRemote, i.Capabilities().Workspace)
	})
	t.Run("nil backend reports local parity", func(t *testing.T) {
		i := &Instance{}
		assert.Equal(t, (&LocalBackend{}).Capabilities(), i.Capabilities())
	})
}

// writeScript writes an executable shell script to the given path. Shared by
// the local-restore terminal tests.
func writeScript(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	err := os.WriteFile(path, []byte("#!/bin/sh\n"+content), 0755)
	require.NoError(t, err)
	return path
}
