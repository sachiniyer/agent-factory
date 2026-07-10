package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
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
)

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
	// stateSwitchProject is the state when the project-picker overlay is open
	// (#1461): pick another repo af has seen and switch the TUI to it in place.
	stateSwitchProject
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
	// attachTransitioning suppresses the final pre-attach View so Bubble Tea
	// clears AF chrome before the blocking full-screen tmux attach takes over.
	attachTransitioning bool
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
	// tabDrag tracks a possible/active sidebar-tab drag. The candidate starts
	// on tree tab press; motion promotes it to an active drag; release either
	// replays the ordinary tab click or drops onto a visible pane.
	tabDrag *tabDragState

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
	// pendingPaneAutoHideStatus is set by relayout when a previously visible
	// pane is auto-hidden by width pressure. Callers that can return a tea.Cmd
	// consume it to start the same transient clear timer normal errors use.
	pendingPaneAutoHideStatus string
	// restoredPaneBaseline holds the panes reopened from persisted TUI state so
	// the first real (non-fallback) relayout after launch can detect panes the
	// terminal is too narrow to fit. The restore-time relayout runs at term
	// (0,0) → fallback → visiblePanes=nil, so without a baseline the first
	// WindowSizeMsg sees an empty previousVisible and newlyAutoHiddenPane never
	// surfaces the "N hidden: terminal too narrow" status (#1535). Consumed once
	// on the first non-fallback relayout.
	restoredPaneBaseline []*store.OpenPane
	// lastPaneCapture is when each pane's capture was last dispatched, keyed
	// by pane id; the paneCaptureMinInterval throttle reads it (RFC §5.2).
	lastPaneCapture map[int]time.Time
	// panePreviewTxn is a transient #1321 preview binding owned by the most
	// recently focused content pane. It never mutates the pane's committed
	// store.OpenPane binding; commit/cancel semantics land in later PRs.
	panePreviewTxn *panePreviewTxn
	// lastFocusedPaneID remembers the focused pane before sidebar navigation
	// re-homes focus to the tree (#1233/#1236). Preview-on-scroll uses it as
	// the owner pane while the tree cursor moves.
	lastFocusedPaneID int
	// panePreviewSuppression remembers a user-dismissed preview target so the
	// 100ms preview tick does not recreate it until the sidebar target changes.
	panePreviewSuppression *panePreviewSuppression
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
	// transientNoticeID is a generation token for the status-bar notice timer.
	// Each new error/success notice increments it; a stale hideErrMsg from an
	// older timer must not clear a newer notice.
	transientNoticeID uint64
	// alarmBanner is the top-of-screen delivery-failure alarm (#1238): a
	// persistent red bar raised while the daemon snapshot reports a watch task
	// whose events are failing to reach their target session. Fed each poll by
	// applyDeliveryAlarms from the snapshot's DeliveryAlarms projection.
	alarmBanner *ui.AlarmBanner
	// global spinner instance. we plumb this down to where it's needed
	spinner spinner.Model
	// textOverlay displays text information
	textOverlay *overlay.TextOverlay
	// textOverlayDismissAnyKey keeps the one-shot intro/created overlays as
	// press-any-key gates while the general ? help behaves like a scrollable
	// modal with explicit dismiss keys. The attach overlay has its own policy
	// (attachHelpDismissPolicy) distinguishing Enter (proceed) from Esc/Ctrl+C
	// (cancel).
	textOverlayDismissAnyKey bool
	// textOverlayDismissPolicy, when set, decides whether a help overlay key
	// closes the overlay and whether its OnDismiss callback should run.
	textOverlayDismissPolicy func(tea.KeyMsg) (dismiss bool, runOnDismiss bool)
	// replayHelpDismissKey marks the first-run interactive pane help: the
	// key that closes that overlay is the user's first pane keystroke, so it
	// must be forwarded after the deferred live bind completes (#1410).
	replayHelpDismissKey bool
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
	// projectPickerOverlay handles switching the active project (#1461)
	projectPickerOverlay *overlay.ProjectPickerOverlay
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
	applyTheme(appConfig.Theme)

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
func (m *home) updateHandleWindowSizeEvent(msg tea.WindowSizeMsg) tea.Cmd {
	m.termWidth = msg.Width
	m.termHeight = msg.Height
	m.relayout()
	return m.consumePaneAutoHideStatus()
}

