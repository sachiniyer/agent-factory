package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/ui/tree"
)

// tabHotkeysPty is a minimal tmux.PtyFactory backed by a real temp file so the
// attach path can open/close it, forwarding the command to the mock executor so
// session-existence bookkeeping fires.
type tabHotkeysPty struct {
	t       *testing.T
	cmdExec cmd_test.MockCmdExec
}

func (p tabHotkeysPty) Start(cmd *exec.Cmd) (*os.File, error) {
	f, err := os.CreateTemp(p.t.TempDir(), "pty-")
	if err == nil {
		_ = p.cmdExec.Run(cmd)
	}
	return f, err
}
func (p tabHotkeysPty) Close() {}

// nameKeyedTmuxExec tracks tmux session existence per name so an instance's
// agent session and any shell siblings are independent. It also returns a spawn
// hook that marks a session alive WITHOUT a new-session, modeling the real
// daemon spawning a tab's tmux session server-side before the TUI attaches:
// since #1152 AttachShellTab is attach-only and no longer resurrects a missing
// session, so the session must already exist when the TUI reflects the tab.
func nameKeyedTmuxExec() (cmd_test.MockCmdExec, func(sessionName string)) {
	existing := map[string]bool{}
	nameOf := func(cmd *exec.Cmd) string {
		for i, a := range cmd.Args {
			switch {
			case (a == "-t" || a == "-s") && i+1 < len(cmd.Args):
				// Strip the exact-match `=name:` wrapper (`-t =name:`) so the
				// modeled session name matches the bare key tmux resolves to (#1006).
				return strings.TrimSuffix(strings.TrimPrefix(cmd.Args[i+1], "="), ":")
			case strings.HasPrefix(a, "-t="):
				return strings.TrimPrefix(a, "-t=")
			case strings.HasPrefix(a, "-s="):
				return strings.TrimPrefix(a, "-s=")
			}
		}
		return ""
	}
	exec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			s := cmd.String()
			n := nameOf(cmd)
			switch {
			case strings.Contains(s, "has-session"):
				if existing[n] {
					return nil
				}
				return fmt.Errorf("session does not exist")
			case strings.Contains(s, "new-session"):
				existing[n] = true
				return nil
			case strings.Contains(s, "kill-session"):
				delete(existing, n)
				return nil
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte("content"), nil
		},
	}
	spawn := func(sessionName string) { existing[sessionName] = true }
	return exec, spawn
}

func setupGitRepoForTabs(t *testing.T, workdir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init"},
		{"config", "--local", "user.email", "test@example.com"},
		{"config", "--local", "user.name", "Test User"},
	} {
		c := exec.Command("git", args...)
		c.Dir = workdir
		require.NoError(t, c.Run())
	}
	require.NoError(t, os.WriteFile(filepath.Join(workdir, "test.txt"), []byte("x"), 0644))
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "init"}} {
		c := exec.Command("git", args...)
		c.Dir = workdir
		require.NoError(t, c.Run())
	}
}

// freshLocalInstance returns a started local instance with a live mock-backed
// agent session and nothing else — the exact shape a user's new instance has
// since #1100: one agent tab, no shell tab until 't'.
func freshLocalInstance(t *testing.T, title string) *session.Instance {
	t.Helper()
	return freshLocalInstanceNamed(t, fmt.Sprintf("af-tabs-%s-%d", title, time.Now().UnixNano()))
}

func freshLocalInstanceNamed(t *testing.T, name string) *session.Instance {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	workdir := t.TempDir()
	setupGitRepoForTabs(t, workdir)

	cmdExec, spawn := nameKeyedTmuxExec()
	pty := tabHotkeysPty{t: t, cmdExec: cmdExec}

	inst, err := session.NewInstance(session.InstanceOptions{Title: name, Path: workdir, Program: "bash"})
	require.NoError(t, err)
	inst.SetTmuxSession(tmux.NewTmuxSessionWithDeps(name, "bash", pty, cmdExec))
	require.NoError(t, inst.Start(true))
	require.Equal(t, 1, inst.TabCount(), "a fresh start must yield only the agent tab (#1100)")
	registerDaemonSpawn(t, inst, spawn)
	return inst
}

