package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/overlay"
	"github.com/sachiniyer/agent-factory/ui/store"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Version is set by main before calling Run.
var Version string

// Run is the main entrypoint into the application.
func Run(ctx context.Context, program string, autoYes bool, repo *config.RepoContext) error {
	p := tea.NewProgram(
		newHome(ctx, program, autoYes, repo),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(), // Mouse scroll
	)
	_, err := p.Run()
	return err
}

type state int

const (
	stateDefault state = iota
	// stateNew is the state when the user is creating a new instance.
	stateNew
	// stateHelp is the state when a help screen is displayed.
	stateHelp
	// stateConfirm is the state when a confirmation modal is displayed.
	stateConfirm
	// stateSearch is the state when the user is searching sessions.
	stateSearch
	// stateSelectProgram is the state when the user is selecting a program during naming.
	stateSelectProgram
	// stateHooks is the state when the post-worktree hooks editor overlay is
	// open (#1024 PR 4: hooks lost their persistent sidebar slot and are
	// hosted as a modal overlay instead).
	stateHooks
)

type home struct {
	ctx context.Context

	// -- Storage and Configuration --

	program string
	autoYes bool
	repoID  string
	// repoRoot is the main-worktree root of the repo this TUI run is scoped
	// to. Used to resolve and persist the in-repo .agent-factory/config.json.
	repoRoot string

	// snapshotFetcher fetches the daemon's authoritative session snapshot for
	// this repo. It is a PER-home field, not a package global, precisely because
	// fetchSnapshotCmd reads it from an off-loop tea.Cmd goroutine: a shared
	// mutable global swapped by a test seam would race that goroutine against a
	// sibling test's swap under `go test -parallel`. Each home owns its fetcher,
	// so there is no cross-test shared state to race. Defaults to
	// snapshotThroughDaemon in production; tests assign a fake directly.
	snapshotFetcher func(repoID string) ([]session.InstanceData, error)
	// appConfig stores persistent application configuration
	appConfig *config.Config
	// appState stores persistent application state like seen help screens
	appState config.AppState

	// -- State --

	// state is the current discrete state of the application
	state state
	// namingInstance is the instance currently being named in stateNew.
	// Stored as a direct pointer so background sync cannot change which
	// instance the naming keystrokes target.
	namingInstance *session.Instance

	// keySent is used to manage underlining menu items
	keySent bool

	// -- UI Components --

	// store is the single read-only projection of daemon-owned state that the
	// panes render (#1024 PR 2): the instance list + repo bookkeeping + tasks +
	// hook count, plus the cross-pane selection (selected instance, active tab
	// index). Written on the bubbletea event loop by reconcileSnapshot and the
	// session-control handlers; read by the panes, which keep only their own
	// local UI state (cursor, scroll, expansion).
	store *store.Projection

	// -- Workspace layout (#1024 PR 4) --
	//
	// The window is tiled by layout.Grid into the RFC §2.1 regions: the
	// instances+tabs tree on the left, the single content pane A in the
	// center, the automations strip along the bottom, and the status bar
	// under everything. Each region is a layout.Pane that renders exactly its
	// rect; relayout() re-solves the grid and re-rects the panes.

	// grid is the single sizing authority; termWidth/termHeight the last
	// tea.WindowSizeMsg, so focus changes (which resize the automations
	// strip) can re-solve without waiting for a resize event.
	grid                  layout.Grid
	lastLayout            layout.Layout
	termWidth, termHeight int
	// ring is the focus ring: tree → pane A → automations. Tab/Shift-Tab
	// cycle it; regions hidden by the degradation ladder are skipped.
	ring *layout.Ring

	// sidebar is the left-rail instances+tabs tree
	sidebar *ui.Sidebar
	// paneA is the workspace content pane: the selected tab's live view
	paneA *ui.TabbedWindow
	// automations is the bottom strip (compact task rows; expands to the
	// full task manager on focus)
	automations *ui.AutomationsPane
	// statusBar merges the menu hints and the error line
	statusBar *ui.StatusBar
	// hooksPane is the post-worktree hooks editor, hosted as an overlay
	// (stateHooks)
	hooksPane *ui.HooksPane
	// menu displays the key hints inside the status bar (shared handle for
	// SetState/keydown callers)
	menu *ui.Menu
	// errBox displays error messages inside the status bar (shared handle)
	errBox *ui.ErrBox
	// global spinner instance. we plumb this down to where it's needed
	spinner spinner.Model
	// textOverlay displays text information
	textOverlay *overlay.TextOverlay
	// confirmationOverlay displays confirmation modals
	confirmationOverlay *overlay.ConfirmationOverlay
	// pendingConfirmMsg holds a non-error tea.Msg returned by a confirmation
	// action so that handleStateConfirm can forward it to the Bubble Tea
	// event loop after OnConfirm runs.
	pendingConfirmMsg tea.Msg
	// selectionOverlay handles program selection during new-instance naming
	selectionOverlay *overlay.SelectionOverlay
	// searchOverlay handles session search
	searchOverlay *overlay.SearchOverlay
	// pendingProgram tracks the program selected during new instance naming
	pendingProgram string

	// attached is set while the user is inside an attached tmux session.
	// While true, periodic background work that hits the shared tmux server
	// (capture-pane via runMetadataTick, refreshPanesCmd, fetchPRInfoCmd) is
	// paused so the user's detach key-press is never queued behind it. See
	// issue #598 — the 44s detach hang was traced to wg.Wait waiting on
	// the tmux client to exit, which itself was blocked behind ~40 RPS of
	// capture-pane requests we were generating from the metadata tick.
	//
	// Stored as atomic because the attach overlay's onDismiss callback runs
	// off the bubbletea Update goroutine (as a tea.Cmd) and toggles this
	// while Update reads it on every tick.
	attached atomic.Bool
}

