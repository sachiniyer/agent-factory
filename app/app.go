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
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
	"github.com/sachiniyer/agent-factory/ui/overlay"
	"github.com/sachiniyer/agent-factory/ui/store"
	"github.com/sachiniyer/agent-factory/ui/tree"
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
	// stateTasks is the state when the task manager (list + create/edit form)
	// overlay is open. The in-rail automations section shows only the compact
	// summary (#1087 play-test): the full manager gets a centered overlay so
	// its form is never clamped into the narrow rail.
	stateTasks
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
	// snapshotThroughDaemon in production; tests assign a fake directly. It
	// returns the full daemon.SnapshotResponse so the session list and the
	// delivery-failure alarms (#1238) arrive from one authoritative RPC — the
	// alarm is a field on the snapshot, not a side channel.
	snapshotFetcher func(repoID string) (daemon.SnapshotResponse, error)
	// pauseStatusPoll / resumeStatusPoll are the daemon poll-pause seams for the
	// attach heartbeat (#1160). PER-home fields, not package globals, for the
	// same reason as snapshotFetcher: the heartbeat reads the seam from an
	// off-loop goroutine, so a shared mutable global swapped by a test would race
	// that goroutine against a sibling test's swap under `go test -parallel
	// -race` (the #964 / #960-PR4 snapshot-fetcher race). Each home owns its
	// seams; the goroutine captures them into locals at spawn so it never touches
	// shared home state mid-flight. Default to pauseStatusPollThroughDaemon /
	// resumeStatusPollThroughDaemon in production; tests assign fakes directly.
	pauseStatusPoll  func(title, repoID string) error
	resumeStatusPoll func(title, repoID string) error
	// appConfig stores persistent application configuration
	appConfig *config.Config
	// appState stores persistent application state like seen help screens
	appState config.AppState
	// lastTUIViewState prevents preview ticks from rewriting unchanged state.
	lastTUIViewState    config.TUIRepoViewState
	hasLastTUIViewState bool

	// -- State --

	// state is the current discrete state of the application
	state state
	// quitting suppresses Bubble Tea's final graceful render after handleQuit has
	// started terminal teardown, so stale TUI chrome is not repainted on exit.
	quitting bool
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
	// The window is tiled by layout.Grid into the RFC §2.1 regions (as
	// revised by #1087/#1090): the left rail — the instances+tabs tree over
	// the bottom-aligned automations section, separated by a horizontal
	// rule — the full-height content pane A beside it, and the status bar
	// under everything. Each region is a layout.Pane that renders exactly its
	// rect; relayout() re-solves the grid and re-rects the panes.

	// grid is the single sizing authority; termWidth/termHeight the last
	// tea.WindowSizeMsg, so focus changes (which resize the automations
	// strip) can re-solve without waiting for a resize event.
	grid                  layout.Grid
	lastLayout            layout.Layout
	termWidth, termHeight int
	// ring is the focus ring: tree → open panes (in workspace order) →
	// automations (#1088). Tab/Shift-Tab cycle it; its pane entries are
	// rebuilt by relayout as panes open, close, and auto-hide, and regions
	// hidden by the degradation ladder are skipped.
	ring *layout.Ring
	// zones is the mouse hit-test registry (#1024 R4, RFC §2.5): View()
	// resets it every frame and each pane re-registers its interactive rects
	// while rendering, so the registry always mirrors what is actually on
	// screen. handleMouse resolves every tea.MouseMsg through it.
	zones *zones.Registry
	// lastClickZone/lastClickAt implement double-click detection: a second
	// press on the same zone within doubleClickInterval reads as a double
	// click. mouseClock is time.Now, swapped by tests for determinism.
	lastClickZone string
	lastClickAt   time.Time
	mouseClock    func() time.Time

	// sidebar is the left-rail instances+tabs tree
	sidebar *ui.Sidebar
	// paneWindows hosts one content window per open pane, keyed by the
	// store's stable pane id (#1088). The window owns per-pane view state
	// (capture content, scroll mode); the (instance, tab) binding it renders
	// lives in the store's open-pane list. Windows are created when a pane
	// opens and dropped when it closes; auto-hidden panes keep their window
	// (zero-rected) so their capture and scroll state survive narrow spells.
	paneWindows map[int]*ui.TabbedWindow
	// visiblePanes is the laid-out subset of the store's open panes, in
	// left-to-right order — rebuilt by relayout (§2.6 pane-count fitting:
	// the least-recently-focused panes beyond Layout.MaxPanes are hidden).
	visiblePanes []*store.OpenPane
	// lastPaneCapture is when each pane's capture was last dispatched, keyed
	// by pane id; the paneCaptureMinInterval throttle reads it (RFC §5.2).
	lastPaneCapture map[int]time.Time
	// -- Live embedded terminal (#1089 PR 1, read-only proof path) --
	//
	// At most ONE pane holds a live termpane attachment: the focused pane
	// (or the pane that already holds it while focus visits the rail). Its
	// window renders the termpane grid instead of the capture, and its
	// capture polling is skipped. Lifecycle in live_termpane.go; all four
	// fields are event-loop only.

	// liveTerm is the live attachment (a *termpane.TermPane in production,
	// behind the seam interface); nil when every pane renders captures.
	liveTerm liveTermAttachment
	// livePane is the open pane liveTerm is bound to; nil iff liveTerm is.
	livePane *store.OpenPane
	// liveBindKey identifies the (pane, tab, session) binding last attempted,
	// so the tick-driven sync only rebinds when the binding actually changed.
	liveBindKey string
	// liveBindFailedAt is when the last bind attempt failed, for the
	// liveBindRetryInterval backoff.
	liveBindFailedAt time.Time
	// liveBoundAt is when liveTerm was bound, so the client-died warning can
	// report the client's lifetime (instant death reads very differently
	// from a session killed hours in).
	liveBoundAt time.Time
	// liveDeathLogKey/liveDeathLogAt rate-limit the client-died warning: one
	// line per binding, refreshed every liveDeathLogInterval while a
	// respawn-die loop persists, instead of a line per 5s retry.
	liveDeathLogKey string
	liveDeathLogAt  time.Time
	// pendingTUIViewFocus is loaded before Bubble Tea reports the terminal size.
	// The pane bindings can restore immediately, but a pane focus target is only
	// focusable after the first non-fallback relayout has rebuilt the ring.
	pendingTUIViewFocus *config.TUIStateFocus

	// interactive is the two-mode keyboard switch (#1089 PR 2, RFC §2.3).
	// Nav mode (false): the host owns the keyboard — focus ring, verbs,
	// overlays, exactly as before. Interactive mode (true): EVERY keystroke
	// (including Tab) forwards down the focused pane's live attachment; the
	// only host-reserved key is Ctrl-], which returns to nav. The mode is
	// only ever true while liveTerm is bound to the focused pane —
	// enforceInteractiveInvariant drops it the moment that premise breaks.
	// Orthogonal to `state`: overlays opened by async events still own the
	// keyboard (handleKeyPress checks state alongside this flag). Event-loop
	// only; the pane's green frame and the status bar mirror it.
	interactive bool

	// initialPaneOpened latches the one-time startup auto-open: the first
	// instance selection opens its pane so the workspace isn't empty on
	// launch. Never reset — once the user has hidden every pane, the
	// workspace stays empty until they open one (`s`).
	initialPaneOpened bool
	// automations is the bottom section of the left rail (#1087): compact
	// task rows only — S/Enter open its full task manager as the stateTasks
	// overlay
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
	// alarmBanner is the top-of-screen delivery-failure alarm (#1238): a
	// persistent red bar raised while the daemon snapshot reports a watch task
	// whose events are failing to reach their target session. Fed each poll by
	// applyDeliveryAlarms from the snapshot's DeliveryAlarms projection.
	alarmBanner *ui.AlarmBanner
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
		alarmBanner:      ui.NewAlarmBanner(),
		ctx:              ctx,
		spinner:          spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		store:            proj,
		menu:             menu,
		errBox:           errBox,
		paneWindows:      make(map[int]*ui.TabbedWindow),
		lastPaneCapture:  make(map[int]time.Time),
		automations:      ui.NewAutomationsPane(proj),
		statusBar:        ui.NewStatusBar(menu, errBox),
		hooksPane:        ui.NewHooksPane(),
		ring:             layout.NewRing(layout.RegionTree, layout.RegionAutomations),
		zones:            zones.NewRegistry(),
		mouseClock:       time.Now,
		snapshotFetcher:  snapshotThroughDaemon,
		pauseStatusPoll:  pauseStatusPollThroughDaemon,
		resumeStatusPoll: resumeStatusPollThroughDaemon,
		appConfig:        appConfig,
		program:          program,
		autoYes:          autoYes,
		repoID:           repoID,
		repoRoot:         repo.Root,
		state:            stateDefault,
		appState:         appState,
	}
	h.sidebar = ui.NewSidebar(&h.spinner, autoYes, proj)
	h.wireZoneRegistry()
	// No panes are open at startup: the focus ring is tree → automations
	// until the first pane opens (relayout rebuilds the ring's pane entries
	// thereafter; the first instance selection auto-opens its pane).
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
	h.restoreTUIViewStateOnLaunch()

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
// and the #1088 pane-count fitting — and every pane is re-rected. Called on
// every WindowSizeMsg and whenever a grid input changes without a resize (a
// pane opening or closing).
func (m *home) relayout() {
	// The grid is asked for every open pane; it honors at most MaxPanes of
	// them (§2.6). The store then picks WHICH panes stay visible — the
	// most-recently-focused ones, in workspace order — while the hidden
	// panes' bindings persist and restore on grow, which is exactly the
	// retain-and-restore contract the A/B split had.
	m.grid.Panes = m.store.NumOpenPanes()
	// Size the automations section to its content: the grid grows it to show
	// every automation when the rail has the room, collapsing only when the
	// tree + automations can't both fit (#1126).
	m.grid.Automations = m.store.NumTasks()
	// Reserve the alarm banner row exactly when a delivery-failure alarm is
	// raised (#1238), so the row appears/disappears with the alarm and never
	// steals space in the healthy steady state.
	m.grid.Banner = m.alarmBanner.Active()
	lay := m.grid.Solve(m.termWidth, m.termHeight)
	m.lastLayout = lay
	if lay.Fallback {
		// No rects to hand out, but keep the focus flags + hints coherent so
		// key routing stays correct while the terminal is too small.
		m.visiblePanes = nil
		m.syncFocus()
		return
	}

	m.visiblePanes = m.store.VisibleOpenPanes(lay.PaneCount())

	// Rebuild the ring's pane entries to the visible set (auto-hidden panes
	// leave the ring; the focused pane is most-recently-focused, so it is
	// never the one auto-hidden). SetIDs keeps the active id when it
	// survives; a vanished active falls back to the tree.
	ids := make([]string, 0, len(m.visiblePanes)+2)
	ids = append(ids, layout.RegionTree)
	for _, p := range m.visiblePanes {
		ids = append(ids, layout.PaneRegion(p.ID()))
	}
	ids = append(ids, layout.RegionAutomations)
	m.ring.SetIDs(ids...)
	m.ring.SetHidden(layout.RegionAutomations, !lay.AutomationsVisible)
	m.applyPendingTUIViewFocus()
	m.syncFocus()

	m.sidebar.SetRect(lay.Tree)
	visible := make(map[int]bool, len(m.visiblePanes))
	for i, p := range m.visiblePanes {
		visible[p.ID()] = true
		if w := m.paneWindows[p.ID()]; w != nil {
			w.SetRect(lay.Panes[i])
		}
	}
	// Auto-hidden panes render nothing while retaining their window state.
	for id, w := range m.paneWindows {
		if !visible[id] {
			w.SetRect(layout.Rect{})
		}
	}
	m.automations.SetRect(lay.Automations)
	m.automations.SetCompact(lay.AutomationsCompact)
	m.statusBar.SetRect(lay.StatusBar)
	m.alarmBanner.SetRect(lay.Banner)

	m.layoutTextOverlay()
	if m.selectionOverlay != nil {
		m.selectionOverlay.SetWidth(int(float32(m.termWidth) * 0.6))
	}
	m.hooksPane.SetSize(int(float32(m.termWidth)*0.6), int(float32(m.termHeight)*0.6))
	// The task manager renders in the centered tasks overlay (stateTasks), so
	// it sizes off the terminal like the hooks editor — never off the rail.
	// Floor the width so the edit form's fields and key line stay readable at
	// an 80-col terminal, capped under the window for the overlay frame.
	taskPaneW := int(float32(m.termWidth) * 0.6)
	if taskPaneW < 52 {
		taskPaneW = 52
	}
	if lim := m.termWidth - 8; taskPaneW > lim {
		taskPaneW = lim
	}
	m.automations.TaskPane().SetSize(taskPaneW, int(float32(m.termHeight)*0.6))

	// tmux sessions render at the pane content size. Panes divide the
	// workspace evenly, so the first visible pane's inner size stands for all
	// of them; with no panes open the last size is kept (nothing renders).
	if len(m.visiblePanes) > 0 {
		if w := m.paneWindows[m.visiblePanes[0].ID()]; w != nil {
			previewWidth, previewHeight := w.GetPreviewSize()
			if err := m.store.SetSessionPreviewSize(previewWidth, previewHeight); err != nil {
				log.ErrorLog.Print(err)
			}
		}
	}
}

