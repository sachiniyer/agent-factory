package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

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
	"github.com/sachiniyer/agent-factory/ui/tree"
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
	// stateSelectTabKind is the state when the user is choosing which kind of tab
	// the new-tab action should create.
	stateSelectTabKind
	// statePromptInput is the state when the initial-prompt field of the naming
	// form is open (#1936). Like stateSelectProgram it is a sub-state of
	// stateNew: closing it returns to naming rather than to stateDefault.
	statePromptInput
	// stateHooks is the state when the post-worktree hooks editor overlay is
	// open (#1024 PR 4: hooks lost their persistent sidebar slot and are
	// hosted as a modal overlay instead).
	stateHooks
	// stateTasks is the state when the task manager (list + create/edit form)
	// overlay is open. The in-rail automations section shows only the compact
	// summary (#1087 play-test): the full manager gets a centered overlay so
	// its form is never clamped into the narrow rail.
	stateTasks
	// stateConfigEditor is the state when the global config editor overlay is
	// open (","). Like the hooks and tasks overlays it owns the keyboard while
	// open, so its value field can take arbitrary text (a listen address, a
	// branch prefix) without the global key map eating the runes.
	stateConfigEditor
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
	// previewFetcher captures a session tab's content through the daemon — the sole
	// capturer since #1592 Phase 2 PR6 (the TUI no longer shells out to tmux
	// capture-pane). It backs TabPane's render path for content not streamed live
	// over WS (remote/hook, scroll-mode scrollback, the transient preview target).
	// A PER-home field for the same off-loop-race reason as snapshotFetcher:
	// TabPane's capture runs on the refreshPaneBindingCmd goroutine, so a package
	// global swapped by a test would race under `go test -parallel`. Defaults to
	// previewThroughDaemon; tests assign a fake directly.
	previewFetcher func(req daemon.PreviewRequest) (content string, gone bool, err error)
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
	// releaseTerminal / restoreTerminal hand the REAL terminal to a full-screen
	// attach and take it back (#2157). They are Bubble Tea's own
	// Program.ReleaseTerminal / RestoreTerminal, wired in Run; PER-home fields
	// rather than package globals for the same reason as the seams above, and nil
	// in tests, which drive attachOverlayCallback without a Program or a tty.
	// See releaseTerminalToAttach for why a raw-proxy attach must have them.
	releaseTerminal func() error
	restoreTerminal func() error
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
	// configAgentSpawning is the in-flight guard for the config-agent hotkey.
	// The spawn is a daemon round trip that waits out the agent's readiness
	// budget (60s), during which the TUI shows nothing — so without this a user
	// who presses C again gets a SECOND config agent, and a third. Mirrors the
	// attachTransitioning re-entry guard (#1530), which exists for exactly this
	// reason on the attach path. Cleared when the spawn reports back.
	configAgentSpawning bool
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
	// automations → projects (#1088, #1588 follow-up). Tab/Shift-Tab cycle it;
	// its pane entries are rebuilt by relayout as panes open, close, and
	// auto-hide, and regions hidden by the degradation ladder are skipped.
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
	// paneAutoHideNoticeID is the transient-notice generation of the currently
	// displayed "N hidden: terminal too narrow" status, or 0 when none is shown.
	// relayout clears the notice the moment a resize fits every open pane again,
	// so the guidance never lingers on screen contradicting the visible panes
	// (#1557). Tracked by id (not content) so a newer, unrelated notice that
	// superseded it is never wiped.
	paneAutoHideNoticeID uint64
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
	// selectionEpoch bumps every time the tree selection genuinely moves to a
	// different row. It is the TUI twin of the web's layoutGeneration (#1862): an
	// explicit pane mutation pins the epoch it happened in, and a tree-cursor
	// preview that would move that pane is held off while the epoch is unchanged —
	// so a pane-focused 1-9 jump is a COMMIT the trailing selectionChanged /
	// background tick cannot repaint away (#1885). A real navigation bumps the
	// epoch, staling the pin, and previews resume.
	selectionEpoch uint64
	// lastSelectionKey is the identity of the tree row selectionChanged last saw,
	// used to decide whether the selection genuinely moved (and so whether to bump
	// selectionEpoch). The jump's own selectionChanged leaves the cursor put, so
	// the key is unchanged and the pin survives.
	lastSelectionKey string
	// paneJumpIntent records, per pane id, the selectionEpoch at which the pane
	// was last explicitly jumped (1-9). While the entry equals the current
	// selectionEpoch the pane's committed tab is pinned intent: updatePanePreview
	// refuses to preview it onto the tree cursor's divergent tab (#1885).
	paneJumpIntent map[int]uint64
	// inPreviewTick is set while a BACKGROUND REFRESH drives selectionChanged —
	// the idle 100ms preview tick (#1558) or a daemon snapshot poll (#1603). It
	// gates the one side effect that must never fire from a background refresh:
	// pulling focus onto the selected instance's already-open pane. Without the
	// gate the tick yanked focus back to that pane the moment the user Tabbed off
	// it, so the focus ring could never traverse the other panes or rest on the
	// tree (#1558); a snapshot poll did the same on any out-of-band change
	// (#1603). User-driven selectionChanged calls (nav, the open-or-focus verb)
	// leave it false and keep the focus-steal behavior.
	inPreviewTick bool
	// -- Live embedded terminals (#1592 Phase 2 PR6, WS PTY stream) --
	//
	// EVERY visible, eligible pane holds its own live termpane attachment: a
	// reconnecting WebSocket subscription to that pane's (session, tab) PTY
	// stream, fanned from the daemon's clientless capture (§6). The window
	// renders the termpane grid instead of a daemon-Preview capture; a WS drop
	// reconnects and replays via ?since, so there is NO capture fallback and no
	// rebind-retry loop (the reliability payoff over the old tmux attach client).
	// Lifecycle in live_termpane.go; both maps are event-loop only.

	// liveTerms maps an open pane's id to its live attachment (a *termpane.TermPane
	// in production, behind the seam interface).
	liveTerms map[int]liveTermAttachment
	// liveKeys maps a pane id to the (id/tab/session) binding key its attachment
	// was created for, so the sync only rebinds when the binding actually changed.
	liveKeys map[int]string
	// pendingTUIViewFocus is loaded before Bubble Tea reports the terminal size.
	// The pane bindings can restore immediately, but a pane focus target is only
	// focusable after the first non-fallback relayout has rebuilt the ring.
	pendingTUIViewFocus *config.TUIStateFocus

	// interactive is the two-mode keyboard switch (#1089 PR 2, RFC §2.3).
	// Nav mode (false): the host owns the keyboard — focus ring, verbs,
	// overlays, exactly as before. Interactive mode (true): EVERY keystroke
	// (including Tab) forwards down the focused pane's live attachment; the
	// only host-reserved key is Ctrl-], which returns to nav. The mode is
	// only ever true while the focused pane has a live attachment —
	// enforceInteractiveInvariant drops it the moment that premise breaks.
	// Orthogonal to `state`: overlays opened by async events still own the
	// keyboard (handleKeyPress checks state alongside this flag). Event-loop
	// only; the pane's green frame and the status bar mirror it.
	interactive bool

	// interactivePauseTitle is the session whose #1160 capture-poll pause lease
	// this TUI currently holds because the user is typing into it through the
	// FOCUSED embedded interactive pane (#1586). Empty when not interactively
	// focused on a local session. Holding the lease makes the daemon treat the
	// session as attached and DEFER automated task deliveries (cron/watch) into
	// it, so a scheduled prompt can't paste into and submit the user's
	// in-progress input — the same guarantee full-screen attach already had
	// (attachOverlayCallback), extended to the common in-pane flow. Renewed on
	// the preview tick and released when interactive mode ends;
	// interactivePauseAt throttles the renew to statusPollRenewInterval.
	// Event-loop only.
	interactivePauseTitle string
	interactivePauseAt    time.Time

	// initialPaneOpened latches the one-time startup auto-open: the first
	// instance selection opens its pane so the workspace isn't empty on
	// launch. Never reset — once the user has hidden every pane, the
	// workspace stays empty until they open one (`s`).
	initialPaneOpened bool
	// automations is the bottom section of the left rail (#1087): compact
	// task rows only — S/Enter open its full task manager as the stateTasks
	// overlay
	automations *ui.AutomationsPane
	// projects is the bottom-most section of the left rail (#1588 follow-up): a
	// peer of the automations section, BELOW it, that the focus ring Tabs into
	// (tree → panes → automations → projects → tree). Its rows list the projects
	// af has seen; Enter on the cursor row switches the rail to that project via
	// the #1547 switchProject path.
	projects *ui.ProjectsPane
	// statusBar merges the menu hints and the error line
	statusBar *ui.StatusBar
	// hooksPane is the post-worktree hooks editor, hosted as an overlay
	// (stateHooks)
	hooksPane *ui.HooksPane
	// configPane is the global config editor, hosted as an overlay
	// (stateConfigEditor). It renders from the config manifest and writes
	// through config.SetGlobalConfigValue — the same path `af config set` uses.
	configPane *ui.ConfigPane
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
	// selectionOverlay handles short enum choices: program selection during
	// new-instance naming and tab-kind selection from the new-tab action.
	selectionOverlay *overlay.SelectionOverlay
	// tabCreateTitle identifies the session that opened the tab-kind picker.
	// Background snapshots may move the sidebar selection or replace an instance
	// pointer while the modal is open, so submit re-resolves this stable title.
	tabCreateTitle string
	// searchOverlay handles session search
	searchOverlay *overlay.SearchOverlay
	// projectPickerOverlay handles switching the active project (#1461)
	projectPickerOverlay *overlay.ProjectPickerOverlay
	// pendingProgram tracks the program selected during new instance naming
	pendingProgram string
	// promptOverlay handles initial-prompt entry during new-instance naming
	// (#1936).
	promptOverlay *overlay.PromptOverlay
	// pendingPrompt tracks the initial prompt typed during new instance naming.
	// It is the value handleStateNew puts on sessionStartRequest.Prompt, which
	// the daemon delivers to the agent once it is ready — the same field
	// `af sessions create --prompt` fills. Reset by startNewInstance so a
	// cancelled create can never leak its prompt into the next one.
	pendingPrompt string

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
		store:            proj,
		menu:             menu,
		errBox:           errBox,
		paneWindows:      make(map[int]*ui.TabbedWindow),
		lastPaneCapture:  make(map[int]time.Time),
		paneJumpIntent:   make(map[int]uint64),
		liveTerms:        make(map[int]liveTermAttachment),
		liveKeys:         make(map[int]string),
		automations:      ui.NewAutomationsPane(proj),
		projects:         ui.NewProjectsPane(),
		statusBar:        ui.NewStatusBar(menu, errBox),
		hooksPane:        ui.NewHooksPane(),
		configPane:       ui.NewConfigPane(),
		ring:             layout.NewRing(layout.RegionTree, layout.RegionAutomations, layout.RegionProjects),
		zones:            zones.NewRegistry(),
		mouseClock:       time.Now,
		snapshotFetcher:  snapshotThroughDaemon,
		previewFetcher:   previewThroughDaemon,
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
	h.sidebar = ui.NewSidebar(autoYes, proj)
	h.wireZoneRegistry()
	// No panes are open at startup: the focus ring is tree → automations →
	// projects until the first pane opens (relayout rebuilds the ring's pane
	// entries thereafter; the first instance selection auto-opens its pane).
	h.syncFocus()

	// Cold-start the projection from the daemon's authoritative Snapshot (#960 PR 6).
	// The TUI no longer reads instances.json — the daemon is the sole writer/owner
	// of session state, so startup mirrors the same projection the refresh tick
	// reconciles against. A warming daemon (#829) is waited out, not raced.
	if err := h.coldStartFromSnapshot(); err != nil {
		fmt.Printf("Failed to load sessions from daemon: %v\n", err)
		os.Exit(1)
	}

	h.restoreTUIViewStateOnLaunch()
	// Populate the sidebar's Projects section from the cross-repo discovery so it
	// renders (collapsed) at launch with the active project marked.
	h.refreshSidebarProjects()

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
	// Size the Projects section to its content the same way (#1588 follow-up):
	// the grid grows it to show every project the rail has room for.
	m.grid.Projects = len(m.projects.Projects())
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
	// terminal can't fit (#1535). Clear it ONLY when it is actually consumed
	// (previousVisible empty): an intermediate relayout that reaches here with a
	// real previousVisible already established leaves nothing to consume, and one
	// that falls through to fallback returned above — so the baseline survives
	// every relayout until the first sized one uses it, instead of being cleared
	// out from under that resize (#1551 review). Since visiblePanes is nil until
	// the first non-fallback relayout, that first sized relayout is exactly the
	// one that consumes it.
	if len(previousVisible) == 0 && len(m.restoredPaneBaseline) > 0 {
		previousVisible = m.restoredPaneBaseline
		m.restoredPaneBaseline = nil
	}

	nextVisible := m.store.VisibleOpenPanes(lay.PaneCount())
	if hidden := newlyAutoHiddenPane(previousVisible, nextVisible, m.store.OpenPanes()); hidden != nil {
		m.setPaneAutoHideStatus(hidden, m.store.NumOpenPanes())
	}
	m.visiblePanes = nextVisible
	m.clearStaleAutoHideStatus()

	// Rebuild the ring's pane entries to the visible set (auto-hidden panes
	// leave the ring; the focused pane is most-recently-focused, so it is
	// never the one auto-hidden). SetIDs keeps the active id when it
	// survives; a vanished active falls back to the tree.
	ids := make([]string, 0, len(m.visiblePanes)+3)
	ids = append(ids, layout.RegionTree)
	for _, p := range m.visiblePanes {
		ids = append(ids, layout.PaneRegion(p.ID()))
	}
	// Projects follows automations so forward Tab is tree → panes → automations
	// → projects → (wrap) tree (#1588 follow-up).
	ids = append(ids, layout.RegionAutomations, layout.RegionProjects)
	m.ring.SetIDs(ids...)
	m.ring.SetHidden(layout.RegionAutomations, !lay.AutomationsVisible)
	m.ring.SetHidden(layout.RegionProjects, !lay.ProjectsVisible)
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
	m.projects.SetRect(lay.Projects)
	m.projects.SetCompact(lay.ProjectsCompact)
	m.statusBar.SetRect(lay.StatusBar)
	m.alarmBanner.SetRect(lay.Banner)

	m.layoutModalOverlays()

	// Live panes size their sessions over the WS stream (last-resize-wins
	// resize-window, #1592 Phase 2 PR6): each attachment's Resize rides the pane
	// geometry through SetRect → w.live.Resize, so the TUI no longer resizes local
	// tmux sessions from the relayout.
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