// daemonSpawnHooks maps a test instance to the closure that marks a tmux session
// alive in its mock exec. A stubbed daemon CreateTab uses it to model the real
// daemon spawning a tab's session server-side BEFORE the TUI attaches — required
// since #1152 made AttachShellTab attach-only (it no longer resurrects a missing
// session, so the session must already exist when the TUI reflects the tab).
// Package-level rather than threaded through the many builder call sites; these
// tests use t.Setenv so none run in parallel, making the shared map race-free.
var daemonSpawnHooks = map[*session.Instance]func(string){}

func registerDaemonSpawn(t *testing.T, inst *session.Instance, spawn func(string)) {
	daemonSpawnHooks[inst] = spawn
	t.Cleanup(func() { delete(daemonSpawnHooks, inst) })
}

// spawnDaemonTab is the shared body of every stubbed daemon CreateTab: derive
// the next shell tab name, mark that sibling session alive in the mock (the
// daemon's server-side spawn), and return the name AND the tmux session name for
// the TUI to attach to — both, as the real RPC does since #1957, so the stub
// can't quietly re-establish the name-derives-the-session assumption the fix
// removed.
func spawnDaemonTab(inst *session.Instance) (string, string, error) {
	name := nextShellTabName(inst.GetTabs())
	tmuxName := inst.TabTmuxName(0) + "__" + name
	if spawn := daemonSpawnHooks[inst]; spawn != nil {
		spawn(tmuxName)
	}
	return name, tmuxName, nil
}

// startedLocalInstance is freshLocalInstance plus one on-demand shell tab
// (added via AddShellTab — the 't' path), so handleNewTab / handleCloseTab
// exercise the real tab-lifecycle path hermetically.
func startedLocalInstance(t *testing.T, title string) *session.Instance {
	t.Helper()
	inst := freshLocalInstance(t, title)
	_, err := inst.AddShellTab()
	require.NoError(t, err)
	return inst
}

// selectInstance wires the instance into the sidebar + store selection,
// mirroring what selectionChanged would do.
func selectInstance(h *home, inst *session.Instance) {
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(h.store.NumInstances() - 1)
	h.store.SetSelectedInstance(inst)
	h.clampSelectionTab()
}

// nextShellTabName mirrors session.uniqueShellName ("shell", "shell-2", …) so a
// stubbed daemon CreateTab returns the same name the real daemon would derive
// from the instance's current tab list — letting AttachShellTab reconnect by the
// expected name. Kept here (not imported) so production stays free of a
// test-only export.
func nextShellTabName(tabs []*session.Tab) string {
	used := map[string]bool{}
	for _, t := range tabs {
		used[t.Name] = true
	}
	if !used["shell"] {
		return "shell"
	}
	for n := 2; ; n++ {
		c := fmt.Sprintf("shell-%d", n)
		if !used[c] {
			return c
		}
	}
}

// stubTabDaemonSeams installs daemon CreateTab/CloseTab seam stubs that simulate
// the daemon hermetically against the single in-process instance (#960 PR 2):
// CreateTab returns the next shell name (the TUI then reconnects via
// AttachShellTab); CloseTab is a no-op (the TUI then drops the tab locally via
// DropClosedTab). Records the calls so tests can assert routing went through the
// RPC rather than a local save. Returns pointers to the call counters and a
// restore func.
func stubTabDaemonSeams(t *testing.T, inst *session.Instance) (created, closed *int) {
	t.Helper()
	var c, d int
	t.Cleanup(SetTabCreatorForTest(func(daemon.CreateTabRequest) (string, string, error) {
		c++
		return spawnDaemonTab(inst)
	}))
	t.Cleanup(SetTabCloserForTest(func(daemon.CloseTabRequest) error {
		d++
		return nil
	}))
	return &c, &d
}