func newHome(ctx context.Context, program string, autoYes bool, repo *config.RepoContext) *home {
	repoID := repo.ID
	// Load application config
	appConfig, err := config.LoadConfig()
	if err != nil {
		fmt.Printf("Failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Apply configured detach key
	if appConfig.DetachKeys != "" {
		b, err := config.ParseDetachKey(appConfig.DetachKeys)
		if err != nil {
			fmt.Printf("Invalid detach_keys %q in config: %v\n", appConfig.DetachKeys, err)
			os.Exit(1)
		}
		tmux.SetDetachKey(b, appConfig.DetachKeys)
	}

	// Load application state (seen-help-screens flags; #960 PR 6 the TUI no
	// longer reads instances.json — the daemon owns it and answers Snapshot).
	appState := config.LoadState()

	proj := store.NewProjection()
	menu := ui.NewMenu()
	errBox := ui.NewErrBox()

	h := &home{
		ctx:             ctx,
		spinner:         spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		store:           proj,
		menu:            menu,
		errBox:          errBox,
		paneA:           ui.NewTabbedWindow(ui.NewTabPane(), proj),
		automations:     ui.NewAutomationsPane(proj),
		statusBar:       ui.NewStatusBar(menu, errBox),
		hooksPane:       ui.NewHooksPane(),
		ring:            layout.NewRing(layout.RegionTree, layout.RegionPaneA, layout.RegionAutomations),
		snapshotFetcher: snapshotThroughDaemon,
		appConfig:       appConfig,
		program:         program,
		autoYes:         autoYes,
		repoID:          repoID,
		repoRoot:        repo.Root,
		state:           stateDefault,
		appState:        appState,
	}
	h.sidebar = ui.NewSidebar(&h.spinner, autoYes, proj)
	h.syncFocus()

	// Cold-start the projection from the daemon's authoritative Snapshot (#960 PR 6).
	// The TUI no longer reads instances.json — the daemon is the sole writer/owner
	// of session state, so startup mirrors the same projection the refresh tick
	// reconciles against. A warming daemon (#829) is waited out, not raced.
	if err := h.coldStartFromSnapshot(); err != nil {
		fmt.Printf("Failed to load sessions from daemon: %v\n", err)
		os.Exit(1)
	}

	h.importRemoteHookSessions()

	// Load tasks for sidebar display
	tasks, err := task.LoadTasksForCurrentRepo()
	if err != nil {
		log.WarningLog.Printf("failed to load tasks: %v", err)
	} else {
		h.store.SetTasks(tasks)
	}

	// Load tasks into the automations strip's task manager
	if len(tasks) > 0 {
		h.automations.TaskPane().SetTasks(tasks)
	}

	// Load hooks for the hooks overlay. ResolveConfig applies the in-repo
	// .agent-factory/config.json over the legacy per-repo file.
	repoCfg, err := config.ResolveConfig(repo.Root)
	if err != nil {
		log.WarningLog.Printf("failed to resolve repo config: %v", err)
	} else {
		h.store.SetHookCount(len(repoCfg.PostWorktreeCommands))
		h.hooksPane.SetCommands(repoCfg.PostWorktreeCommands)
	}

	return h
}

// updateHandleWindowSizeEvent records the terminal size and re-solves the
// layout.
func (m *home) updateHandleWindowSizeEvent(msg tea.WindowSizeMsg) {
	m.termWidth = msg.Width
	m.termHeight = msg.Height
	m.relayout()
}

// relayout is the single sizing path (#1024 PR 4): layout.Grid turns the
// terminal size into the region rects — applying the §2.6 degradation ladder
// — and every pane is re-rected. Called on every WindowSizeMsg and whenever a
// grid input changes without a resize (focusing the automations strip expands
// it in place).
func (m *home) relayout() {
	m.grid.AutomationsExpanded = m.ring.Active() == layout.RegionAutomations
	lay := m.grid.Solve(m.termWidth, m.termHeight)
	m.lastLayout = lay
	if lay.Fallback {
		// No rects to hand out, but keep the focus flags + hints coherent so
		// key routing stays correct while the terminal is too small.
		m.syncFocus()
		return
	}

	// Regions hidden by the ladder leave the focus ring; Ring.Active moves
	// focus forward off a hidden region on its own, so re-sync pane focus
	// flags afterwards.
	m.ring.SetHidden(layout.RegionAutomations, !lay.AutomationsVisible)
	m.syncFocus()

	m.sidebar.SetRect(lay.Tree)
	m.paneA.SetRect(lay.PaneA)
	m.automations.SetRect(lay.Automations)
	m.automations.SetCompact(lay.AutomationsCompact)
	m.statusBar.SetRect(lay.StatusBar)

	if m.textOverlay != nil {
		m.textOverlay.SetWidth(int(float32(m.termWidth) * 0.6))
	}
	if m.selectionOverlay != nil {
		m.selectionOverlay.SetWidth(int(float32(m.termWidth) * 0.6))
	}
	m.hooksPane.SetSize(int(float32(m.termWidth)*0.6), int(float32(m.termHeight)*0.6))

	previewWidth, previewHeight := m.paneA.GetPreviewSize()
	if err := m.store.SetSessionPreviewSize(previewWidth, previewHeight); err != nil {
		log.ErrorLog.Print(err)
	}
}

// syncFocus applies the focus ring's active region to the panes and the
// status-bar hints.
func (m *home) syncFocus() {
	active := m.ring.Active()
	for id, pane := range map[string]layout.Pane{
		layout.RegionTree:        m.sidebar,
		layout.RegionPaneA:       m.paneA,
		layout.RegionAutomations: m.automations,
	} {
		if id == active {
			pane.Focus()
		} else {
			pane.Blur()
		}
	}
	m.menu.SetFocusRegion(active)
}

// focusRegion moves focus directly to the given region (the s/S jump keys)
// and re-solves the layout, since the automations strip sizes off focus.
func (m *home) focusRegion(region string) {
	m.ring.Focus(region)
	m.relayout()
}

// cycleFocus advances the focus ring (Tab / Shift-Tab). Leaving the
// automations strip persists any dirty task edits, exactly like the pre-#1024
// focus-release path: a failed save surfaces in the error box and the panes
// reload to match disk, so the dropped edit is never silent.
func (m *home) cycleFocus(back bool) tea.Cmd {
	leaving := m.ring.Active()
	if back {
		m.ring.Prev()
	} else {
		m.ring.Next()
	}
	var cmd tea.Cmd
	if leaving == layout.RegionAutomations && m.ring.Active() != leaving {
		if err := m.saveContentPaneState(); err != nil {
			cmd = m.handleError(err)
		}
	}
	m.relayout()
	return cmd
}

func (m *home) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		func() tea.Msg {
			time.Sleep(100 * time.Millisecond)
			return previewTickMsg{}
		},
		tickUpdatePRInfoCmd,
		tickRefreshExternalCmd,
	)
}

