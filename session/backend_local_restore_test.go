package session

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

// --- terminal_cmd (#843) ---

func TestHookBackendAttachTerminalNotStarted(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{TerminalCmd: "/bin/true"}}
	i := &Instance{backend: b, started: false}
	_, err := b.AttachTerminal(i)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not been started")
}

func TestHookBackendAttachTerminalNotConfigured(t *testing.T) {
	for name, terminalCmd := range map[string]string{"empty": "", "whitespace": "   "} {
		t.Run(name, func(t *testing.T) {
			b := &HookBackend{Hooks: config.RemoteHooks{TerminalCmd: terminalCmd}}
			i := &Instance{backend: b}
			i.started = true
			_, err := b.AttachTerminal(i)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "terminal_cmd",
				"error must name the missing field so the fix is actionable")
		})
	}
}

// TestHookBackendAttachTerminalRunsTerminalCmdWithSlug verifies the
// terminal_cmd contract (#843): the script is invoked with the session's hook
// name as its only positional argument, behind a PTY (mirroring attach_cmd),
// and the done channel closes when it exits. The attach_cmd-based preview
// process must be left running — terminal_cmd opens a separate shell, so
// unlike Attach there is nothing to stop.
func TestHookBackendAttachTerminalRunsTerminalCmdWithSlug(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "terminal-args")
	terminalCmd := writeScript(t, dir, "terminal.sh",
		`echo "$1" > "`+argsFile+`"`)
	// Preview attach_cmd prints once and lingers so we can observe that
	// AttachTerminal does not tear it down.
	attachCmd := writeScript(t, dir, "attach.sh",
		`echo "preview for $1"
sleep 5`)

	b := &HookBackend{Hooks: config.RemoteHooks{AttachCmd: attachCmd, TerminalCmd: terminalCmd}}
	i := &Instance{
		Title:   "Terminal Cmd Test",
		Path:    t.TempDir(),
		backend: b,
	}
	i.started = true

	require.NoError(t, b.ensurePTY(i))
	require.NotNil(t, b.getPTY(i.Title))
	defer b.closePTY(i.Title)

	done, err := b.AttachTerminal(i)
	require.NoError(t, err)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("terminal_cmd did not exit")
	}

	raw, err := os.ReadFile(argsFile)
	require.NoError(t, err, "terminal_cmd should have run and recorded its args")
	assert.Equal(t, Slugify("Terminal Cmd Test"), strings.TrimSpace(string(raw)),
		"terminal_cmd must receive the session slug as its positional argument")

	assert.NotNil(t, b.getPTY(i.Title),
		"AttachTerminal must leave the attach_cmd preview process running")
}

// TestHookBackendAttachTerminalUsesRemoteMetaName mirrors the attach_cmd
// behavior: an imported session's authoritative remote_meta.name wins over the
// title-derived slug.
func TestHookBackendAttachTerminalUsesRemoteMetaName(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "terminal-args")
	terminalCmd := writeScript(t, dir, "terminal.sh",
		`echo "$1" > "`+argsFile+`"`)

	b := &HookBackend{Hooks: config.RemoteHooks{TerminalCmd: terminalCmd}}
	i := &Instance{
		Title:   "display title",
		Path:    t.TempDir(),
		backend: b,
	}
	i.started = true
	i.remoteMeta = map[string]interface{}{"name": "imported-name"}

	done, err := b.AttachTerminal(i)
	require.NoError(t, err)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("terminal_cmd did not exit")
	}

	raw, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	assert.Equal(t, "imported-name", strings.TrimSpace(string(raw)))
}

func TestInstanceRemoteTerminalCapability(t *testing.T) {
	t.Run("remote with terminal_cmd advertises the terminal tab", func(t *testing.T) {
		caps := (&Instance{backend: &HookBackend{Hooks: config.RemoteHooks{TerminalCmd: "/bin/true"}}}).Capabilities()
		assert.True(t, caps.Workspace == WorkspaceRemote && caps.TerminalTab)
	})

	t.Run("remote without terminal_cmd does not", func(t *testing.T) {
		caps := (&Instance{backend: &HookBackend{}}).Capabilities()
		assert.False(t, caps.Workspace == WorkspaceRemote && caps.TerminalTab)
	})

	t.Run("non-hook backend rejects AttachRemoteTerminal", func(t *testing.T) {
		i := &Instance{backend: &LocalBackend{}}
		caps := i.Capabilities()
		assert.False(t, caps.Workspace == WorkspaceRemote && caps.TerminalTab)
		_, err := i.AttachRemoteTerminal()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "remote sessions")
	})
}

// errPtyFactory is a tmux.PtyFactory whose Start always fails — it simulates a
// PTY allocation failure (EMFILE/ENOMEM) on the attach path so a restore can
// fail AFTER `tmux has-session` confirms the server-side session is alive.
type errPtyFactory struct{ err error }

func (e errPtyFactory) Start(_ *exec.Cmd) (*os.File, error) { return nil, e.err }
func (e errPtyFactory) Close()                              {}