// TestHandleNewTabAppendsAndSelects: the new-tab hotkey appends a shell tab and
// selects it.
func TestHandleNewTabAppendsAndSelects(t *testing.T) {
	h := newTestHome(t)
	inst := startedLocalInstance(t, "new")
	selectInstance(h, inst)
	created, _ := stubTabDaemonSeams(t, inst)
	require.Equal(t, 2, inst.TabCount(), "helper gives an agent tab + one on-demand shell tab")

	_, _ = h.handleNewTab()

	require.Equal(t, 1, *created, "new-tab must route through the daemon CreateTab RPC")
	require.Equal(t, 3, inst.TabCount(), "new-tab must append a shell tab")
	require.Equal(t, 2, h.store.ActiveTab(),
		"the freshly created tab must be selected")
}

// TestNewTabPickerCreatesVSCodeThroughDaemon is the #2077 TUI acceptance path:
// the actual `t` action opens a visible kind choice, and choosing VS Code routes
// through the daemon seam before reflecting the same TabKindVSCode the CLI's
// --kind vscode request creates.
func TestNewTabPickerCreatesVSCodeThroughDaemon(t *testing.T) {
	h := newTestHome(t)
	inst := freshLocalInstance(t, "vscode-picker")
	selectInstance(h, inst)

	called := 0
	t.Cleanup(SetTabCreatorForTest(func(request daemon.CreateTabRequest) (string, string, error) {
		called++
		require.Equal(t, inst.ID, request.ID)
		require.Equal(t, inst.Title, request.Title)
		require.Equal(t, h.repoID, request.RepoID)
		require.Equal(t, "vscode", request.Kind)
		return "vscode", "", nil
	}))

	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}}, keys.KeyNewTab)
	require.Equal(t, stateSelectTabKind, h.state)
	require.NotNil(t, h.selectionOverlay)
	h.termWidth, h.termHeight = 80, 24
	h.layoutSelectionOverlay()
	picker := xansi.Strip(h.selectionOverlay.Render())
	require.Contains(t, picker, "Terminal")
	require.Contains(t, picker, "VS Code")

	h.selectionOverlay.SetSelectedIndex(1)
	_, _ = h.handleStateSelectTabKind(tea.KeyMsg{Type: tea.KeyEnter})

	require.Equal(t, 1, called, "VS Code creation must cross the daemon CreateTab seam")
	require.Equal(t, stateDefault, h.state)
	require.Nil(t, h.selectionOverlay)
	require.Equal(t, 2, inst.TabCount())
	tabs := inst.GetTabs()
	require.Equal(t, session.TabKindVSCode, tabs[1].Kind)
	require.Empty(t, tabs[1].ID, "the projection waits for the daemon's authoritative tab id")
	require.Equal(t, 1, h.store.ActiveTab(), "the fresh VS Code tab must be selected")
	require.Equal(t, []string{"◆ Agent", "◱ vscode"}, tree.TabLabels(inst),
		"the resolved daemon name is the tab's addressable label, matching CLI creation")
}

// A tab picker retains intent about one session while its modal owns the
// keyboard. A different session that reuses the title in that window must not
// inherit the pending create.
func TestNewTabPickerDoesNotTargetSameTitleReplacement(t *testing.T) {
	h := newTestHome(t)
	stale := freshLocalInstance(t, "vscode-stale")
	selectInstance(h, stale)
	_, _ = h.showNewTabPicker()

	replacement := freshLocalInstanceNamed(t, stale.Title)
	require.NotEqual(t, stale.ID, replacement.ID)
	require.True(t, h.store.ReplaceInstanceByTitle(stale.Title, replacement))
	called := false
	t.Cleanup(SetTabCreatorForTest(func(daemon.CreateTabRequest) (string, string, error) {
		called = true
		return "vscode", "", nil
	}))

	h.selectionOverlay.SetSelectedIndex(1)
	_, _ = h.handleStateSelectTabKind(tea.KeyMsg{Type: tea.KeyEnter})

	require.Equal(t, 1, stale.TabCount(), "the swapped-out projection must stay untouched")
	require.Equal(t, 1, replacement.TabCount(), "the replacement must not inherit the pending tab create")
	require.False(t, called, "a stale picker target must fail closed before the daemon request")
}