func (m *home) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case hideErrMsg:
		m.errBox.Clear()
	case previewTickMsg:
		// While the user is attached to an instance, the preview/terminal
		// panes are hidden behind the tmux client they detached into. Running
		// selectionChanged here would dispatch refreshPanesCmd (two tmux
		// capture-pane shell-outs against the shared tmux server) every
		// 100ms — exactly the contention that produced the 44s detach hang
		// in #598. Keep the tick alive so the first post-detach iteration
		// fires within ~100ms of clearing the flag, but skip the work.
		var cmd tea.Cmd
		if !m.attached.Load() {
			cmd = m.selectionChanged()
		}
		return m, tea.Batch(
			cmd,
			func() tea.Msg {
				time.Sleep(100 * time.Millisecond)
				return previewTickMsg{}
			},
		)
	case keyupMsg:
		m.menu.ClearKeydownIfMatch(msg.name)
		return m, nil
	case tickUpdatePRInfoMessage:
		// Lazy: only refresh PR info for the currently selected instance. Other
		// instances keep whatever was last fetched (or restored from disk) —
		// they'll refresh when the user actually looks at them. The fetch
		// itself runs in a background goroutine so the UI stays responsive.
		// Skip the fetch while attached: gh pr view is a network call and
		// the sidebar PR badge is hidden behind the tmux client (#598).
		if m.attached.Load() {
			return m, tickUpdatePRInfoCmd
		}
		selected := m.sidebar.GetSelectedInstance()
		return m, tea.Batch(tickUpdatePRInfoCmd, fetchPRInfoCmd(selected, true))
	case prInfoUpdatedMsg:
		detachTraceMark("prInfoUpdatedMsg-handler-entry")
		// msg.instance is the pointer captured when the async gh fetch kicked
		// off. A background refresh can swap it out of the sidebar while the
		// fetch is in flight — RemoveInstanceByTitle + a rebuilt
		// FromInstanceData pointer (#765) — orphaning it. Writing the result to
		// that orphan loses the update from the UI and from persisted state.
		// Re-resolve the live instance by title (mirroring the #808 fix to
		// instanceStartedMsg) so the update lands on whatever instance now
		// represents this session. If the session is gone entirely, drop the
		// stale fetch result (#862).
		target := msg.instance
		if !m.store.ContainsInstance(target) {
			target = m.store.GetInstanceByTitle(msg.instance.Title)
		}
		if target == nil {
			return m, nil
		}
		// The title-only re-resolution above can land on a different session
		// than the fetch was kicked off for: a user can kill the original
		// instance and recreate one with the same title on a *different*
		// branch while the gh fetch is in flight (#921). PR info is
		// branch-specific, so applying a branch-X result to a branch-Y
		// instance would show the wrong PR badge and persist it. Gate the
		// apply on the captured branch still matching the resolved target.
		if target.GetBranch() != msg.branch {
			return m, nil
		}
		if msg.err != nil {
			log.WarningLog.Printf("PR info fetch failed for %q: %v", msg.instance.Title, msg.err)
			// Mark as fetched anyway so we don't thrash retries on every
			// selection change when the network is unreachable.
			target.MarkPRInfoFetched()
			return m, nil
		}
		// Apply the fetched PR info to the in-memory instance immediately so the
		// sidebar badge updates without waiting on the daemon round-trip. The
		// gh-pr-view fetch stays TUI-side (#921, per-selection, debounced); only
		// the persisted WRITE goes to the daemon (#960) — the single writer owns
		// it, so the TUI never originates an instances.json write (#959).
		target.SetPRInfo(msg.info)
		var prData session.PRInfoData
		if msg.info != nil {
			prData = session.PRInfoData{
				Number: msg.info.Number,
				Title:  msg.info.Title,
				URL:    msg.info.URL,
				State:  msg.info.State,
			}
		}
		saveStart := time.Now()
		if err := setPRInfoThroughDaemon(target.Title, m.repoID, prData); err != nil {
			// In-memory update already applied for the UI; surface the persist
			// failure but don't drop the badge. callDaemon already waited out the
			// daemon warm-up window (#829), so a residual error is a real failure,
			// not version skew — there is no TUI write path to fall back to.
			log.WarningLog.Printf("failed to persist PR info for %q via daemon: %v", target.Title, err)
		}
		detachTrace(saveStart, "prInfoUpdatedMsg-setPRInfoThroughDaemon-returned")
		return m, nil
	case tickRefreshExternalMessage:
		// The tick only PACES the loop; the actual Snapshot fetch runs off the
		// event loop (it can block while a daemon warms up — #829) and comes back
		// as snapshotFetchedMsg, which reconciles and re-arms the tick. Deliberately
		// no reschedule here: re-arming from snapshotFetchedMsg keeps exactly one
		// fetch in flight.
		detachTraceMark("tickRefreshExternalMessage-handler-entry")
		return m, m.fetchSnapshotCmd()
	case snapshotFetchedMsg:
		// Reconcile the sidebar to the daemon's authoritative snapshot on the
		// event loop (sidebar mutation must stay here — #682), then re-arm the
		// pacing tick. No TUI SaveInstances: the snapshot is a read-only mirror of
		// state the daemon already persists, so there is nothing for the TUI to
		// write back — and writing its whole-list view back is exactly the clobber
		// the single-writer model retires (#959/#960).
		detachTraceMark("snapshotFetchedMsg-handler-entry")
		tickStart := time.Now()
		changed := m.handleSnapshot(msg)
		detachTrace(tickStart, "snapshotFetchedMsg-reconcile-returned")
		cmds := []tea.Cmd{tickRefreshExternalCmd}
		if changed {
			cmds = append(cmds, m.selectionChanged())
		}
		return m, tea.Batch(cmds...)
	case tea.MouseMsg:
		if msg.Action == tea.MouseActionPress {
			if msg.Button == tea.MouseButtonWheelDown || msg.Button == tea.MouseButtonWheelUp {
				// The wheel scrolls whatever the focus ring points at: the
				// expanded automations strip scrolls its own task list
				// independent of the sidebar selection (#524); everywhere
				// else it scrolls the workspace pane, which needs a bound
				// instance. Hit-tested wheel routing is #1024 PR 6.
				if m.ring.Active() == layout.RegionAutomations {
					switch msg.Button {
					case tea.MouseButtonWheelUp:
						m.automations.ScrollUp()
					case tea.MouseButtonWheelDown:
						m.automations.ScrollDown()
					}
					return m, nil
				}
				if m.store.GetSelectedInstance() == nil {
					return m, nil
				}
				switch msg.Button {
				case tea.MouseButtonWheelUp:
					m.paneA.ScrollUp()
				case tea.MouseButtonWheelDown:
					m.paneA.ScrollDown()
				}
			}
		}
		return m, nil
	case tea.KeyMsg:
		return m.handleKeyPress(msg)
	case tea.WindowSizeMsg:
		m.updateHandleWindowSizeEvent(msg)
		return m, nil
	case error:
		return m, m.handleError(msg)
	case runOnEventLoopMsg:
		// Invoked only by test harnesses — lets them read model state from
		// the tea goroutine so it doesn't race with Update mutations.
		msg.fn(m)
		close(msg.done)
		return m, nil
	case instanceChangedMsg:
		return m, m.selectionChanged()
	case startKillMsg:
		// The row was already flipped to Deleting synchronously by the kill
		// confirmation; dispatch the slow teardown off the event loop (#844).
		return m, m.killInstanceCmd(msg.title)
	case instanceKilledMsg:
		return m.handleInstanceKilled(msg)
	case repaintAfterDetachMsg:
		// Trigger an immediate repaint with whatever content is already
		// cached on the panes (rendered when bubbletea's main loop calls
		// View() after this Update returns), and kick off the async
		// preview/terminal refresh so fresh content lands within a few
		// milliseconds. selectionChanged() also dispatches the captures
		// off the event loop, so this handler returns instantly.
		detachTraceMark("repaintAfterDetachMsg-handler-entry")
		cmd := m.selectionChanged()
		detachTraceMark("repaintAfterDetachMsg-handler-exit")
		// The watchdog armed in attachOverlayCallback is ended when the
		// post-detach paint completes (panesRefreshedMsg). That msg is only
		// emitted by refreshPanesCmd, which selectionChanged dispatches only
		// when an instance row is selected. When the selection has fallen back
		// to a section header — e.g. the only instance was removed while the
		// user was attached — selectionChanged returns nil, no
		// panesRefreshedMsg ever arrives, and the watchdog would fire a
		// spurious goroutine dump after slowDetachThreshold even though there
		// were no panes to refresh. Cancel it here: a nil cmd means the detach
		// already completed everything it was going to do (#683).
		if cmd == nil {
			endDetachWatchdog()
		}
		return m, cmd
	case panesRefreshedMsg:
		// The refresh cmd already wrote captured content into the mutex-
		// guarded pane state. Returning here causes bubbletea to invoke
		// View() again, which renders the now-fresh content.
		detachTraceMark("panesRefreshedMsg-handler-entry")
		// End the slow-detach watchdog: the post-detach paint completed,
		// so any in-flight watchdog should stop. No-op when no detach is
		// currently in flight.
		endDetachWatchdog()
		return m, nil
	case instanceStartedMsg:
		// The user may have navigated elsewhere while the instance was
		// starting. Don't yank their selection or pop a modal onto them.
		userStillWatching := m.state == stateDefault && m.sidebar.GetSelectedInstance() == msg.instance

		if msg.err != nil {
			// Remove the *specific* instance that failed, by title. The old
			// code did m.sidebar.Kill() after SelectInstance(msg.instance),
			// which would have killed whatever the user was currently
			// looking at if we skipped the re-select. Backend.Start already
			// cleans up tmux/worktree on failure (see backend_local.go
			// defer), so there is nothing more to tear down here.
			//
			// Capture the user's current selection before the remove so we
			// can restore it afterwards. RemoveInstanceByTitle shifts the
			// instances slice, which can bump selectedIdx onto a different
			// row when the removed instance preceded the selected one.
			priorSelection := m.sidebar.GetSelectedInstance()
			m.store.RemoveInstanceByTitle(msg.instance.Title)
			if priorSelection != nil && priorSelection != msg.instance {
				m.sidebar.SelectInstance(priorSelection)
			}
			// No instances.json write: the failed instance was a Loading
			// placeholder, which is never persisted, and the daemon is the sole
			// writer (#960 PR 4). Removing the in-memory row is the whole cleanup.

			return m, tea.Batch(m.handleError(msg.err), m.selectionChanged())
		}

		started := msg.instance
		if msg.started != nil {
			started = msg.started
		}
		// A successful Replace now maintains the repos map itself (#971: it
		// decrements the outgoing row's repo and registers the replacement's), so
		// the explicit RegisterRepoForInstance below must be skipped on that path or
		// it would double-count. The other paths (in-place start of an already-present
		// Loading row, or a fresh add) need the explicit registration: the placeholder
		// was added before it was started, so its AddInstance finalize registered
		// nothing (RepoName fails until started), and no presence-changing primitive
		// runs here to register it now.
		swapped := false
		if started != msg.instance {
			if m.store.ReplaceInstance(msg.instance, started) {
				swapped = true
			} else if !m.store.ContainsInstance(started) {
				// The Loading placeholder may have been swapped for a
				// disk-built copy of this same session by a background sync
				// while the start RPC was in flight; both Replace and
				// Contains are pointer-based and miss it. Adding
				// unconditionally would leave two sidebar rows — and two
				// persisted records — for one session (#808), so replace any
				// same-title row instead.
				if m.store.ReplaceInstanceByTitle(started.Title, started) {
					swapped = true
				} else {
					m.store.AddInstance(started)
				}
			}
		} else if !m.store.ContainsInstance(started) {
			m.store.AddInstance(started)
		}

		started.SetStatus(session.Running)
		if !swapped && !started.IsRemote() {
			m.store.RegisterRepoForInstance(started)
		}
		started.SetAutoYes(m.autoYes)

		if !userStillWatching {
			// User moved on — update status silently and keep their current
			// focus. The instance flips from Loading to Running in the
			// sidebar on its own.
			return m, tea.Batch(tea.WindowSize(), m.selectionChanged())
		}

		m.menu.SetState(ui.StateDefault)
		m.showHelpScreen(helpStart(started), nil)

		return m, tea.Batch(tea.WindowSize(), m.selectionChanged())
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *home) handleQuit() (tea.Model, tea.Cmd) {
	// Save any dirty task/hooks state. On failure the panes were reloaded to
	// match disk; abort the quit and surface the error so the user sees the
	// dropped edit instead of losing it silently on the way out.
	if err := m.saveContentPaneState(); err != nil {
		return m, m.handleError(err)
	}

	// No instances.json write on quit: the daemon is the sole writer (#960 PR 4)
	// and every session/tab mutation already persisted through it as it
	// happened. The TUI holds no authoritative instance state to flush.
	//
	// Do NOT tear down tab sessions on quit: as of #930 PR 2 each instance owns
	// its agent and shell tab tmux sessions, and they must survive an af restart
	// so the user reconnects to them on next launch (Sachin's persistence
	// requirement). Killing an instance still tears its tabs down via
	// LocalBackend.Kill.
	return m, tea.Quit
}

