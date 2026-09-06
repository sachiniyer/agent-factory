package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/stretchr/testify/require"
)

// These are app-model driver captures: real Update/View paths with deterministic
// RPC failures. The real tmux onboarding driver supplements these fixtures.
func TestRecoveryDriverScenes(t *testing.T) {
	sourceDir, err := os.Getwd()
	require.NoError(t, err)
	profile, dark := lipgloss.ColorProfile(), lipgloss.HasDarkBackground()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile); lipgloss.SetHasDarkBackground(dark) })
	for _, theme := range []string{"light", "dark"} {
		for _, scene := range []string{"zero-sessions", "no-project", "zero-tasks", "zero-accounts", "remote-accounts", "tasks-unavailable", "projects-unavailable", "no-daemon", "create-failed", "archive-failed", "kill-failed", "task-save-failed", "task-edit-save-failed", "too-small"} {
			t.Run(scene+"-"+theme, func(t *testing.T) {
				lipgloss.SetHasDarkBackground(theme == "dark")
				h := newTestHome(t)
				ui.ApplyAppearance(theme)
				h.termWidth, h.termHeight = 120, 36
				h.repoRoot = "/project"
				h.relayout()
				want := ""
				switch scene {
				case "zero-sessions":
					want = "No sessions yet"
				case "no-project":
					h.repoRoot = ""
					want = "No project registered"
				case "zero-tasks":
					h.state = stateTasks
					h.automations.TaskPane().SetFocus(true)
					want = "No tasks"
				case "zero-accounts":
					h.state = stateConfigEditor
					h.configPane.SetAccounts(nil, []string{"claude", "codex"}, nil)
					h.configPane.SetFocus(true)
					want = "No accounts"
				case "remote-accounts":
					t.Setenv("AF_DAEMON_URL", "http://buildbox:8443")
					h.state = stateConfigEditor
					h.configPane.SetEntries([]config.ConfigEntry{{Key: "default_program", Value: "codex", Tier: 1}},
						"http://buildbox:8443 · /srv/af/config.toml")
					t.Cleanup(SetAccountSeamsForTest(func(daemon.ListAccountsRequest) (daemon.ListAccountsResponse, error) {
						return daemon.ListAccountsResponse{Entries: []daemon.AccountEntry{{Agent: "codex", Name: "remote-work"}}, Agents: []string{"codex"}}, nil
					}, registerAccount, startAccountLogin))
					load := h.loadAccountsIntoPane()
					h.configPane.SetFocus(true)
					require.NotNil(t, load)
					_, _ = h.Update(load())
					_, _ = h.Update(tea.KeyMsg{Type: tea.KeyDown})
					_, _ = h.Update(tea.KeyMsg{Type: tea.KeyEnter})
					require.True(t, h.configPane.HasFocus(), "refused login keeps Accounts visible")
					require.Contains(t, ansi.Strip(h.configPane.String()), "http://buildbox:8443")
					want = "cannot do that against a remote daemon"
				case "tasks-unavailable":
					h.state = stateTasks
					h.automations.TaskPane().SetFocus(true)
					h.refreshTasks(nil, errors.New("The task file could not be read."))
					want = "Cannot load tasks"
				case "projects-unavailable":
					h.repoRoot = ""
					h.projects.SetDegraded(true)
					want = "Cannot load projects"
				case "no-daemon":
					clock := &fakeClock{}
					h.snapshotClock = clock.Now
					h.handleSnapshot(snapshotFetchedMsg{err: errors.New("connection refused")})
					clock.advance(snapshotFailureGrace)
					h.handleSnapshot(snapshotFetchedMsg{err: errors.New("connection refused")})
					want = "Cannot reach the daemon"
				case "create-failed":
					inst := newLoadingInstance(t, "Retained session")
					inst.Path = h.repoRoot
					h.store.AddInstance(inst)
					h.sidebar.SelectInstance(inst)
					req := sessionStartRequest{Title: inst.Title, RepoPath: h.repoRoot, Program: "claude", Prompt: "Keep my prompt", Backend: "local", Account: "work"}
					_, _ = h.Update(instanceStartedMsg{instance: inst, draft: &req, rawPrompt: " Keep my prompt\n", err: errors.New("The daemon refused this create.")})
					require.Same(t, inst, h.namingInstance)
					require.Equal(t, " Keep my prompt\n", h.pendingPrompt)
					require.Equal(t, "work", h.pendingAccount)
					want = "Cannot create session"
				case "archive-failed", "kill-failed":
					inst := newLoadingInstance(t, "Retained session")
					h.store.AddInstance(inst)
					target := captureSessionActionTarget(inst, h.repoID)
					if scene == "archive-failed" {
						_, _ = h.Update(instanceArchivedMsg{target: target, err: errors.New("The daemon refused this archive.")})
						want = "Cannot archive session"
					} else {
						_, _ = h.Update(instanceKilledMsg{target: target, err: errors.New("The daemon refused this kill.")})
						want = "Cannot kill session"
					}
					require.True(t, h.store.ContainsInstance(inst))
				case "task-edit-save-failed":
					h.repoRoot = setupRealRepo(t)
					t.Chdir(h.repoRoot)
					sp := h.automations.TaskPane()
					sp.SetTasks([]task.Task{{ID: "draft", Name: "Retained task", Enabled: true}})
					sp.SetFocus(true)
					sp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
					t.Cleanup(SetTaskUpdaterForTest(func(string, task.TaskUpdate, task.ProjectExpectation) error {
						return errors.New("The daemon refused this save.")
					}))
					require.Error(t, h.saveContentPaneState())
					require.True(t, sp.IsDirty())
					require.False(t, sp.GetTasks()[0].Enabled)
					want = "Cannot save task"
				case "task-save-failed":
					h.repoRoot = setupRealRepo(t)
					sp := h.automations.TaskPane()
					sp.EnterCreateMode(h.repoRoot)
					h.state = stateTasks
					key := func(msg tea.KeyMsg) { sp.HandleKeyPress(msg) }
					key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Retained task")})
					for i := 0; i < 3; i++ {
						key(tea.KeyMsg{Type: tea.KeyTab})
					}
					key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Keep my task prompt")})
					key(tea.KeyMsg{Type: tea.KeyShiftTab})
					key(tea.KeyMsg{Type: tea.KeyShiftTab})
					key(tea.KeyMsg{Type: tea.KeyShiftTab})
					key(tea.KeyMsg{Type: tea.KeyEnter})
					require.True(t, sp.HasPendingCreate())
					t.Cleanup(SetTaskAdderForTest(func(task.Task) error { return errors.New("The daemon refused this save.") }))
					h.handleTaskCreate()
					require.True(t, sp.IsCreating())
					want = "Cannot save task"
				case "too-small":
					h.termWidth, h.termHeight = 39, 9
					h.relayout()
					want = "Terminal too small"
				}
				frame := h.View()
				require.Contains(t, ansi.Strip(frame), want)
				if h.recovery != nil {
					_, _ = h.Update(tea.KeyMsg{Type: tea.KeySpace})
					require.Nil(t, h.recovery, "one action returns to the retained state")
					if scene == "task-save-failed" {
						require.Contains(t, ansi.Strip(h.automations.TaskPane().String()), "Retained task")
						require.Contains(t, ansi.Strip(h.automations.TaskPane().String()), "Keep my task prompt")
					}
				}
				svg := recoverySVG(frame, theme, h.termWidth, h.termHeight)
				golden := filepath.Join(sourceDir, "testdata", "recovery", scene+"-"+theme+".svg")
				if out := os.Getenv("AF_TUI_RECOVERY_CAPTURE"); out != "" {
					require.NoError(t, os.MkdirAll(out, 0755))
					require.NoError(t, os.WriteFile(filepath.Join(out, filepath.Base(golden)), []byte(svg), 0644))
					require.NoError(t, os.WriteFile(filepath.Join(out, strings.TrimSuffix(filepath.Base(golden), ".svg")+".ansi"), []byte(frame), 0644))
					return
				}
				data, err := os.ReadFile(golden)
				require.NoError(t, err)
				require.Equal(t, string(data), svg, "app view changed: inspect and recapture the recovery still")
			})
		}
	}
}