// setPaneAutoHideStatus reports the pane the terminal could not fit. It names
// the pane that was ACTUALLY displaced — `instance · tab`, the same identity the
// pane's own header shows — or says nothing about which pane when it cannot name
// one. The instance title alone is not a pane identity: since #930 an instance
// can own several panes, so "docs hidden" while a second `docs` pane is on
// screen tells the user something they can see is false (#1997).
//
// The reason clause drops the word "terminal" to pay for the tab name: at 80
// columns the old line already sat one cell under the limit, and the bar
// truncates from the RIGHT, so a longer line silently eats the recovery hint
// (#1973). Long instance titles still overflow — nothing can fit an unbounded
// title — but the fragments are ordered worst-first, so what survives the
// truncation is the half that matters: which pane went away.
func (m *home) setPaneAutoHideStatus(p *store.OpenPane, paneCount int) {
	if p == nil || paneCount <= 1 {
		return
	}
	subject := "a pane is hidden"
	if label, ok := paneStatusLabel(p); ok {
		subject = label + " hidden"
	}
	msg := fmt.Sprintf("%s — too narrow for %d panes; resize wider%s",
		subject, paneCount, paneRecoveryStatusHint())
	m.pendingPaneAutoHideStatus = msg
	m.paneAutoHideNoticeID = m.setTransientNotice(errors.New(msg))
}