// saveContentPaneState persists any changes from the hooks/task panes and
// returns a non-nil error if any persist operation failed. Both panes'
// failures are accumulated so neither is dropped when both are dirty at once.
//
// Recovery semantics on a hooks-save failure (#1001): we leave the HooksPane
// dirty and deliberately do NOT reload it from disk. The edit the user is
// trying to save lives only in memory, so reloading would discard the very
// edit they care about — the silent data loss this fix exists to prevent.
// Returning the error lets callers (handleQuit / focus release) abort the
// destructive action and surface it via handleError; the dirty pane preserves
// the edit so the user can retry from where they left off.
//
// Recovery semantics on a task-save failure (#934): we reload BOTH the sidebar
// and the TaskPane from disk so the two panes always agree and always reflect
// the committed on-disk state — never a mix of stale in-memory edits in one
// pane and disk state in the other. Reloading clears the TaskPane's dirty flag,
// which means a failed edit is discarded rather than left dangling; we
// therefore return the error so callers surface it (via handleError) and the
// dropped edit is never silent. We deliberately do NOT keep dirty=true for an
// in-place retry: after the reload the in-memory edits are gone, so a lingering
// dirty flag would point at nothing. The user re-applies the edit from a
// known-consistent state instead.
func (m *home) saveContentPaneState() error {
	// Accumulate failures across both panes so a hooks error and a task error
	// can never clobber one another (#1001).
	var saveErr error

	hp := m.hooksPane
	if hp.IsDirty() {
		// Hook edits are written to the in-repo .agent-factory/config.json —
		// the canonical location for post_worktree_commands since #800. The
		// legacy ~/.agent-factory/repos/<id>/config.json stays untouched as a
		// read-only fallback; the saved in-repo key (even when emptied)
		// shadows it.
		if err := saveInRepoPostWorktreeCommandsFn(m.repoRoot, hp.GetCommands()); err != nil {
			log.ErrorLog.Printf("failed to save hooks: %v", err)
			// Surface the failure instead of swallowing it (#1001): callers
			// abort the quit / focus release and show the error overlay rather
			// than silently dropping the edit. The HooksPane stays dirty (see
			// the recovery note above) so the in-memory edit survives for retry.
			saveErr = errors.Join(saveErr, fmt.Errorf("failed to save hooks: %w", err))
		} else {
			m.store.SetHookCount(len(hp.GetCommands()))
		}
	}

	sp := m.automations.TaskPane()
	if !sp.IsDirty() {
		return saveErr
	}

	// Collect every persist failure instead of swallowing them: a partial
	// failure must still surface so the user knows their edit didn't fully
	// land (matches api/tasks.go, which propagates these errors).
	for _, tsk := range sp.GetTasks() {
		if err := task.UpdateTask(tsk); err != nil {
			log.ErrorLog.Printf("failed to update task: %v", err)
			saveErr = errors.Join(saveErr, fmt.Errorf("failed to save task %q: %w", tsk.Name, err))
		}
	}
	for _, tsk := range sp.ConsumeDeleted() {
		if err := task.RemoveTask(tsk.ID); err != nil {
			log.ErrorLog.Printf("failed to remove task: %v", err)
			saveErr = errors.Join(saveErr, fmt.Errorf("failed to remove task %q: %w", tsk.Name, err))
		}
	}
	// Schedules live in the daemon (#782): a single reload poke after
	// the batched writes brings its cron entries in sync.
	reloadDaemonTaskSchedules()
	// Reload BOTH panes from disk so the TaskPane and sidebar can never diverge
	// (#934): whatever actually committed, both panes now show it.
	tasks, err := task.LoadTasksForCurrentRepo()
	if err == nil {
		m.store.SetTasks(tasks)
		sp.SetTasks(tasks)
	} else {
		saveErr = errors.Join(saveErr, fmt.Errorf("failed to reload tasks after save: %w", err))
	}
	return saveErr
}

