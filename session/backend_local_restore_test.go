package session

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func TestLocalBackendPrepareAgentSwapRejectsMissingExecutableBeforeRuntime(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	cfg := config.DefaultConfig()
	if cfg.ProgramOverrides == nil {
		cfg.ProgramOverrides = make(map[string]string)
	}
	missing := filepath.Join(t.TempDir(), "missing-gemini")
	cfg.ProgramOverrides[tmux.ProgramGemini] = missing
	require.NoError(t, config.SaveConfig(cfg))

	repoRoot := initTempGitRepo(t)
	gw, gwErr := git.NewGitWorktreeFromStorage(
		repoRoot, repoRoot, "handoff-preflight", "handoff-preflight", "", true, false,
	)
	require.NoError(t, gwErr)
	inst := handoffTestInstance(t, tmux.ProgramClaude)
	inst.Path = repoRoot
	inst.gitWorktree = gw
	plan, err := (&LocalBackend{}).PrepareAgentSwap(inst, tmux.ProgramGemini)
	require.Error(t, err)
	require.Contains(t, err.Error(), missing)
	require.Empty(t, plan.program, "a rejected preflight must not produce an executable swap plan")
	require.Equal(t, tmux.ProgramClaude, inst.AgentProgram(), "preflight must not rewrite the live record")
}

func TestLocalBackendPrepareAgentSwapSnapshotsCommandSpecificCodexHome(t *testing.T) {
	afHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)
	t.Setenv("CODEX_HOME", t.TempDir()) // deliberately not the launched store

	binDir := t.TempDir()
	codexBin := filepath.Join(binDir, "codex")
	require.NoError(t, os.WriteFile(codexBin, []byte("#!/bin/sh\n"), 0o755))
	launchedHome := filepath.Join(t.TempDir(), "command-codex-home")
	cfg := config.DefaultConfig()
	if cfg.ProgramOverrides == nil {
		cfg.ProgramOverrides = make(map[string]string)
	}
	cfg.ProgramOverrides[tmux.ProgramCodex] = "env CODEX_HOME=" + launchedHome + " " + codexBin
	require.NoError(t, config.SaveConfig(cfg))

	repoRoot := initTempGitRepo(t)
	gw, err := git.NewGitWorktreeFromStorage(
		repoRoot, repoRoot, "handoff-capture", "handoff-capture", "", true, false,
	)
	require.NoError(t, err)
	inst := handoffTestInstance(t, tmux.ProgramClaude)
	inst.Path = repoRoot
	inst.gitWorktree = gw
	inst.SetBackend(&LocalBackend{})

	plan, err := (&LocalBackend{}).PrepareAgentSwap(inst, tmux.ProgramCodex)
	require.NoError(t, err)
	require.Equal(t, launchedHome, plan.conversationCapture.codexHome,
		"handoff capture must watch the incoming command's store, not daemon CODEX_HOME")
}

func TestLocalBackendSwapAgentResetsBrokerCapture(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	ptyFactory := &recordingPtyFactory{t: t}
	killed := false
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			joined := strings.Join(c.Args, " ")
			switch {
			case strings.Contains(joined, "kill-session"):
				killed = true
				return nil
			case strings.Contains(joined, "has-session"):
				if killed && len(ptyFactory.cmds) == 0 {
					return errors.New("session absent after close")
				}
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if strings.Contains(strings.Join(c.Args, " "), "display-message") {
				return nil, errors.New("pane pid unavailable")
			}
			return nil, nil
		},
	}

	repoRoot := initTempGitRepo(t)
	worktreePath := t.TempDir()
	gw, err := git.NewGitWorktreeFromStorage(repoRoot, worktreePath, "handoff-broker", "handoff-broker-branch", "", false, false)
	require.NoError(t, err)
	ts := tmux.NewTmuxSessionWithDeps("handoff-broker", tmux.ProgramClaude, ptyFactory, cmdExec)
	backend := &LocalBackend{}
	inst := &Instance{
		ID:          "handoff-broker-id",
		Title:       "handoff-broker",
		Path:        repoRoot,
		Program:     tmux.ProgramGemini,
		backend:     backend,
		Tabs:        []*Tab{newAgentTab(ts)},
		gitWorktree: gw,
		started:     true,
		liveness:    LiveRunning,
	}

	channel := &fakeClientlessChannel{snapshot: []byte("old-pane")}
	broker := newPTYBroker(channel)
	sub, err := broker.subscribe(0)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = sub.Close()
		broker.close()
	})
	server := inst.AgentServer().(*localAgentServer)
	server.brokers = map[string]*ptyBroker{"agent": broker}

	plan := AgentSwapPlan{target: tmux.ProgramGemini, program: tmux.ProgramGemini}
	require.NoError(t, backend.SwapAgent(inst, plan))
	require.Equal(t, 1, channel.stops, "handoff must stop the capture bound to the outgoing pane")
	require.Equal(t, 2, channel.starts, "the attached subscriber must resume on the incoming pane without reconnecting")
}