// clearStaleAutoHideStatus drops the "N hidden: terminal too narrow" guidance
// the moment a relayout fits every open pane again — otherwise a resize wide
// enough to reveal the auto-hidden panes still leaves the narrow-width status
// on the bar, contradicting the now-visible panes (#1557). Keyed on the notice
// id so a newer, unrelated status that superseded ours is never wiped; the
// guidance's own transient timer still handles the case where it is the current
// notice but the panes never came back.
func (m *home) clearStaleAutoHideStatus() {
	if m.paneAutoHideNoticeID == 0 || m.store.NumOpenPanes() > len(m.visiblePanes) {
		return
	}
	if m.transientNoticeID == m.paneAutoHideNoticeID {
		m.errBox.Clear()
	}
	m.paneAutoHideNoticeID = 0
	m.pendingPaneAutoHideStatus = ""
}

// paneStatusLabel names a pane for a user-facing message the way the pane's own
// header names it — `instance · tab` (ui.TabbedWindow.renderHeader), reading the
// tab through the same tree label source so the toast and the header can never
// disagree about what a pane is called.
//
// It reports false rather than guessing. An instance title alone is not a pane
// identity (#930), and tree.TabLabels answers with a placeholder "Agent" slot
// for an instance whose tabs have not materialized — so the tab is read through
// TabLabelAt, which distinguishes a real tab from "no tab list yet". A caller
// that cannot name the pane must say so instead of naming the wrong one (#1997).
func paneStatusLabel(p *store.OpenPane) (string, bool) {
	if p == nil || p.Instance() == nil || p.Instance().Title == "" {
		return "", false
	}
	label, ok := tree.TabLabelAt(p.Instance(), p.Tab())
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%s · %s", p.Instance().Title, label), true
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
	m.paneAutoHideNoticeID = m.setTransientNotice(errors.New(status))
	return m.clearTransientMessageAfterDelay(m.paneAutoHideNoticeID)
}