// reloadDaemonTaskSchedulesFn is indirected so TUI tests can observe the poke
// without dialing (or spawning) a real daemon.
var reloadDaemonTaskSchedulesFn = daemon.ReloadTasks

// saveInRepoPostWorktreeCommandsFn is indirected so TUI tests can force a
// hooks-save failure deterministically — without relying on filesystem
// permission tricks that a root test runner would bypass (#1001).
var saveInRepoPostWorktreeCommandsFn = config.SaveInRepoPostWorktreeCommands

// reloadDaemonTaskSchedules asks the daemon to re-read tasks.json after a
// TUI-side task edit. Best-effort: the daemon reloads all tasks at every
// start, so a failed poke only delays the change until then.
func reloadDaemonTaskSchedules() {
	if err := reloadDaemonTaskSchedulesFn(); err != nil {
		log.WarningLog.Printf("task change saved, but the daemon schedule reload failed (the change applies at next daemon start): %v", err)
	}
}

// handleTaskCreate processes a pending task creation from the inline form.
func (m *home) handleTaskCreate() tea.Cmd {
	sp := m.automations.TaskPane()
	name, prompt, cronExpr, watchCmd, targetSession, projectPath, program := sp.ConsumePendingCreate()

	if name == "" {
		return m.handleError(fmt.Errorf("task name is required"))
	}
	// Re-validate the trigger contract behind the form (#782): exactly one of
	// cron / watch cmd, and cron tasks need a prompt — there is no event line
	// to fall back to. Mirrors `af tasks add` (api/tasks.go).
	hasCron := cronExpr != ""
	hasWatch := watchCmd != ""
	if hasCron == hasWatch {
		return m.handleError(fmt.Errorf("exactly one of cron or watch cmd is required"))
	}
	if hasCron {
		if strings.TrimSpace(prompt) == "" {
			return m.handleError(fmt.Errorf("prompt must be non-empty"))
		}
		if err := task.ValidateCronExpr(cronExpr); err != nil {
			return m.handleError(fmt.Errorf("invalid cron: %v", err))
		}
	}
	// Expand a leading ~ before resolving to absolute — filepath.Abs does not
	// expand "~", so "~/project" would otherwise become "<cwd>/~/project"
	// (#924). validateForm already normalized the field, so this is idempotent.
	absPath, err := filepath.Abs(config.ExpandTilde(projectPath))
	if err != nil {
		return m.handleError(fmt.Errorf("invalid path: %v", err))
	}
	if program == "" {
		program = m.program
	}
	id, err := task.GenerateID()
	if err != nil {
		return m.handleError(fmt.Errorf("failed to generate task id: %v", err))
	}
	t := task.Task{
		ID:            id,
		Name:          name,
		Prompt:        prompt,
		CronExpr:      cronExpr,
		WatchCmd:      watchCmd,
		TargetSession: targetSession,
		ProjectPath:   absPath,
		Program:       program,
		Enabled:       true,
		CreatedAt:     time.Now(),
	}
	if err := task.AddTask(t); err != nil {
		return m.handleError(fmt.Errorf("failed to save task: %v", err))
	}
	reloadDaemonTaskSchedules()
	// Refresh sidebar and task pane
	tasks, err := task.LoadTasksForCurrentRepo()
	if err == nil {
		m.store.SetTasks(tasks)
		sp.SetTasks(tasks)
	}
	return nil
}

// handleTaskTrigger immediately spawns an instance for the selected task.
func (m *home) handleTaskTrigger() tea.Cmd {
	sp := m.automations.TaskPane()
	tsk := sp.ConsumePendingTrigger()
	if tsk == nil {
		return m.handleError(fmt.Errorf("no task selected"))
	}

	// Watch tasks fire from their watch command's stdout; a manual trigger
	// has no event line to render the prompt with. Mirrors daemon.RunTask.
	if tsk.IsWatch() {
		return m.handleError(fmt.Errorf("task %q is a watch task; it fires when its watch command emits output", task.TaskRunBaseTitle(*tsk)))
	}

	repo, err := config.RepoFromPath(tsk.ProjectPath)
	if err != nil {
		return m.handleError(fmt.Errorf("failed to resolve repo for task path: %w", err))
	}
	title, err := task.NextTaskRunTitle(repo.ID, tsk.ProjectPath, task.TaskRunBaseTitle(*tsk), tsk.Program)
	if err != nil {
		return m.handleError(fmt.Errorf("failed to allocate task run title: %w", err))
	}

	instance, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    tsk.ProjectPath,
		Program: tsk.Program,
	})
	if err != nil {
		return m.handleError(fmt.Errorf("failed to create instance: %w", err))
	}

	m.store.AddInstance(instance)
	m.sidebar.SetSelectedInstance(m.store.NumInstances() - 1)
	instance.SetStatus(session.Loading)
	m.menu.SetState(ui.StateDefault)
	// The run lands as a new session: move focus to the tree so the user is
	// looking at the spawned instance, not the strip.
	m.focusRegion(layout.RegionTree)

	prompt := tsk.Prompt
	taskID := tsk.ID
	// Capture the start seam on the event loop before the goroutine reads it, so a
	// concurrent test-seam swap can't race the read (#960 PR 4 race-fix class).
	start := startSessionThroughDaemon
	startCmd := func() tea.Msg {
		started, err := start(instance, sessionStartRequest{
			Title:    instance.Title,
			RepoPath: instance.Path,
			Program:  instance.Program,
			Prompt:   prompt,
			AutoYes:  m.autoYes,
		})
		if err != nil {
			return instanceStartedMsg{instance: instance, err: err}
		}

		// Update task last run status. UpdateTaskStatus skips Program enum
		// validation so legacy task records (pre-#658) still receive status
		// bumps; see #664.
		now := time.Now()
		if err := task.UpdateTaskStatus(taskID, &now, "triggered"); err != nil {
			log.ErrorLog.Printf("failed to update task status: %v", err)
		}

		return instanceStartedMsg{instance: instance, started: started, err: nil}
	}

	return tea.Batch(tea.WindowSize(), m.selectionChanged(), startCmd)
}