// recordingPtyFactory is a tmux.PtyFactory that records each exec.Cmd passed
// to Start, lets the caller inspect the new-session vs attach-session sequence
// emitted by Restore's lazy-respawn path. It returns a real (writable) temp
// file as the PTY so callers that close it don't crash.
type recordingPtyFactory struct {
	t    *testing.T
	cmds []*exec.Cmd
}

func (p *recordingPtyFactory) Start(c *exec.Cmd) (*os.File, error) {
	path := filepath.Join(p.t.TempDir(), fmt.Sprintf("pty-%s-%d", p.t.Name(), rand.Int31()))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	p.cmds = append(p.cmds, c)
	return f, nil
}

func (p *recordingPtyFactory) Close() {}

// TestLocalBackendStartRestoreReinjectsSystemPrompt is a regression test for
// issue #511. After a reboot the tmux server is gone, so Restore takes the
// lazy-respawn path added in #386/#444 and spawns a fresh tmux session using
// the program string stored on the TmuxSession. Before the fix that program
// was the raw `i.Program` (e.g. "claude") with no `--plugin-dir` flag, so
// Agent Factory's /af-* slash commands silently disappeared until the user
// killed and recreated the session. The fix re-injects the system prompt in
// LocalBackend.Start before calling Restore, so the respawned tmux session
// receives the same program string as the original first-time launch.
func TestLocalBackendStartRestoreReinjectsSystemPrompt(t *testing.T) {
	// Isolate the plugin dir to a temp config home so ensurePluginDir has
	// somewhere safe to write and tests don't fight over a shared dir.
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	ptyFactory := &recordingPtyFactory{t: t}

	// First two has-session calls report missing (the outer Restore check, then
	// the existence check at the top of Start). After tmux new-session runs,
	// subsequent has-session calls report exists so Start's poll loop and the
	// inner Restore("") attach call succeed.
	hasSessionCalls := 0
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if strings.Contains(c.String(), "has-session") {
				hasSessionCalls++
				if hasSessionCalls <= 2 {
					return fmt.Errorf("can't find session")
				}
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			return []byte("output"), nil
		},
	}

	repoRoot := initTempGitRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "worktree-511")
	gw, err := git.NewGitWorktreeFromStorage(repoRoot, worktreePath, "respawn-511", "respawn-511-branch", "", false, false)
	require.NoError(t, err)

	// The tmuxSession is pre-attached on the instance (the production path
	// builds it from persisted state). It starts with the raw program string,
	// just like a freshly-deserialized instance.
	ts := tmux.NewTmuxSessionWithDeps("respawn-511", "claude", ptyFactory, cmdExec)

	inst := &Instance{
		Title:       "respawn-511",
		Path:        repoRoot,
		Program:     "claude",
		backend:     &LocalBackend{},
		Tabs:        []*Tab{newAgentTab(ts)},
		gitWorktree: gw,
	}

	require.NoError(t, inst.Start(false))
	assert.True(t, inst.Started())

	require.GreaterOrEqual(t, len(ptyFactory.cmds), 1,
		"expected at least one PTY command from the respawn path")
	newSessionCmd := cmd.ToString(ptyFactory.cmds[0])
	require.Contains(t, newSessionCmd, "new-session",
		"first PTY command must be the lazy-respawn new-session (not an attach)")
	require.Contains(t, newSessionCmd, "--plugin-dir",
		"respawned session must include claude --plugin-dir injection so /af-* slash commands keep working (#511)")
}