// syncFocus applies the focus ring's active region to the panes and the
// status-bar hints, and stamps a focused pane most recently focused so the
// §2.6 auto-hide order tracks real attention.
func (m *home) syncFocus() {
	active := m.ring.Active()
	panes := map[string]layout.Pane{
		layout.RegionTree:        m.sidebar,
		layout.RegionAutomations: m.automations,
	}
	for _, p := range m.visiblePanes {
		if w := m.paneWindows[p.ID()]; w != nil {
			panes[layout.PaneRegion(p.ID())] = w
		}
	}
	for id, pane := range panes {
		if id == active {
			pane.Focus()
		} else {
			pane.Blur()
		}
	}
	if p := m.focusedOpenPane(); p != nil {
		m.store.TouchOpenPane(p)
	}
	m.menu.SetFocusRegion(active)
}

// focusRegion moves focus directly to the given region and re-solves the
// layout so ring visibility and pane rects stay coherent.
func (m *home) focusRegion(region string) {
	m.ring.Focus(region)
	m.relayout()
}

// cycleFocus advances the focus ring (Tab / Shift-Tab). Task edits are made
// only inside the tasks overlay, which saves on close (handleStateTasks) —
// the in-rail automations section is read-only, so no save is needed here.
func (m *home) cycleFocus(back bool) tea.Cmd {
	if back {
		m.ring.Prev()
	} else {
		m.ring.Next()
	}
	m.relayout()
	return nil
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
	defer m.persistTUIViewStateAfter(msg)
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
		//
		// The live-termpane sync rides the same tick (and handles the
		// attached case itself, by closing the attachment): renders are
		// pulled by this existing cadence, never by a termpane-owned loop
		// (#1089 perf guard).
		m.syncLiveTermPane()
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
		// Live-project out-of-band task changes on the same poll (#1168),
		// independent of the session snapshot's own error path.
		if m.refreshTasks(msg.tasks, msg.tasksErr) {
			changed = true
		}
		detachTrace(tickStart, "snapshotFetchedMsg-reconcile-returned")
		cmds := []tea.Cmd{tickRefreshExternalCmd}
		if changed {
			cmds = append(cmds, m.selectionChanged())
		}
		return m, tea.Batch(cmds...)
	case taskTriggeredMsg:
		// The daemon-side run (#1169) failed — surface it. Success needs no
		// action: the resulting session and updated task status live-project in.
		if msg.err != nil {
			return m, m.handleError(fmt.Errorf("failed to trigger task %q: %w", msg.title, msg.err))
		}
		return m, nil
	case tea.MouseMsg:
		// First-class mouse (#1024 R4, closes #1025): every event is
		// resolved through the zone registry the last View() rebuilt and
		// dispatched to the region actually under the cursor — clicks
		// select/focus/interact/act, the wheel scrolls the hovered region,
		// and while interactive, events over the live pane's grid forward
		// to the embedded terminal. Purely additive: the keyboard remains
		// fully sufficient.
		return m.handleMouse(msg)
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
	case enterInteractiveMsg:
		// Deferred by the first-time interactive help screen's dismiss cmd
		// (#1089 PR 2); the pane pointer is re-validated inside.
		return m, m.activateInteractive(msg.pane)
	case startKillMsg:
		// The row was already flipped to Deleting synchronously by the kill
		// confirmation; dispatch the slow teardown off the event loop (#844).
		return m, m.killInstanceCmd(msg.title)
	case instanceKilledMsg:
		return m.handleInstanceKilled(msg)
	case startArchiveMsg:
		// Archive confirmed; run the daemon teardown+move off the event loop
		// (#1028), mirroring the kill dispatch.
		return m, m.archiveInstanceCmd(msg.title)
	case instanceArchivedMsg:
		return m.handleInstanceArchived(msg)
	case instanceRestoredMsg:
		return m.handleInstanceRestored(msg)
	case limitRetriedMsg:
		return m.handleLimitRetried(msg)
	case repaintAfterDetachMsg:
		// Trigger an immediate repaint with whatever content is already
		// cached on the panes (rendered when bubbletea's main loop calls
		// View() after this Update returns), and kick off the async
		// preview/terminal refresh so fresh content lands within a few
		// milliseconds. selectionChanged() also dispatches the captures
		// off the event loop, so this handler returns instantly.
		detachTraceMark("repaintAfterDetachMsg-handler-entry")
		// The post-detach repaint must be immediate: reset the per-pane
		// capture throttle so every visible pane's capture dispatches now
		// instead of waiting out paneCaptureMinInterval (#1088 — the
		// single-pane path was unthrottled and repainted instantly).
		m.lastPaneCapture = make(map[int]time.Time)
		cmd := m.selectionChanged()
		detachTraceMark("repaintAfterDetachMsg-handler-exit")
		// The watchdog armed in attachOverlayCallback is ended when the
		// post-detach paint completes (panesRefreshedMsg). That msg is only
		// emitted by refreshPanesCmd, which selectionChanged dispatches only
		// for visible open panes. With no pane to refresh — an empty
		// workspace, or every pane pruned while the user was attached — no
		// panesRefreshedMsg ever arrives, and the watchdog would fire a
		// spurious goroutine dump after slowDetachThreshold even though there
		// was nothing to paint. Cancel it here: a nil cmd, or a cmd carrying
		// only non-capture work (the PR-info fetch), means the detach already
		// completed everything it was going to paint (#683 class).
		if cmd == nil || len(m.visiblePanes) == 0 {
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
		// A fresh session with an empty workspace opens its agent pane
		// (#1088): the pre-N-pane TUI showed the new session immediately, and
		// an empty workspace after `n` would read as a failed create. A
		// workspace that already has panes is left alone — the new session is
		// one `s` away.
		if m.store.NumOpenPanes() == 0 {
			m.initialPaneOpened = true
			m.openPaneWindow(started, 0)
			m.relayout()
		}
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
	m.flushTUIViewStateBestEffort()

	// No instances.json write on quit: the daemon is the sole writer (#960 PR 4)
	// and every session/tab mutation already persisted through it as it
	// happened. The TUI holds no authoritative instance state to flush.
	//
	// Do NOT tear down tab sessions on quit: as of #930 PR 2 each instance owns
	// its agent and shell tab tmux sessions, and they must survive an af restart
	// so the user reconnects to them on next launch (Sachin's persistence
	// requirement). Killing an instance still tears its tabs down via
	// LocalBackend.Kill.
	//
	// The live termpane attachment is the one exception: release its attach
	// CLIENT (the session survives, exactly like a detach) so no orphaned
	// `tmux attach-session` child outlives the TUI (#1089).
	m.closeLiveTermPane()
	m.quitting = true
	return m, cleanQuitCmd()
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
	//
	// The writes route through the daemon (#1029 PR 6): the daemon is the sole
	// writer of tasks.json among clients (#960), so a TUI edit/delete goes
	// through the same RPC wrappers the CLI uses instead of touching the file
	// directly. Each CRUD RPC re-arms the daemon's scheduler + watchers
	// in-process, so there is no separate ReloadTasks poke here — the write and
	// its schedule refresh are one atomic daemon call (removing the old
	// double-reload).
	//
	// Persist ONLY the tasks the user actually edited (ConsumeDirty), not the
	// whole pane: an unmodified task changed out-of-band (CLI, daemon) while the
	// pane was open must not be clobbered by the pane's stale copy — #1213.
	for _, tsk := range sp.ConsumeDirty() {
		if err := updateTaskThroughDaemon(tsk); err != nil {
			log.ErrorLog.Printf("failed to update task: %v", err)
			saveErr = errors.Join(saveErr, fmt.Errorf("failed to save task %q: %w", tsk.Name, err))
		}
	}
	for _, tsk := range sp.ConsumeDeleted() {
		if err := removeTaskThroughDaemon(tsk.ID); err != nil {
			log.ErrorLog.Printf("failed to remove task: %v", err)
			saveErr = errors.Join(saveErr, fmt.Errorf("failed to remove task %q: %w", tsk.Name, err))
		}
	}
	// Reload BOTH panes from disk so the TaskPane and sidebar can never diverge
	// (#934): whatever actually committed, both panes now show it.
	tasks, err := task.LoadTasksForCurrentRepo()
	if err == nil {
		m.store.SetTasks(tasks)
		sp.SetTasks(tasks)
		// The task count feeds the rail's automations-section height (#1126);
		// reflow so an add/delete grows or shrinks the section immediately.
		m.relayout()
	} else {
		saveErr = errors.Join(saveErr, fmt.Errorf("failed to reload tasks after save: %w", err))
	}
	return saveErr
}

// saveInRepoPostWorktreeCommandsFn is indirected so TUI tests can force a
// hooks-save failure deterministically — without relying on filesystem
// permission tricks that a root test runner would bypass (#1001).
var saveInRepoPostWorktreeCommandsFn = config.SaveInRepoPostWorktreeCommands

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
	// Route the create through the daemon (#1029 PR 6): it is the sole writer of
	// tasks.json among clients (#960) and re-arms its own scheduler/watchers in
	// the same RPC, so there is no separate ReloadTasks poke — the write and its
	// schedule refresh are one atomic daemon call.
	if err := addTaskThroughDaemon(t); err != nil {
		return m.handleError(fmt.Errorf("failed to save task: %v", err))
	}
	// Refresh sidebar and task pane
	tasks, err := task.LoadTasksForCurrentRepo()
	if err == nil {
		m.store.SetTasks(tasks)
		sp.SetTasks(tasks)
		// Reflow so the new automation grows the rail's section (#1126).
		m.relayout()
	}
	return nil
}