func (m *home) handleMenuHighlighting(msg tea.KeyMsg) (cmd tea.Cmd, returnEarly bool) {
	if m.keySent {
		m.keySent = false
		return nil, false
	}
	// While naming a new instance the menu only shows the submit-name (enter)
	// and change-program (tab) options, so those two keys are the only ones
	// that should drive the highlight animation. Every other key is naming
	// text and must pass through untouched — matching on msg.String() rather
	// than GlobalKeyStringsMap also keeps "o" (a KeyEnter alias) usable as a
	// literal name character. See #691: stateNew used to sit in the
	// early-return filter below, which made this remapping unreachable.
	if m.state == stateNew {
		var name keys.KeyName
		switch msg.String() {
		case "enter":
			name = keys.KeySubmitName
		case "tab":
			name = keys.KeyChangeProgram
		default:
			return nil, false
		}
		m.keySent = true
		return tea.Batch(
			func() tea.Msg { return msg },
			m.keydownCallback(name)), true
	}
	if m.state == stateHelp || m.state == stateConfirm ||
		m.state == stateSearch || m.state == stateSelectProgram ||
		m.state == stateHooks {
		return nil, false
	}
	// Don't highlight when the automations strip has the keyboard
	if m.ring.Active() == layout.RegionAutomations {
		return nil, false
	}
	name, ok := keys.GlobalKeyStringsMap[msg.String()]
	if !ok {
		return nil, false
	}

	if name == keys.KeyShiftDown || name == keys.KeyShiftUp {
		return nil, false
	}
	// Skip sidebar nav keys from menu highlighting
	if name == keys.KeyLeft || name == keys.KeyRight || name == keys.KeyNextSection || name == keys.KeyPrevSection {
		return nil, false
	}

	m.keySent = true
	return tea.Batch(
		func() tea.Msg { return msg },
		m.keydownCallback(name)), true
}

func (m *home) handleKeyPress(msg tea.KeyMsg) (mod tea.Model, cmd tea.Cmd) {
	cmd, returnEarly := m.handleMenuHighlighting(msg)
	if returnEarly {
		return m, cmd
	}

	// Dispatch to state-specific handlers
	switch m.state {
	case stateHelp:
		return m.handleHelpState(msg)
	case stateNew:
		return m.handleStateNew(msg)
	case stateConfirm:
		return m.handleStateConfirm(msg)
	case stateSearch:
		return m.handleStateSearch(msg)
	case stateSelectProgram:
		return m.handleStateSelectProgram(msg)
	case stateHooks:
		return m.handleStateHooks(msg)
	}

	// The focused automations strip owns the keyboard (its task manager);
	// Tab/Shift-Tab and quit keys deliberately fall through.
	if mod, cmd, consumed := m.handleAutomationsFocus(msg); consumed {
		return mod, cmd
	}

	// Exit scrolling mode when ESC is pressed
	if msg.Type == tea.KeyEsc {
		if m.paneA.IsInScrollMode() {
			selected := m.sidebar.GetSelectedInstance()
			if err := m.paneA.ResetToNormalMode(selected); err != nil {
				return m, m.handleError(err)
			}
			return m, m.selectionChanged()
		}
	}

	// Handle quit commands
	if msg.String() == "ctrl+c" || msg.String() == "q" {
		return m.handleQuit()
	}

	// Number-key tab jump (1-9): jump directly to that tab of the selected
	// instance (#930 PR 4). Handled here, before the GlobalKeyStringsMap
	// lookup, because digits are dispatched manually rather than mapped to a
	// KeyName. Digits typed into the focused automations strip were consumed
	// above and pass through untouched.
	if len(msg.Runes) == 1 {
		if r := msg.Runes[0]; r >= '1' && r <= '9' {
			return m.handleTabJump(int(r - '0'))
		}
	}

	name, ok := keys.GlobalKeyStringsMap[msg.String()]
	if !ok {
		return m, nil
	}

	return m.handleDefaultKeyPress(msg, name)
}

// remoteDetachTerminalReassert re-establishes the terminal modes bubbletea
// set at startup (see Run: WithAltScreen + WithMouseCellMotion, plus the
// hidden cursor and the bracketed paste bubbletea enables by default) after a
// remote attach stream has scribbled over them. The hook backend's neutral
// restore (session.hookAttachTerminalRestore) hands the terminal back on the
// MAIN screen with the cursor visible and all reporting modes off — correct
// for the CLI attach path, but not the state this TUI runs in.
//
// Hand-rolled rather than bubbletea-native, for two reasons (#845):
//   - bubbletea cannot re-assert state it believes is already set: the
//     renderer's enterAltScreen() is a no-op while its altScreenActive
//     bookkeeping is true, and that bookkeeping never saw the remote PTY's
//     writes. An ExitAltScreen/EnterAltScreen dance defeats the guard, but
//     runs as queued program msgs racing the post-detach msg backlog — diff
//     frames could land on the main screen first, leaking TUI content into
//     the shell's scrollback.
//   - Writing synchronously here, while the Update goroutine is still blocked
//     inside the onDismiss callback, guarantees the terminal is back in the
//     state the renderer assumes before it can emit a single frame.
//
// The renderer's diff cache is still stale after this (it thinks the
// pre-attach frame is on screen; the 1049h re-entry cleared it), so the
// caller follows up with tea.ClearScreen — the native lever for "invalidate
// the cache and repaint everything".
const remoteDetachTerminalReassert = "" +
	"\x1b[?1049h" + // re-enter the alt screen (terminal clears it)
	"\x1b[?25l" + // bubbletea hid the cursor at startup; re-hide it
	"\x1b[?1002h\x1b[?1006h" + // WithMouseCellMotion + SGR encoding
	"\x1b[?2004h" // bracketed paste (bubbletea default-on)

// remoteDetachResetWriter is where remoteDetachTerminalReassert is written —
// the real terminal in production, swappable so tests can capture it.
var remoteDetachResetWriter io.Writer = os.Stdout