// syncFocus applies the focus ring's active region to the panes and the
// status-bar hints, and stamps a focused pane most recently focused so the
// §2.6 auto-hide order tracks real attention.
func (m *home) syncFocus() {
	active := m.ring.Active()
	panes := map[string]layout.Pane{
		layout.RegionTree:        m.sidebar,
		layout.RegionAutomations: m.automations,
		layout.RegionProjects:    m.projects,
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
	// A live pane preview is transient chrome, not a focus stop. Tab / Shift-Tab
	// must DISMISS it and still advance the ring one step — never swallow the
	// keystroke (#1705). Earlier this early-returned after cancelling the
	// preview, so with a preview live (which the idle tick creates whenever the
	// tree cursor names a tab the owner pane isn't bound to) reverse traversal
	// out of a pane was impossible: the ring never moved.
	//
	// The step anchors on ring.Active() — cancelPanePreview(false), NOT
	// cancelPanePreview(true) — so a Tab pressed from the tree while a
	// background preview happens to be owned by some pane still steps from the
	// tree, not from that pane.
	var refresh tea.Cmd
	if m.panePreviewTxn != nil {
		m.suppressActivePanePreview()
		m.cancelPanePreview(false)
		refresh = m.panesRefresh(m.attached.Load())
	}
	if back {
		m.ring.Prev()
	} else {
		m.ring.Next()
	}
	m.relayout()
	return refresh
}

// focusAdjacentSection moves focus to the next / previous SECTION region — the
// non-pane focus-ring stops: the instances tree, the automations rail, and the
// projects rail. Workspace panes are skipped (Tab steps through those, 1-9 jump
// to tabs). This is what the `]` / `[` "next / prev section" bindings drive
// (#1706): automations and projects became their own Tab-focusable rail
// sections (#1470 / #1588 / #1590) rather than sidebar section headers, so the
// old header-walking JumpNextSection could never reach them — from an instance
// row `]` was a silent no-op. Stepping the real focus ring makes `]` land on
// Automations (dropping the instance-only `D kill` from the footer) exactly as
// the binding advertises, and `[` walks back.
func (m *home) focusAdjacentSection(back bool) tea.Cmd {
	// Mirror cycleFocus: a live preview is transient chrome and must be
	// dismissed without swallowing the keystroke (#1705 class).
	var refresh tea.Cmd
	if m.panePreviewTxn != nil {
		m.suppressActivePanePreview()
		m.cancelPanePreview(false)
		refresh = m.panesRefresh(m.attached.Load())
	}
	// Step the ring, skipping workspace panes, until it rests on a section
	// region. The tree is always a visible non-pane stop, so this always
	// terminates; bound the loop by the ring size as a backstop.
	for i := 0; i < len(m.visiblePanes)+3; i++ {
		var id string
		if back {
			id = m.ring.Prev()
		} else {
			id = m.ring.Next()
		}
		if id == "" || !layout.IsPaneRegion(id) {
			break
		}
	}
	m.relayout()
	return refresh
}

func (m *home) Init() tea.Cmd {
	return tea.Batch(
		func() tea.Msg {
			time.Sleep(100 * time.Millisecond)
			return previewTickMsg{}
		},
		tickUpdatePRInfoCmd,
		tickRefreshExternalCmd,
	)
}