// TestLocalBackendRestorePtyFailureDoesNotKillSession is the regression test
// for issue #895. When restoring an existing instance, `tmux has-session`
// confirms the server-side session is alive but the local attach PTY cannot be
// allocated (EMFILE/ENOMEM), Restore returns an error. The deferred restore
// cleanup in LocalBackend.Start must tear down only the local attach
// resources (CloseAttachOnly) — it must NOT run `tmux kill-session`, which
// would destroy a live, recoverable session (scrollback + running processes)
// and turn a transient attach failure into data loss.
func TestLocalBackendRestorePtyFailureDoesNotKillSession(t *testing.T) {
	var mu sync.Mutex
	var ran []string
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			mu.Lock()
			ran = append(ran, strings.Join(c.Args, " "))
			mu.Unlock()
			// All commands (notably has-session) succeed → Restore takes the
			// attach branch where the PTY allocation failure fires.
			return nil
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return []byte("output"), nil },
	}

	ts := tmux.NewTmuxSessionWithDeps("restore-pty-fail", "claude",
		errPtyFactory{err: fmt.Errorf("pty allocation failed")}, cmdExec)
	inst := &Instance{
		Title:   "restore-pty-fail",
		backend: &LocalBackend{},
		started: true,
		Tabs:    []*Tab{newAgentTab(ts)},
	}

	// firstTimeSetup=false → restore path. No gitWorktree is attached, so
	// Restore is called with an empty workDir and a present session attaches
	// (then fails at PTY) rather than re-spawning.
	err := inst.backend.Start(inst, false)
	require.Error(t, err, "restore must surface the PTY allocation failure")
	assert.Contains(t, err.Error(), "pty allocation failed")

	mu.Lock()
	defer mu.Unlock()
	for _, c := range ran {
		assert.NotContains(t, c, "kill-session",
			"restore PTY failure must NOT kill the live tmux session (#895); ran: %v", ran)
	}

	// The local attach was still torn down: the instance no longer holds the
	// session and is marked not-started so a later retry is clean.
	assert.Nil(t, inst.tmuxLocked(), "attach resources should be released on restore failure")
	assert.False(t, inst.Started(), "instance should be marked not-started after a failed restore")
}

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

// closeTrackingPtyFactory is a tmux.PtyFactory returning real temp files as
// PTYs and recording every one it hands out, so a test can assert each was
// closed (a second Close on an *os.File fails with os.ErrClosed).
type closeTrackingPtyFactory struct {
	t     *testing.T
	mu    sync.Mutex
	files []*os.File
}

func (f *closeTrackingPtyFactory) Start(_ *exec.Cmd) (*os.File, error) {
	file, err := os.CreateTemp(f.t.TempDir(), "pty-")
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.files = append(f.files, file)
	f.mu.Unlock()
	return file, nil
}

func (f *closeTrackingPtyFactory) Close() {}

// TestLocalBackendCloseAttachOnlyReleasesEveryTabPTY is the regression test
// for issue #1065. The daemon discards a duplicate multi-tab Instance restored
// from disk via CloseAttachOnly (#867); since #930 every tab owns its own
// tmux session whose restore opened its own attach PTY, so CloseAttachOnly
// must release the PTY of EVERY tab — not just the agent tab's — and it must
// still never run `tmux kill-session`, because the server-side sessions are
// shared with the canonical tracked Instance.
func TestLocalBackendCloseAttachOnlyReleasesEveryTabPTY(t *testing.T) {
	var mu sync.Mutex
	var ran []string
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			mu.Lock()
			ran = append(ran, strings.Join(c.Args, " "))
			mu.Unlock()
			// has-session succeeds → Restore takes the attach branch and opens
			// a PTY for each session.
			return nil
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return []byte("output"), nil },
	}
	pty := &closeTrackingPtyFactory{t: t}

	agentTs := tmux.NewTmuxSessionFromSanitizedNameWithDeps("af_dup_agent", "claude", pty, cmdExec)
	shellTs := tmux.NewTmuxSessionFromSanitizedNameWithDeps("af_dup_agent__shell", "/bin/sh", pty, cmdExec)
	procTs := tmux.NewTmuxSessionFromSanitizedNameWithDeps("af_dup_agent__btop", "btop", pty, cmdExec)
	for _, ts := range []*tmux.TmuxSession{agentTs, shellTs, procTs} {
		require.NoError(t, ts.Restore(""), "restore must attach and open a PTY")
	}
	pty.mu.Lock()
	nOpened := len(pty.files)
	pty.mu.Unlock()
	require.Equal(t, 3, nOpened, "each tab's restore must have opened its own PTY")

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

	// Every tab's attach PTY was closed — the agent's AND the shell/process
	// tabs' (the fds that leaked before the fix).
	pty.mu.Lock()
	files := append([]*os.File(nil), pty.files...)
	pty.mu.Unlock()
	for i, f := range files {
		assert.ErrorIs(t, f.Close(), os.ErrClosed,
			"PTY %d (%s) must already be closed by CloseAttachOnly", i, f.Name())
	}

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