// attachOverlayCallbackFn is the indirection handleEnter reaches
// attachOverlayCallback through. Production points it at the method; tests swap
// it to substitute a hermetic attach func (no real tmux client or remote
// terminal_cmd PTY) while preserving the real `remote` argument the call site
// computed. That keeps the call-site decision exercised end to end — the #889
// regression is that the terminal-tab site passed a hardcoded false instead of
// selected.IsRemote(), which can only be caught by a test that drives the real
// handleEnter and observes the post-detach reset keyed off that argument.
var attachOverlayCallbackFn = (*home).attachOverlayCallback

// attachOverlayCallback runs the attach-overlay onDismiss lifecycle: emits
// the detach-trace markers, invokes attach, arms the attached flag for the
// duration of `<-ch`, then returns the tea.Cmd to emit the
// repaintAfterDetachMsg{}. Returns nil when attach itself fails so the
// callback can be passed directly to showHelpScreen's onDismiss.
//
// remote selects the post-detach terminal handling. A local tmux detach
// leaves the terminal exactly as the TUI expects (the long-lived tmux client
// never replays its setup/teardown sequences across attach cycles), so the
// flow is the plain repaint it has always been. A remote detach hands the
// terminal back in the neutral state described on
// remoteDetachTerminalReassert, so the TUI's modes are re-asserted before the
// event loop resumes, and the repaint is preceded by tea.ClearScreen (#845).
//
// The defer on m.attached.Store(false) is load-bearing: it guarantees the
// flag clears even if `<-ch` is woken by an abnormal close or a panic
// further down the stack. Leaving the flag stuck at true would silently
// stall the metadata tick, preview refresh, and PR info fetcher until the
// next process restart — exactly the kind of regression #598 wants to
// avoid creating while fixing the original hang.
//
// Extracted so the attach call-sites (handleEnter sidebar, handleEnter
// terminal-tab) all funnel through one place — and so the pause-while-attached
// gating + the flag-clears-on-error path are testable without spinning up
// real tmux.
func (m *home) attachOverlayCallback(label, traceSuffix string, remote bool, attach func() (chan struct{}, error)) tea.Cmd {
	detachTraceMark(label + "-onDismiss-entry" + traceSuffix)
	ch, err := attach()
	if err != nil {
		log.ErrorLog.Printf("failed to attach (%s): %v", label+traceSuffix, err)
		return nil
	}
	m.attached.Store(true)
	defer m.attached.Store(false)
	// <-ch blocks for as long as the user is attached. Mark the boundary so
	// post-detach elapsed times in the trace are measured from when the user
	// actually returned to the UI, not from when the attach started.
	detachTraceMark(label + "-blocking-on-<-ch" + traceSuffix)
	<-ch
	detachStart := time.Now()
	detachTraceMark(label + "-<-ch-unblocked" + traceSuffix)
	m.state = stateDefault
	// Arm the slow-detach watchdog: if the post-detach paint
	// (panesRefreshedMsg) does not arrive within slowDetachThreshold, a
	// goroutine dump is appended to detach-slow.log so we can see which
	// goroutine is blocked.
	beginDetachWatchdog(label + traceSuffix)
	repaintCmd := func() tea.Msg {
		detachTrace(detachStart, label+"-repaintAfterDetachMsg-emitted")
		return repaintAfterDetachMsg{}
	}
	if remote {
		// The hook backend wrote its neutral restore before closing ch, so
		// this lands strictly after it. The Update goroutine is still blocked
		// in this callback, so no renderer write can interleave (#845).
		_, _ = io.WriteString(remoteDetachResetWriter, remoteDetachTerminalReassert)
		// ClearScreen first so the renderer's stale diff cache is invalidated
		// before the repaint flow runs; then the usual repaintAfterDetachMsg
		// path, watchdog semantics (#683) included.
		return tea.Sequence(tea.ClearScreen, repaintCmd)
	}
	return repaintCmd
}

// selectionChanged updates the workspace pane binding and menu based on the
// sidebar selection. The preview/terminal tmux captures are dispatched via a
// tea.Cmd (goroutine) rather than run synchronously: each call shells out to
// `tmux capture-pane` (~3–5ms locally), and on the bubbletea Update goroutine
// that cost compounded — every previewTickMsg (100ms) blocked the event loop,
// and the first paint after detach paid the full cost on top of waiting up
// to a full tick cycle for the next msg (#579, #559 sibling). The TabPane
// guards its captured state with a mutex so the goroutine can mutate it while
// View() reads it. Synchronous fields touched here (selection binding, menu
// state) stay on the event loop.
func (m *home) selectionChanged() tea.Cmd {
	selectionStart := time.Now()
	detachTraceMark("selectionChanged-entry")
	sel := m.sidebar.GetSelection()

	// While attached, the workspace is hidden behind the tmux client and the
	// panes will be repainted by repaintAfterDetachMsg as soon as the user
	// detaches. Skip the refresh + PR fetch dispatches so they don't queue
	// capture-pane / gh pr view work behind the user's detach key (#598). The
	// synchronous mutations (binding, menu state) still run so sidebar nav
	// that happens between attach failures is consistent.
	attachedNow := m.attached.Load()

	var prFetch tea.Cmd
	var refreshCmd tea.Cmd
	if sel.Kind == ui.SectionInstances && !sel.IsHeader {
		selected := m.sidebar.GetSelectedInstance()
		// Bind the workspace pane to the cursor's instance — the store's
		// display binding replaces the pre-#1024 TabbedWindow.instance pointer,
		// including the nil case — then re-clamp the active tab index against
		// the new instance's tab count.
		m.store.SetSelectedInstance(selected)
		m.paneA.ClampActiveTab()
		m.menu.SetInstance(selected)
		// The tree cursor drives the active tab too (landing on a tab row
		// selects that tab — #1024 PR 3), so mirror it into the menu here, not
		// just in the explicit tab-jump handlers.
		m.menu.SetActiveTab(m.paneA.GetActiveTab())
		if !attachedNow {
			refreshCmd = refreshPanesCmd(m.paneA, selected)
		}
		detachTrace(selectionStart, "selectionChanged-instance-branch-built-cmds")
		// Lazily refresh PR info when the user lands on an instance that
		// hasn't been fetched recently. fetchPRInfoCmd is a no-op when the
		// data is still fresh, so rapid Up/Down navigation doesn't hammer gh.
		if !attachedNow && selected != nil && selected.Started() {
			prFetch = fetchPRInfoCmd(selected, false)
		}
	} else {
		// Header row: the workspace keeps rendering the sticky display
		// selection while it is still live, and the menu drops the
		// instance-specific hints.
		m.menu.SetInstance(nil)
		selected := m.store.GetSelectedInstance()
		if selected != nil && !m.store.ContainsInstance(selected) {
			// The sticky binding dangles — its instance was removed (e.g. the
			// last instance killed while attached). Drop it so the pane can't
			// keep showing a dead session's capture.
			m.store.SetSelectedInstance(nil)
			selected = nil
		}
		if selected == nil {
			// Reset the pane to the nil-instance fallback synchronously:
			// UpdateContent(nil) is a mutex-guarded string write, no tmux
			// shell-out, so it is safe on the event loop — and deliberately
			// NOT a refresh cmd, preserving the #683 contract that a header
			// selection with nothing to refresh returns a nil cmd (the
			// repaint handler ends the detach watchdog off that nil).
			if err := m.paneA.UpdateContent(nil); err != nil {
				log.WarningLog.Printf("UpdateContent(nil) failed: %v", err)
			}
		} else if !attachedNow {
			refreshCmd = refreshPanesCmd(m.paneA, selected)
		}
	}

	return tea.Batch(prFetch, refreshCmd)
}