// taskTriggeredMsg reports the outcome of a TUI "run now" (#1169). The run
// itself happens daemon-side (create-or-deliver + status write); the resulting
// session and updated task status live-project back into the TUI, so success
// needs no on-loop mutation — only a failure is surfaced to the user.
type taskTriggeredMsg struct {
	title string
	err   error
}

// handleTaskTrigger runs the selected task through the daemon's single shared
// trigger path — via the TriggerTask RPC, which calls daemon.RunTask, the SAME
// entrypoint `af tasks trigger` and the cron scheduler use. Previously the TUI
// "run now" unconditionally spawned a new per-run session, ignoring the task's
// target_session and orphaning it (#1169); routing through RunTask makes it
// honor target_session (deliver into it, auto-creating when missing) and spawn
// a fresh session only when there is no target — matching CLI/cron exactly. The
// daemon owns the create/deliver, so the new/updated session appears via the
// Snapshot projection and the task's run status via the task refresh, with no
// divergent TUI spawn path to drift.
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

	// The trigger fires from inside the tasks overlay: close it and move focus to
	// the tree so the user is looking at the sessions, where the run lands (a
	// fresh per-run session, or the delivered-into target_session).
	if m.state == stateTasks {
		m.state = stateDefault
		sp.SetFocus(false)
	}
	m.focusRegion(layout.RegionTree)

	taskID := tsk.ID
	taskTitle := task.TaskRunBaseTitle(*tsk)
	// Capture the trigger seam on the event loop before the goroutine reads it, so
	// a concurrent test-seam swap can't race the read (#960 PR 4 race-fix class).
	trigger := triggerTaskThroughDaemon
	triggerCmd := func() tea.Msg {
		if err := trigger(taskID); err != nil {
			return taskTriggeredMsg{title: taskTitle, err: err}
		}
		return taskTriggeredMsg{title: taskTitle}
	}

	return tea.Batch(tea.WindowSize(), m.selectionChanged(), triggerCmd)
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
	// Any other modal state (help/confirm/search/select-program/hooks): the
	// overlay owns the keyboard, so no hint highlighting and no re-emit —
	// this runs BEFORE handleKeyPress's state switch, so without this guard
	// mapped keys typed into an overlay would take the highlight + re-emit
	// detour first. A blanket non-default check (rather than enumerating
	// states) can't silently miss a future modal state (Greptile on #1083).
	if m.state != stateDefault {
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
	// Interactive mode owns the keyboard before anything else — menu
	// highlighting, quit keys, the global key map (#1089 PR 2, RFC §2.3).
	// The state gate matters: an overlay opened by an async event (e.g. the
	// instance-started help screen) is modal and keeps the keyboard until
	// dismissed, exactly as in nav mode.
	if m.interactive && m.state == stateDefault {
		return m.handleInteractiveKey(msg)
	}

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
	case stateTasks:
		return m.handleStateTasks(msg)
	}

	// The focused in-rail automations section owns its cursor keys; Enter/Esc
	// route here too (open the manager overlay / return to the tree), while
	// Tab/Shift-Tab, quit, and the global overlay keys (S/H/?) fall through.
	if mod, cmd, consumed := m.handleAutomationsFocus(msg); consumed {
		return mod, cmd
	}

	// Exit scrolling mode when ESC is pressed (each pane keeps its own
	// scroll state, #1088)
	if msg.Type == tea.KeyEsc {
		if pane, bound := m.focusedContentPane(); pane != nil && pane.IsInScrollMode() {
			if err := pane.ResetToNormalMode(bound); err != nil {
				return m, m.handleError(err)
			}
			return m, m.selectionChanged()
		}
	}

	// Ctrl+C is an always-on hard exit — never rebindable, so it stays a
	// hardcoded check ahead of the keymap. The quit VERB (default q, or
	// whatever [keys].quit rebinds it to) dispatches through the generated
	// table like every other rebindable action, via keys.KeyQuit in
	// handleDefaultKeyPress (#1026).
	if msg.String() == "ctrl+c" {
		return m.handleQuit()
	}

	// Number-key tab jump (1-9): jump directly to that tab of the selected
	// instance (#930 PR 4). Handled here, before the GlobalKeyStringsMap
	// lookup, because digits are dispatched manually rather than mapped to a
	// KeyName. Gated on the focus region, not just on the strip having
	// consumed the key above: the jump belongs to the tree/workspace, so a
	// digit with the automations strip focused must never retarget the
	// selection — the pre-cutover ContentModeTasks behavior (Greptile on
	// #1083; the strip's task list does consume digits today, but this gate
	// must not depend on that).
	if active := m.ring.Active(); active == layout.RegionTree || layout.IsPaneRegion(active) {
		if len(msg.Runes) == 1 {
			if r := msg.Runes[0]; r >= '1' && r <= '9' {
				return m.handleTabJump(int(r - '0'))
			}
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
func (m *home) attachOverlayCallback(title, label, traceSuffix string, remote bool, attach func() (chan struct{}, error)) tea.Cmd {
	detachTraceMark(label + "-onDismiss-entry" + traceSuffix)
	ch, err := attach()
	if err != nil {
		log.ErrorLog.Printf("failed to attach (%s): %v", label+traceSuffix, err)
		return nil
	}

	// While we hold the shared tmux server full-screen, ask the daemon to pause
	// its per-instance capture-pane liveness poll for THIS instance so it stops
	// contending with the live attach (#1160, Fix A follow-up to #1157). A
	// heartbeat renews the daemon's lease-bounded pause until detach; the pause
	// is best-effort so a down/slow daemon never disturbs the attach.
	//
	// Capture the seams + repoID off the home HERE, on the event loop, before
	// any goroutine spawns: the seams are per-home fields (not shared globals)
	// precisely so the goroutines never read home state a sibling test could
	// reassign under `go test -parallel -race` (the #964 / #960-PR4 race class).
	pause := m.pauseStatusPoll
	resume := m.resumeStatusPoll
	repoID := m.repoID
	pauseDone := make(chan struct{})
	heartbeatExited := make(chan struct{})
	go runStatusPollPauseHeartbeat(pause, title, repoID, pauseDone, heartbeatExited)

	m.attached.Store(true)
	defer m.attached.Store(false)
	// <-ch blocks for as long as the user is attached. Mark the boundary so
	// post-detach elapsed times in the trace are measured from when the user
	// actually returned to the UI, not from when the attach started.
	detachTraceMark(label + "-blocking-on-<-ch" + traceSuffix)
	<-ch
	// Stop the heartbeat and resume the daemon's poll immediately on this clean
	// detach — don't wait out the lease. The resume must WIN over any in-flight
	// pause: both were fire-and-forget, so a naive resume could land on the wire
	// before the heartbeat's last pause() and leave the instance paused until the
	// lease expires (Greptile P). runStatusPollResume waits for heartbeatExited
	// (the heartbeat closes it after its final synchronous pause() returns — and
	// callDaemon blocks until the daemon has applied that pause) so the resume
	// strictly follows it. This runs on its OWN goroutine so the detach hot path
	// never blocks on the wait or the RPC — attach/detach responsiveness is the
	// whole point of #1160.
	close(pauseDone)
	go runStatusPollResume(resume, title, repoID, heartbeatExited)
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

// statusPollRenewInterval is how often an attached TUI re-sends PauseStatusPoll
// to renew the daemon's lease-bounded pause (#1160). It MUST stay below the
// daemon's statusPollLease (3s) so a live attach never lets the lease lapse and
// let the daemon's capture-pane poll resume mid-attach; 1s against a 3s lease
// leaves two missed renews of slack for a hiccuping daemon.
const statusPollRenewInterval = 1 * time.Second

// runStatusPollPauseHeartbeat pauses the daemon's capture-pane poll for the
// attached instance and renews that lease every statusPollRenewInterval until
// done closes (detach), then closes exited. pause + repoID are captured off the
// event loop by the caller so this goroutine never touches shared home state
// (#964 race class). Every RPC is best-effort — a down/slow daemon logs and
// continues, never disturbing the attach (worst case the daemon keeps polling,
// the pre-#1160 behavior). Because pause() is called SYNCHRONOUSLY in the loop,
// once this goroutine returns no pause RPC is in-flight or pending, so exited
// firing is the signal a following resume can safely win the wire.
func runStatusPollPauseHeartbeat(pause func(title, repoID string) error, title, repoID string, done <-chan struct{}, exited chan<- struct{}) {
	defer close(exited)
	send := func() {
		if err := pause(title, repoID); err != nil {
			log.ErrorLog.Printf("failed to pause daemon status poll for %q: %v", title, err)
		}
	}
	send() // pause immediately on attach, before the first renew tick
	ticker := time.NewTicker(statusPollRenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			send()
		}
	}
}

// runStatusPollResume resumes the daemon's poll on a clean detach so the poll
// resumes immediately rather than after the lease expires (#1160). It waits for
// heartbeatExited FIRST so the resume RPC strictly follows the heartbeat's final
// pause() — guaranteeing resume wins over any in-flight pause instead of racing
// it (Greptile P). resume + repoID are captured off the event loop by the
// caller (#964 race class). Best-effort; the caller runs this on its own
// goroutine so the detach hot path never blocks on the wait or the RPC.
func runStatusPollResume(resume func(title, repoID string) error, title, repoID string, heartbeatExited <-chan struct{}) {
	<-heartbeatExited
	if err := resume(title, repoID); err != nil {
		log.ErrorLog.Printf("failed to resume daemon status poll for %q: %v", title, err)
	}
}

// selectionChanged updates the selection binding and menu based on the
// sidebar selection, and drives the open panes' capture refresh. The
// preview/terminal tmux captures are dispatched via a tea.Cmd (goroutine)
// rather than run synchronously: each call shells out to `tmux capture-pane`
// (~3–5ms locally), and on the bubbletea Update goroutine that cost
// compounded — every previewTickMsg (100ms) blocked the event loop, and the
// first paint after detach paid the full cost on top of waiting up to a full
// tick cycle for the next msg (#579, #559 sibling). The TabPane guards its
// captured state with a mutex so the goroutine can mutate it while View()
// reads it. Synchronous fields touched here (selection binding, menu state)
// stay on the event loop.
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
	if sel.Kind == ui.SectionInstances && !sel.IsHeader {
		selected := m.sidebar.GetSelectedInstance()
		// Track the cursor's instance in the store's display selection — what
		// selection-scoped verbs (`s`, Enter-from-tree, tree-focus 1-9) act on —
		// then re-clamp the active tab index against the new instance's tab count.
		m.store.SetSelectedInstance(selected)
		m.clampSelectionTab()
		m.menu.SetInstance(selected)
		// The tree cursor drives the active tab too (landing on a tab row
		// selects that tab — #1024 PR 3), so mirror it into the menu here, not
		// just in the explicit tab-jump handlers.
		m.menu.SetActiveTab(m.store.ActiveTab())
		m.maybeAutoOpenInitialPane(selected)
		detachTrace(selectionStart, "selectionChanged-instance-branch-built-cmds")
		// Lazily refresh PR info when the user lands on an instance that
		// hasn't been fetched recently. fetchPRInfoCmd is a no-op when the
		// data is still fresh, so rapid Up/Down navigation doesn't hammer gh.
		if !attachedNow && selected != nil && selected.Started() {
			prFetch = fetchPRInfoCmd(selected, false)
		}
	} else {
		// Header row: the menu drops the instance-specific hints; the open
		// panes are untouched (they are explicit bindings, not
		// selection-driven). The startup auto-open still gets its chance —
		// launch rests the cursor on the Instances header (launch selection
		// parity, #1024 PR 2), so with only the instance-branch call above a
		// cold start with restored sessions landed on the empty workspace
		// until the first cursor move (#1099 play-test).
		m.maybeAutoOpenInitialPane(nil)
		m.menu.SetInstance(nil)
		if selected := m.store.GetSelectedInstance(); selected != nil && !m.store.ContainsInstance(selected) {
			// The sticky binding dangles — its instance was removed (e.g. the
			// last instance killed while attached). Drop it so the pane verbs
			// can't target a dead session.
			m.store.SetSelectedInstance(nil)
		}
	}

	return tea.Batch(prFetch, m.panesRefresh(attachedNow))
}

// clampSelectionTab bounds the selection's active tab index against the
// selected instance's tab count — the tree-selection half of the clamping the
// per-pane windows do for their own bindings (#930 PR 4 class).
func (m *home) clampSelectionTab() {
	n := len(tree.TabLabels(m.store.GetSelectedInstance()))
	if cur := m.store.ActiveTab(); cur >= n && n > 0 {
		m.store.SetActiveTab(n - 1)
	} else if cur < 0 {
		m.store.SetActiveTab(0)
	}
}

// maybeAutoOpenInitialPane opens the first selected instance's tab as the
// first pane, once per TUI run, so launch doesn't land on an empty workspace
// (#1088). Focus is NOT moved — the user is on the tree at startup. Once the
// latch is set the workspace is entirely verb-driven: hiding every pane
// leaves it empty until `s` opens one.
//
// A nil selected falls back to the first NON-reserved sidebar instance
// (firstAutoOpenCandidate): launch never auto-selects a row (the cursor rests
// on the Instances header — #1024 PR 2), so a cold start with restored sessions
// has no selection to auto-open from. Preferring a non-reserved row keeps root
// from being front-and-center after every relaunch (#1238). The fallback opens
// the pane without touching the selection, and because selectionChanged
// re-enters on every preview tick, it also re-fires once a restored instance
// leaves a transient status (#1099 play-test).
func (m *home) maybeAutoOpenInitialPane(selected *session.Instance) {
	if m.initialPaneOpened || m.store.NumOpenPanes() > 0 {
		return
	}
	if selected == nil {
		selected = firstAutoOpenCandidate(m.store.GetInstances())
		if selected == nil {
			return
		}
	}
	if selected.HasInFlightOp() {
		return
	}
	m.initialPaneOpened = true
	m.openPaneWindow(selected, m.store.ActiveTab())
	m.relayout()
}

// paneCaptureMinInterval floors each open pane's capture cadence. At the
// previewTick period (100ms) it admits one capture per pane per tick — the
// one-capture-per-pane budget of RFC §5.2 — and swallows the extra
// selectionChanged calls rapid tree navigation fires between ticks. If
// tmux-server contention ever resurfaces (#598 class), raising this ONE
// constant (e.g. to 250ms) degrades every pane's refresh without touching
// the tick.
const paneCaptureMinInterval = 100 * time.Millisecond

// panesRefresh keeps the open-pane list coherent and returns the visible
// panes' throttled capture cmds (nil when there is nothing to do). Runs on
// the event loop (#1088, generalizing the PR-5 paneBRefresh).
func (m *home) panesRefresh(attachedNow bool) tea.Cmd {
	// Prune panes whose instance left the projection (killed here, or
	// removed by an external kill the snapshot reconcile mirrored) rather
	// than keep rendering a dead session's last capture.
	if m.pruneDeadPanes() {
		m.relayout()
	}
	// All panes pause while attached (#598): no capture work may queue
	// behind the user's detach key. Auto-hidden panes don't capture either —
	// they are invisible.
	if attachedNow {
		return nil
	}
	var cmds []tea.Cmd
	for _, p := range m.visiblePanes {
		w := m.paneWindows[p.ID()]
		if w == nil {
			continue
		}
		// The pane's tab index can dangle when its instance's tab set shrank
		// (e.g. another view closed the tab this pane was showing).
		w.ClampActiveTab()
		// A pane rendering through a live termpane attachment doesn't poll
		// capture-pane (#1089): the attach client already streams the same
		// content, tmux-flow-limited. Capture resumes the tick after the
		// attachment closes.
		if w.HasLive() {
			continue
		}
		if time.Since(m.lastPaneCapture[p.ID()]) < paneCaptureMinInterval {
			continue
		}
		m.lastPaneCapture[p.ID()] = time.Now()
		cmds = append(cmds, refreshPanesCmd(w, p.Instance()))
	}
	return tea.Batch(cmds...)
}

// focusedContentPane resolves which content pane scroll/attach keys act on:
// the focused pane when the focus ring points at one; with the tree or
// automations focused, the pane showing the selection's (instance, active
// tab) if that tab is open and visible. Returns (nil, nil) when no pane
// applies — the workspace may be empty (#1088).
func (m *home) focusedContentPane() (*ui.TabbedWindow, *session.Instance) {
	if p := m.focusedOpenPane(); p != nil {
		return m.paneWindows[p.ID()], p.Instance()
	}
	selected := m.store.GetSelectedInstance()
	if selected == nil {
		return nil, nil
	}
	if p := m.store.FindOpenPane(selected, m.store.ActiveTab()); p != nil {
		for _, vis := range m.visiblePanes {
			if vis == p {
				return m.paneWindows[p.ID()], p.Instance()
			}
		}
	}
	return nil, nil
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

// startArchiveMsg is emitted by the archive confirmation (#1028); its handler
// dispatches archiveInstanceCmd to run the daemon teardown+move off the event
// loop, mirroring startKillMsg → killInstanceCmd.
type startArchiveMsg struct {
	title string
}

// instanceArchivedMsg / instanceRestoredMsg report completion of an async
// archive / restore (#1028). On success the row's new status arrives via the
// next daemon Snapshot reconcile (which re-partitions it into / out of the
// Archived folder); a non-nil err is surfaced in the error box.
type instanceArchivedMsg struct {
	title string
	err   error
}

type instanceRestoredMsg struct {
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
// View composes the workspace from the solved layout. The mouse zone
// registry is rebuilt here every frame (#1024 R4): Reset at the top, then
// each pane registers its interactive rects while rendering and the active
// overlay registers its buttons on top. The registry therefore always
// mirrors exactly what this frame put on screen.
func (m *home) View() string {
	m.zones.Reset()
	if m.quitting {
		return ""
	}

	// Below the hard minimum no layout exists; render the banner alone (and
	// register nothing — there is nothing to click).
	if m.lastLayout.Fallback {
		return ui.TerminalTooSmall(m.termWidth, m.termHeight)
	}

	// The left rail stacks the tree over the bottom-aligned automations
	// section, separated by a horizontal rule (#1087); the workspace panes
	// take the full height beside it (#1090), divided evenly with 1-col
	// dividers (#1088). With no panes open the workspace renders the
	// open-pane affordance.
	railParts := []string{m.sidebar.View()}
	if m.lastLayout.AutomationsVisible {
		railParts = append(railParts, m.renderRailRule(), m.automations.View())
	}
	rail := lipgloss.JoinVertical(lipgloss.Left, railParts...)
	cols := []string{rail}
	if len(m.visiblePanes) == 0 {
		if m.store.NumInstances() == 0 {
			cols = append(cols, ui.FirstRunWorkspace(m.lastLayout.Workspace))
		} else {
			cols = append(cols, ui.EmptyWorkspace(m.lastLayout.Workspace))
		}
	}
	for i, p := range m.visiblePanes {
		if i > 0 {
			cols = append(cols, m.renderDivider(i-1))
		}
		if w := m.paneWindows[p.ID()]; w != nil {
			w.SetSelectionHint(m.paneSelectionHint(p))
			cols = append(cols, w.View())
		}
	}
	top := lipgloss.JoinHorizontal(lipgloss.Top, cols...)
	// Stack the delivery-failure alarm banner (#1238) above everything when
	// raised, so it is visible without navigating and the layout reserved its
	// row in relayout.
	viewParts := make([]string, 0, 3)
	if banner := m.alarmBanner.View(); banner != "" {
		viewParts = append(viewParts, banner)
	}
	viewParts = append(viewParts, top, m.statusBar.View())
	mainView := lipgloss.JoinVertical(lipgloss.Left, viewParts...)

	if m.state == stateHelp {
		if m.textOverlay == nil {
			log.ErrorLog.Printf("text overlay is nil")
		}
		return overlay.PlaceOverlay(0, 0, m.textOverlay.Render(), mainView, true)
	} else if m.state == stateConfirm {
		if m.confirmationOverlay == nil {
			log.ErrorLog.Printf("confirmation overlay is nil")
		}
		fg := m.confirmationOverlay.Render()
		m.confirmationOverlay.RegisterZones(m.zones, overlayOrigin(fg, mainView))
		return overlay.PlaceOverlay(0, 0, fg, mainView, true)
	} else if m.state == stateSearch {
		if m.searchOverlay == nil {
			log.ErrorLog.Printf("search overlay is nil")
		}
		fg := m.searchOverlay.Render()
		m.searchOverlay.RegisterZones(m.zones, overlayOrigin(fg, mainView))
		return overlay.PlaceOverlay(0, 0, fg, mainView, true)
	} else if m.state == stateSelectProgram {
		if m.selectionOverlay == nil {
			log.ErrorLog.Printf("selection overlay is nil")
		}
		fg := m.selectionOverlay.Render()
		m.selectionOverlay.RegisterZones(m.zones, overlayOrigin(fg, mainView))
		return overlay.PlaceOverlay(0, 0, fg, mainView, true)
	} else if m.state == stateHooks {
		return overlay.PlaceOverlay(0, 0, m.renderHooksOverlay(), mainView, true)
	} else if m.state == stateTasks {
		return overlay.PlaceOverlay(0, 0, m.renderTasksOverlay(), mainView, true)
	}

	return mainView
}

// The View render helpers (overlay framing + rail/divider rules) live in
// render.go, extracted to keep app.go under its file-length ceiling (#1145).