func TestRecoveryRetainsFailedCreateWhileAnotherFormIsOpen(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot = "/project"
	failed := newLoadingInstance(t, "first")
	newer := newLoadingInstance(t, "second")
	h.store.AddInstance(failed)
	h.store.AddInstance(newer)
	h.namingInstance = newer
	h.state = stateNew
	h.pendingPrompt = "new draft"
	req := sessionStartRequest{Title: failed.Title, RepoPath: h.repoRoot, Program: "claude"}
	_, _ = h.Update(instanceStartedMsg{instance: failed, draft: &req, rawPrompt: "old draft", err: errors.New("rejected")})
	require.Same(t, newer, h.namingInstance)
	require.Equal(t, "new draft", h.pendingPrompt)
	require.NotNil(t, h.failedCreate)
}

// The same unavailable condition must survive launch, not just a later poll.
func TestRecoveryColdStartFailureKeepsRetrying(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot = "/project"
	h.termWidth, h.termHeight = 120, 36
	h.relayout()
	require.Error(t, h.coldStartFromSnapshot())
	require.True(t, h.snapshotUnavailable)
	require.Contains(t, ansi.Strip(h.View()), "Cannot reach the daemon")
	require.NotContains(t, ansi.Strip(h.View()), "No sessions yet")
	require.True(t, h.handleSnapshot(snapshotFetchedMsg{}))
	require.False(t, h.snapshotUnavailable)
	require.Contains(t, ansi.Strip(h.View()), "No sessions yet")
}

func TestRecoveryMouseDismissDoesNotActivateUnderlyingUI(t *testing.T) {
	h := newTestHome(t)
	h.recovery = &recoveryNotice{"Cannot save task", "Your input is retained.", "Press any key to continue."}
	_, cmd := h.Update(tea.MouseMsg{X: 10, Y: 10, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	require.Nil(t, cmd)
	require.Nil(t, h.recovery)
	require.Equal(t, stateDefault, h.state)
}