// panesRefreshedMsg signals that the off-loop tab capture finished. The msg
// itself carries no payload — bubbletea calls View() after every Update return
// regardless of the msg type, and TabPane already published the captured
// content into its own mutex-guarded state inside the goroutine. Sending the
// msg back is what actually wakes the event loop so View() runs against the
// fresh content.
type panesRefreshedMsg struct{}

// refreshPanesCmd runs the active tab's capture off the bubbletea Update
// goroutine. It shells out to `tmux capture-pane` (~3–5ms locally), which
// previously blocked the event loop on every previewTickMsg (every 100ms) and
// on every post-detach repaint. TabPane serialises its capture writes against
// String() reads with an internal mutex, so the goroutine can mutate the
// captured content concurrently with the renderer (#579).
func refreshPanesCmd(tw *ui.TabbedWindow, selected *session.Instance) tea.Cmd {
	return func() tea.Msg {
		cmdStart := time.Now()
		detachTraceMark("refreshPanesCmd-goroutine-entry")
		if err := tw.UpdateContent(selected); err != nil {
			log.WarningLog.Printf("UpdateContent failed: %v", err)
		}
		detachTrace(cmdStart, "refreshPanesCmd-goroutine-exit")
		return panesRefreshedMsg{}
	}
}

// repaintAfterDetachMsg is dispatched by the attach goroutine immediately
// after `<-ch` unblocks. Without it the first post-detach paint waits up
// to ~100ms for the next previewTickMsg (the goroutine sets stateDefault
// but bubbletea has no event queued, so View() does not re-run). The
// handler hands the actual refresh off to a tea.Cmd so the tmux
// capture-pane calls don't block the event loop (#579).
type repaintAfterDetachMsg struct{}

type keyupMsg struct {
	name keys.KeyName
}

func (m *home) keydownCallback(name keys.KeyName) tea.Cmd {
	m.menu.Keydown(name)
	return func() tea.Msg {
		select {
		case <-m.ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
		return keyupMsg{name: name}
	}
}

type hideErrMsg struct{}
type previewTickMsg struct{}
type instanceChangedMsg struct{}

// startKillMsg is emitted by the kill confirmation action right after the
// target row has been marked Deleting on the event loop. Its handler
// dispatches killInstanceCmd, which runs the slow teardown in a background
// goroutine (#844).
type startKillMsg struct {
	title string
}

// instanceKilledMsg reports completion of an async kill. A nil err means the
// daemon tore the session down and deleted its record; a non-nil err means
// the session is still alive and the row must become retryable again.
type instanceKilledMsg struct {
	title string
	err   error
}

// runOnEventLoopMsg is a test-only primitive: when received by Update, it
// runs fn with the home pointer on the tea goroutine, then closes done.
// Production code never emits these — it exists purely so e2e tests can
// read home state without racing concurrent Update handlers.
type runOnEventLoopMsg struct {
	fn   func(*home)
	done chan struct{}
}

type instanceStartedMsg struct {
	instance *session.Instance
	started  *session.Instance
	err      error
}

func (m *home) handleError(err error) tea.Cmd {
	log.ErrorLog.Printf("%v", err)
	m.errBox.SetError(err)
	return func() tea.Msg {
		select {
		case <-m.ctx.Done():
		case <-time.After(3 * time.Second):
		}
		return hideErrMsg{}
	}
}

func (m *home) confirmAction(message string, action tea.Cmd) tea.Cmd {
	m.state = stateConfirm
	m.confirmationOverlay = overlay.NewConfirmationOverlay(message)
	m.confirmationOverlay.SetWidth(50)

	m.confirmationOverlay.OnConfirm = func() {
		m.state = stateDefault
		if action != nil {
			if msg := action(); msg != nil {
				if err, ok := msg.(error); ok {
					log.ErrorLog.Printf("confirmation action failed: %v", err)
					m.errBox.SetError(err)
				} else {
					// Stash non-error messages so handleStateConfirm can
					// forward them into the Bubble Tea event loop.
					m.pendingConfirmMsg = msg
				}
			}
		}
	}

	m.confirmationOverlay.OnCancel = func() {
		m.state = stateDefault
	}

	return nil
}

// View composes the workspace from the solved layout (#1024 PR 4): every pane
// renders exactly its rect, so the regions tile the full window with no
// padding math. Modal overlays composite on top exactly as before.
func (m *home) View() string {
	// Below the hard minimum no layout exists; render the banner alone.
	if m.lastLayout.Fallback {
		return ui.TerminalTooSmall(m.termWidth, m.termHeight)
	}

	top := lipgloss.JoinHorizontal(lipgloss.Top, m.sidebar.View(), m.paneA.View())
	rows := []string{top}
	if m.lastLayout.AutomationsVisible {
		rows = append(rows, m.automations.View())
	}
	rows = append(rows, m.statusBar.View())
	mainView := lipgloss.JoinVertical(lipgloss.Left, rows...)

	if m.state == stateHelp {
		if m.textOverlay == nil {
			log.ErrorLog.Printf("text overlay is nil")
		}
		return overlay.PlaceOverlay(0, 0, m.textOverlay.Render(), mainView, true)
	} else if m.state == stateConfirm {
		if m.confirmationOverlay == nil {
			log.ErrorLog.Printf("confirmation overlay is nil")
		}
		return overlay.PlaceOverlay(0, 0, m.confirmationOverlay.Render(), mainView, true)
	} else if m.state == stateSearch {
		if m.searchOverlay == nil {
			log.ErrorLog.Printf("search overlay is nil")
		}
		return overlay.PlaceOverlay(0, 0, m.searchOverlay.Render(), mainView, true)
	} else if m.state == stateSelectProgram {
		if m.selectionOverlay == nil {
			log.ErrorLog.Printf("selection overlay is nil")
		}
		return overlay.PlaceOverlay(0, 0, m.selectionOverlay.Render(), mainView, true)
	} else if m.state == stateHooks {
		return overlay.PlaceOverlay(0, 0, m.renderHooksOverlay(), mainView, true)
	}

	return mainView
}

// hooksOverlayStyle frames the hooks editor when it is hosted as an overlay
// (#1024 PR 4: hooks lost their persistent sidebar slot).
var hooksOverlayStyle = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(ui.AccentColor).
	Padding(1, 2)

func (m *home) renderHooksOverlay() string {
	return hooksOverlayStyle.Render(m.hooksPane.String())
}