// relayout is the single sizing path (#1024 PR 4): layout.Grid turns the
// terminal size into the region rects — applying the §2.6 degradation ladder
// and the #1088 pane-count fitting — and every pane is re-rected. Called on
// every WindowSizeMsg and whenever a grid input changes without a resize (a
// pane opening or closing).
func (m *home) relayout() {
	previousVisible := append([]*store.OpenPane(nil), m.visiblePanes...)
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

	// First real relayout after a restore: the restore-time relayout ran at
	// (0,0) and fell through to fallback with visiblePanes=nil, so the restored
	// panes are the visibility baseline this pass uses to detect a pane the
	// terminal can't fit (#1535). Consumed once; every later relayout carries a
	// real previousVisible.
	if len(previousVisible) == 0 && len(m.restoredPaneBaseline) > 0 {
		previousVisible = m.restoredPaneBaseline
	}
	m.restoredPaneBaseline = nil

	nextVisible := m.store.VisibleOpenPanes(lay.PaneCount())
	if hidden := newlyAutoHiddenPane(previousVisible, nextVisible, m.store.OpenPanes()); hidden != nil {
		m.setPaneAutoHideStatus(hidden, m.store.NumOpenPanes())
	}
	m.visiblePanes = nextVisible

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

	m.layoutModalOverlays()

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

func newlyAutoHiddenPane(previousVisible, nextVisible, openPanes []*store.OpenPane) *store.OpenPane {
	if len(previousVisible) == 0 || len(openPanes) <= len(nextVisible) {
		return nil
	}
	open := make(map[*store.OpenPane]bool, len(openPanes))
	for _, p := range openPanes {
		open[p] = true
	}
	visible := make(map[*store.OpenPane]bool, len(nextVisible))
	for _, p := range nextVisible {
		visible[p] = true
	}
	for _, p := range previousVisible {
		if p != nil && open[p] && !visible[p] {
			return p
		}
	}
	return nil
}

func (m *home) setPaneAutoHideStatus(p *store.OpenPane, paneCount int) {
	if p == nil || paneCount <= 1 {
		return
	}
	msg := fmt.Sprintf("%s hidden: terminal too narrow for %d panes; resize wider%s",
		paneStatusTitle(p), paneCount, paneRecoveryStatusHint())
	m.pendingPaneAutoHideStatus = msg
	m.setTransientNotice(errors.New(msg))
}

func paneStatusTitle(p *store.OpenPane) string {
	if p == nil || p.Instance() == nil || p.Instance().Title == "" {
		return "pane"
	}
	return p.Instance().Title
}

func paneRecoveryStatusHint() string {
	if key := bindingKeyWithDesc("pane list"); key != "" {
		return fmt.Sprintf(" or use `%s` pane list", key)
	}
	if binding, ok := keys.GlobalKeyBindings[keys.KeyOpenPane]; ok {
		help := binding.Help()
		if help.Key != "" && help.Desc != "" {
			return fmt.Sprintf(" or use `%s` %s", help.Key, help.Desc)
		}
	}
	return ""
}

func bindingKeyWithDesc(desc string) string {
	for _, binding := range keys.GlobalKeyBindings {
		help := binding.Help()
		if help.Desc == desc && help.Key != "" {
			return help.Key
		}
	}
	return ""
}

func (m *home) consumePaneAutoHideStatus() tea.Cmd {
	if m.pendingPaneAutoHideStatus == "" {
		return nil
	}
	status := m.pendingPaneAutoHideStatus
	m.pendingPaneAutoHideStatus = ""
	return m.showTransientError(errors.New(status))
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
		m.lastFocusedPaneID = p.ID()
	}
	m.menu.SetFocusRegion(active)
	m.syncSplitPaneHint()
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
	if m.panePreviewTxn != nil {
		m.suppressActivePanePreview()
		m.cancelPanePreview(true)
		return m.panesRefresh(m.attached.Load())
	}
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