func TestNewTabPickerFollowsSameIdentitySnapshotRebuild(t *testing.T) {
	h := newTestHome(t)
	stale := freshLocalInstance(t, "vscode-rebuilt")
	selectInstance(h, stale)
	_, _ = h.showNewTabPicker()

	rebuilt := freshLocalInstanceNamed(t, stale.Title)
	rebuilt.ID = stale.ID
	rebuilt.CreatedAt = stale.CreatedAt
	require.True(t, h.store.ReplaceInstanceByTitle(stale.Title, rebuilt))
	var gotRequest daemon.CreateTabRequest
	t.Cleanup(SetTabCreatorForTest(func(request daemon.CreateTabRequest) (string, string, error) {
		gotRequest = request
		return "vscode", "", nil
	}))

	h.selectionOverlay.SetSelectedIndex(1)
	_, _ = h.handleStateSelectTabKind(tea.KeyMsg{Type: tea.KeyEnter})

	require.Equal(t, stale.ID, gotRequest.ID,
		"a pointer rebuild of the same session must retain the captured stable ID")
	require.Equal(t, 1, stale.TabCount(), "the swapped-out pointer must stay untouched")
	require.Equal(t, 2, rebuilt.TabCount(), "the live projection of the same session receives the tab")
}

// TestNewTabPickerDefaultsToTerminal preserves the existing fast path inside the
// new picker: Enter on its first row still creates Shell:true through the same
// daemon seam and selects the resulting terminal.
func TestNewTabPickerDefaultsToTerminal(t *testing.T) {
	h := newTestHome(t)
	inst := freshLocalInstance(t, "terminal-picker")
	selectInstance(h, inst)
	created, _ := stubTabDaemonSeams(t, inst)

	_, _ = h.showNewTabPicker()
	_, _ = h.handleStateSelectTabKind(tea.KeyMsg{Type: tea.KeyEnter})

	require.Equal(t, 1, *created)
	require.Equal(t, session.TabKindShell, inst.GetTabs()[1].Kind)
	require.Equal(t, 1, h.store.ActiveTab())
}

// TestNewTabPickerFitsRequiredTerminalSizes guards both the ordinary 80x24
// play-test viewport and narrower terminals. The picker uses no new status-row
// hint, and its own escape hatch must remain visible instead of being clamped.
func TestNewTabPickerFitsRequiredTerminalSizes(t *testing.T) {
	for _, size := range []struct{ w, h int }{{80, 24}, {60, 15}, {40, 10}} {
		h := newTestHome(t)
		inst := freshLocalInstance(t, fmt.Sprintf("picker-%d", size.w))
		selectInstance(h, inst)
		h.termWidth, h.termHeight = size.w, size.h
		_, _ = h.showNewTabPicker()
		h.layoutSelectionOverlay()

		rendered := h.selectionOverlay.Render()
		for idx, line := range strings.Split(rendered, "\n") {
			require.LessOrEqual(t, lipgloss.Width(line), size.w,
				"picker line %d overflows a %dx%d terminal", idx, size.w, size.h)
		}
		require.LessOrEqual(t, strings.Count(rendered, "\n")+1, size.h,
			"picker is taller than a %dx%d terminal", size.w, size.h)
		plain := xansi.Strip(rendered)
		require.Contains(t, plain, "Terminal")
		require.Contains(t, plain, "VS Code")
		require.Contains(t, plain, "esc cancel", "the way out must survive width pressure")
	}
}

// TestHandleCloseTabSelectsNeighbor: closing a shell tab removes it and selects
// the previous (left) tab.
func TestHandleCloseTabSelectsNeighbor(t *testing.T) {
	h := newTestHome(t)
	inst := startedLocalInstance(t, "close")
	selectInstance(h, inst)
	_, closed := stubTabDaemonSeams(t, inst)
	_, _ = h.handleNewTab() // now agent + shell + shell-2, active = 2
	require.Equal(t, 3, inst.TabCount())
	require.Equal(t, 2, h.store.ActiveTab())

	_, _ = h.handleCloseTab()

	require.Equal(t, 1, *closed, "close must route through the daemon CloseTab RPC")
	require.Equal(t, 2, inst.TabCount(), "close must remove the active tab")
	require.Equal(t, 1, h.store.ActiveTab(),
		"close must select the left neighbor")
}