// --- remote terminal capability (#1592 Phase 4 PR7) ---
//
// The per-config terminal_cmd hook is DELETED: a remote session's terminal tab is
// now served by the in-sandbox af agent-server over the WS PTY stream, exactly
// like docker/ssh, so a hook session always advertises TerminalTab and attaches
// client-side. The former routing-guard subtests here asserted that the backend's
// Attach/AttachTerminal errored rather than attaching; #1852 deleted that surface
// outright, so there is nothing left to mis-route and nothing to guard — what
// remains worth pinning is the capability the client's dispatch reads.

func TestInstanceRemoteTerminalCapability(t *testing.T) {
	t.Run("remote hook session always advertises the terminal tab", func(t *testing.T) {
		caps := (&Instance{backend: &HookBackend{}}).Capabilities()
		assert.True(t, caps.Workspace == WorkspaceRemote && caps.TerminalTab,
			"a provision-and-expose hook session has full parity, incl. the terminal tab")
	})
}

// NOTE: the #895 "restore PTY-allocation failure must not kill the live session"
// regression was retired in #1592 Phase 2 PR7. Restore no longer opens a `tmux
// attach-session` PTY (the local runtime's data plane is the daemon's clientless
// broker), so restoring a live session can no longer fail on PTY allocation — the
// failure mode #895 guarded was designed away, not patched.

// TestLocalBackendKillRunsKillSession is the counterpart to #895: a genuine
// Kill must still run `tmux kill-session`. This guards against an
// over-correction that would leak live sessions by never killing them.
func TestLocalBackendKillRunsKillSession(t *testing.T) {
	var mu sync.Mutex
	var killed bool
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if strings.Contains(strings.Join(c.Args, " "), "kill-session") {
				mu.Lock()
				killed = true
				mu.Unlock()
			}
			return nil
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return []byte(""), nil },
	}

	ts := tmux.NewTmuxSessionWithDeps("genuine-kill", "claude", nil, cmdExec)
	inst := &Instance{
		Title:   "genuine-kill",
		backend: &LocalBackend{},
		started: true,
		Tabs:    []*Tab{newAgentTab(ts)},
	}

	require.NoError(t, inst.Kill())
	mu.Lock()
	defer mu.Unlock()
	assert.True(t, killed, "a genuine Kill must run tmux kill-session")
}

// TestLocalBackendCloseAttachOnlyNeverKillsSharedSession is the enduring half of
// #867/#1065: the daemon discards a duplicate multi-tab Instance restored from
// disk via CloseAttachOnly, and that must NEVER run `tmux kill-session` (the
// server-side sessions are shared with the canonical tracked Instance) and must
// clear the duplicate's local refs. The attach-PTY fd leak the original #1065
// guarded is structurally gone since #1592 Phase 2 PR7 — Restore opens no attach
// PTY — so this now pins the surviving contract: no kill, refs cleared, clean.
func TestLocalBackendCloseAttachOnlyNeverKillsSharedSession(t *testing.T) {
	var mu sync.Mutex
	var ran []string
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			mu.Lock()
			ran = append(ran, strings.Join(c.Args, " "))
			mu.Unlock()
			return nil
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return []byte("output"), nil },
	}

	agentTs := tmux.NewTmuxSessionFromSanitizedNameWithDeps("af_dup_agent", "claude", nil, cmdExec)
	shellTs := tmux.NewTmuxSessionFromSanitizedNameWithDeps("af_dup_agent__shell", "/bin/sh", nil, cmdExec)
	procTs := tmux.NewTmuxSessionFromSanitizedNameWithDeps("af_dup_agent__btop", "btop", nil, cmdExec)

	inst := &Instance{
		Title:   "dup",
		backend: &LocalBackend{},
		started: true,
		Tabs: []*Tab{
			newAgentTab(agentTs),
			newShellTab(shellTs),
			{Name: "btop", Kind: TabKindProcess, Command: "btop", tmux: procTs},
		},
	}

	require.NoError(t, inst.CloseAttachOnly())

	// No session was killed: they are shared with the canonical Instance.
	mu.Lock()
	for _, c := range ran {
		assert.NotContains(t, c, "kill-session",
			"CloseAttachOnly must never kill the shared tmux sessions; ran: %v", ran)
	}
	mu.Unlock()

	// Every tab's tmux ref was cleared and the instance is not-started.
	for _, tab := range inst.GetTabs() {
		assert.Nil(t, tab.tmux, "tab %q tmux ref must be cleared", tab.Name)
	}
	assert.False(t, inst.Started(), "discarded duplicate must be marked not-started")
}