// TestHandleCloseTabAgentTabNoOp: w on the agent tab (index 0) is a gentle no-op
// with a message, never closing it.
func TestHandleCloseTabAgentTabNoOp(t *testing.T) {
	h := newTestHome(t)
	inst := startedLocalInstance(t, "agentnoop")
	selectInstance(h, inst)
	h.store.SetActiveTab(0)
	require.Equal(t, 2, inst.TabCount())

	_, _ = h.handleCloseTab()

	require.Equal(t, 2, inst.TabCount(), "the agent tab must never be closed")
	h.errBox.SetSize(200, 1)
	require.Contains(t, h.errBox.String(), "agent tab can't be closed")
}

// TestHandleNewTabRemoteShowsMessage removed — remote (hook) backends now have
// full local parity including TabManagement, so new-tab is supported for remote
// instances rather than rejected with a message. // #1592 Phase 4 PR7

// TestHandleTabJump covers the number-key jump handler: in range jumps, out of
// range is a no-op.
func TestHandleTabJump(t *testing.T) {
	h := newTestHome(t)
	inst := startedLocalInstance(t, "jump")
	selectInstance(h, inst)
	stubTabDaemonSeams(t, inst)
	_, _ = h.handleNewTab() // agent + shell + shell-2 (3 tabs)
	require.Equal(t, 3, inst.TabCount())

	_, _ = h.handleTabJump(1)
	require.Equal(t, 0, h.store.ActiveTab(), "1 jumps to tab index 0")

	_, _ = h.handleTabJump(3)
	require.Equal(t, 2, h.store.ActiveTab(), "3 jumps to tab index 2")

	_, _ = h.handleTabJump(9)
	require.Equal(t, 2, h.store.ActiveTab(),
		"an out-of-range number must be a no-op")
}

// TestNumberKeyRoutesToTabJump proves the digit dispatch in handleKeyPress routes
// to the jump handler when viewing an instance.
func TestNumberKeyRoutesToTabJump(t *testing.T) {
	h := newTestHome(t)
	inst := startedLocalInstance(t, "route")
	selectInstance(h, inst)
	stubTabDaemonSeams(t, inst)
	_, _ = h.handleNewTab() // 3 tabs

	h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	require.Equal(t, 1, h.store.ActiveTab(),
		"pressing '2' must jump to tab index 1")
}

// TestFreshInstanceSingleTabSlotUI pins the #1100 headline at the UI layer,
// through the real start path: a fresh instance exposes exactly one tab slot
// everywhere tree.TabLabels feeds (tree rows, pane targets, 1-9 jump range),
// so no phantom "Terminal" slot exists to jump to, attach, or close. `t` then
// grows the list to two real slots and the jump reaches the terminal.
func TestFreshInstanceSingleTabSlotUI(t *testing.T) {
	h := newTestHome(t)
	inst := freshLocalInstance(t, "fresh-ui")
	selectInstance(h, inst)

	require.Equal(t, []string{"◆ Agent"}, tree.TabLabels(inst),
		"a fresh instance renders exactly one tab slot — the agent tab")

	_, _ = h.handleTabJump(2)
	require.Equal(t, 0, h.store.ActiveTab(),
		"2 on a fresh instance must be a no-op — there is no second slot")

	// w can only mean the agent tab now, so the user gets the actionable
	// message — never the phantom slot's misleading "tab cannot be closed".
	_, _ = h.handleCloseTab()
	require.Equal(t, 1, inst.TabCount())
	h.errBox.SetSize(200, 1)
	require.Contains(t, h.errBox.String(), "agent tab can't be closed")

	// t materializes the on-demand terminal as a real second slot.
	stubTabDaemonSeams(t, inst)
	_, _ = h.handleNewTab()
	require.Equal(t, 2, inst.TabCount())
	require.Equal(t, []string{"◆ Agent", "› Terminal"}, tree.TabLabels(inst),
		"after t the terminal is a real second slot")
	require.Equal(t, 1, h.store.ActiveTab(), "t selects the fresh terminal")

	_, _ = h.handleTabJump(1)
	require.Equal(t, 0, h.store.ActiveTab())
	_, _ = h.handleTabJump(2)
	require.Equal(t, 1, h.store.ActiveTab(), "2 now lands on the real terminal")
}
