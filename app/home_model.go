package app

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
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

type home struct {
	failedCreate         *instanceStartedMsg
	recovery             *recoveryNotice
	snapshotUnavailable  bool
	snapshotFailureSince *time.Time
	snapshotClock        func() time.Time // nil uses time.Now; injected by snapshot recovery tests.
	ctx                  context.Context

	// -- Storage and Configuration --

	programChoice
	repoID string
	// repoRoot is the main-worktree root of the repo this TUI run is scoped
	// to. Used to resolve and persist the in-repo .agent-factory/config.json.
	repoRoot string
	// projectPathResolutions retains successful Git identity lookups across the
	// 750ms Projects poll. It is event-loop-owned like the rest of home state.
	projectPathResolutions map[string]projectPathResolution
	// registeredProjectIdentities retains PROVEN registry identities across the
	// same poll — a registered root whose exact workspace and checkout marker
	// Git still vouches for. Kept apart from projectPathResolutions because a
	// generic path resolution is not evidence about a durable registration.
	registeredProjectIdentities map[string]registeredProjectIdentity

	// adoptedSnapshotOps separates daemon-adopted in-flight ops from local
	// optimistic ones, which the reconcile guards must treat oppositely (#3005).
	// The reasoning lives on the type, in app/adopted_ops.go.
	adoptedSnapshotOps adoptedOps

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
	previewFetcher func(req daemon.PreviewRequest) (daemon.PreviewResponse, error)
	// pauseStatusPoll / resumeStatusPoll are the daemon poll-pause seams for the
	// attach heartbeat (#1160). PER-home fields, not package globals, for the
	// same reason as snapshotFetcher: the heartbeat reads the seam from an
	// off-loop goroutine, so a shared mutable global swapped by a test would race
	// that goroutine against a sibling test's swap under `go test -parallel
	// -race` (the #964 / #960-PR4 snapshot-fetcher race). Each home owns its
	// seams; the goroutine captures them into locals at spawn so it never touches
	// shared home state mid-flight. Default to pauseStatusPollThroughDaemon /
	// resumeStatusPollThroughDaemon in production; tests assign fakes directly.
	pauseStatusPoll  func(daemon.PauseStatusPollRequest) error
	resumeStatusPoll func(daemon.ResumeStatusPollRequest) error
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
	// namingPlaceholder is the random "adjective-noun" name shown as shadow text
	// while namingInstance has an empty title (#2470). Pressing enter on the
	// untouched field adopts it as the session name (the "autocreate"); it is
	// generated once per naming in startNewInstance and cleared with namingInstance.
	namingPlaceholder string

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
	// store.OpenPane binding; commit/cancel semantics live in pane_preview.go.
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

	// interactivePauseTarget is the session whose #1160 capture-poll pause lease
	// this TUI currently holds because the user is typing into it through the
	// FOCUSED embedded interactive pane (#1586). Zero when not interactively
	// focused on a local session. Holding the lease makes the daemon treat the
	// session as attached and DEFER automated task deliveries (cron/watch) into
	// it, so a scheduled prompt can't paste into and submit the user's
	// in-progress input — the same guarantee full-screen attach already had
	// (attachOverlayCallback), extended to the common in-pane flow. Renewed on
	// the preview tick and released when interactive mode ends;
	// interactivePauseAt throttles the renew to statusPollRenewInterval.
	// Event-loop only.
	interactivePauseTarget sessionActionTarget
	interactivePauseHolder string // per-lifecycle lease holder; see interactivePollPauseCmd (#3027)
	interactivePauseAt     time.Time

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
	// (stateConfigEditor). Both halves follow --daemon-url/AF_DAEMON_URL since
	// #3708 — ui.ReadConfigForEditor fills its rows and its save writes back to
	// the same daemon — so `,` administers whichever daemon this session is
	// attached to, as `af config set` has since #3679.
	configPane *ui.ConfigPane
	// Identifies the current opening and its latest remote accounts operation.
	accountGeneration uint64
	// A remote mutation outlives the overlay and is released only on completion.
	accountRegisterInFlight *daemon.RegisterAccountRequest
	// menu displays the key hints inside the status bar (shared handle for
	// SetState/keydown callers)
	menu *ui.Menu
	// errBox displays error messages inside the status bar (shared handle)
	errBox *ui.ErrBox
	// transientNoticeID is a generation token for the status-bar notice timer.
	// Each new error/success notice increments it; a stale hideErrMsg from an
	// older timer must not clear a newer notice.
	transientNoticeID uint64
	namingNotice      namingFormNotice // a notice the open naming form raised (#4123)
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
	// textOverlayPendingSeenMask holds the one-shot help mask of the overlay
	// currently on screen. It is written to app state when the user DISMISSES
	// the overlay, never when it is displayed (#3628).
	textOverlayPendingSeenMask uint32
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
	// tabCreateTarget identifies the session that opened the tab-kind picker.
	// Background snapshots may move the sidebar selection, replace its pointer,
	// or reuse its display title while the modal is open (#2358).
	tabCreateTarget sessionActionTarget
	// tabRenameTarget identifies the session AND the tab the rename prompt will
	// act on, captured when the prompt opens for the same reason as
	// tabCreateTarget — plus the roster generation, because a pre-#1738 tab has
	// no stable id and the captured name can only be trusted while the roster
	// provably has not changed (the same rule delete consent applies, #2358).
	tabRenameTarget tabRenameRef
	// searchOverlay handles session search
	searchOverlay *overlay.SearchOverlay
	// projectPickerOverlay handles switching the active project (#1461)
	projectPickerOverlay *overlay.ProjectPickerOverlay
	// handoffChoices is the agent list currently offered by the handoff picker
	// (#2013). It is held alongside the overlay because the list is FILTERED (it
	// omits the running agent), so the overlay's selected index cannot be mapped
	// back through tmux.SupportedPrograms the way the create-time picker's can.
	handoffChoices  []string
	handoffAccounts []string
	handoffWarnings []string
	// handoffResolvedAgents is the picker load's resolved_agents, kept for the confirm step's carry-intended judgment (#4367/#4504); nil -> enum like resolvedFor.
	handoffResolvedAgents map[string]string
	// handoffTarget is the immutable session identity that opened the picker.
	// Background snapshots may move the sidebar cursor or replace a same-title
	// row while the modal owns the keyboard; submit must never re-read that
	// mutable selection and retarget a destructive runtime swap (#2322).
	handoffTarget handoffPickerTarget
	// handoffResolve is the resolve-delivery picker's retained state (#4429).
	handoffResolve handoffResolveState
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
	// pendingBackend tracks the backend picked during naming (#1933). "" means
	// "let the repo config decide", which is what CreateSessionRequest.Backend
	// omits on — the same defaulting `af sessions create` gets with no --backend,
	// so an untouched field keeps today's behavior byte-identical. Reset by
	// startNewInstance so a cancelled create cannot leak a backend into the next.
	pendingBackend string
	// backendPickerPending prevents submitting or switching fields before the
	// configured new_remote binding has delivered its promised backend picker.
	backendPickerPending bool
	// pendingAccount tracks the credential account picked during naming (#3844).
	// "" means "the ambient identity", which is what CreateSessionRequest.Account
	// omits on — the same default `af sessions create` gets with no --account, so
	// an untouched field keeps today's behaviour byte-identical. It is CLEARED
	// whenever the program changes, because an account belongs to one agent:
	// claude's "work" and codex's "work" are different identities in different
	// registries. Reset by startNewInstance so a cancelled create cannot leak an
	// identity into the next one.
	pendingAccount string
	// pendingAccountChosen records that the USER decided this form's account,
	// rather than it being the project default the daemon preselected (#3386).
	//
	// The two must be distinguishable, and pendingAccount alone cannot do it: the
	// ambient identity IS the empty string, so "the user picked ambient" and "no
	// answer yet" have the same value. The default fetch is asynchronous, so
	// without this flag a preselection landing a moment after a deliberate pick
	// would silently replace it — putting the session on an identity the user had
	// just chosen against, which is the whole failure this field exists to prevent.
	pendingAccountChosen bool
	// pendingProgramChosen records that the USER confirmed this form's program
	// through the picker (#4889 review) — including deliberately re-picking the
	// value already shown. A confirmed choice submits the concrete program on
	// the wire; an untouched config-derived seed submits "" so the daemon
	// resolves default_program at create time. String equality cannot see the
	// re-picked-same-value case — this flag is what can. Reset by
	// startNewInstance; set by a program-picker submit and by a failed
	// create's draft restore.
	pendingProgramChosen bool
	// backendPickerChoices is the option list the open backend picker is showing,
	// held alongside the overlay for the same reason handoffChoices is: the list is
	// built from the daemon's response (plus a leading "repo default" row), so the
	// overlay's index cannot be mapped back through any local enum.
	backendPickerChoices []backendChoice
	// accountPickerChoices is the option list the open account picker is showing,
	// held alongside the overlay for the same reason backendPickerChoices is: the
	// rows are built from the daemon's registry (plus a leading "ambient identity"
	// row), so the overlay's index cannot be mapped back through any local list.
	accountPickerChoices []accountChoice
	// attached is set while the user is inside an attached tmux session.
	// While true, periodic background work that hits the shared tmux server
	// (capture-pane via runMetadataTick, refreshPanesCmd) is
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

func newHome(ctx context.Context, program, configuredDefault string, repo *config.RepoContext) *home {
	// repo is nil when af was launched outside a git repository (#2477): the TUI
	// opens in registry mode with no active project, and the user selects one
	// from the Projects section. An empty repoID makes the cold-start snapshot an
	// all-repos fetch, and an empty repoRoot skips the repo-scoped config below.
	var repoID, repoRoot string
	if repo != nil {
		repoID = repo.ID
		repoRoot = repo.Root
	}
	// Load application config
	appConfig, err := config.LoadConfig()
	if err != nil {
		fmt.Printf("Failed to load config: %v\n", err)
		os.Exit(1)
	}
	ui.ApplyAppearance(appConfig.Appearance)
	applyTheme()

	// Apply configured detach key. The loader (sanitizeDetachKeys) already
	// warns-and-defaults a bad hand-edited value, so this parse normally
	// succeeds; if a value ever reaches here unparseable, fall back to the
	// default rather than crashing the TUI on a config typo (#2556).
	if appConfig.DetachKeys != "" {
		detachKeys := appConfig.DetachKeys
		b, err := config.ParseDetachKey(detachKeys)
		if err != nil {
			log.WarningLog.Printf("invalid detach_keys %q (%v); using default", detachKeys, err)
			detachKeys = config.DefaultDetachKeys()
			b, _ = config.ParseDetachKey(detachKeys)
		}
		tmux.SetDetachKey(b, detachKeys)
	}

	// Load application state (seen-help-screens flags; #960 PR 6 the TUI no
	// longer reads instances.json — the daemon owns it and answers Snapshot).
	appState := config.LoadState()

	proj := store.NewProjection()
	menu := ui.NewMenu()
	errBox := ui.NewErrBox()

	h := &home{
		adoptedSnapshotOps:          adoptedOps{},
		alarmBanner:                 ui.NewAlarmBanner(),
		ctx:                         ctx,
		store:                       proj,
		menu:                        menu,
		errBox:                      errBox,
		paneWindows:                 make(map[int]*ui.TabbedWindow),
		lastPaneCapture:             make(map[int]time.Time),
		paneJumpIntent:              make(map[int]uint64),
		liveTerms:                   make(map[int]liveTermAttachment),
		liveKeys:                    make(map[int]string),
		automations:                 ui.NewAutomationsPane(proj),
		projects:                    ui.NewProjectsPane(),
		statusBar:                   ui.NewStatusBar(menu, errBox),
		hooksPane:                   ui.NewHooksPane(),
		configPane:                  ui.NewConfigPane(),
		ring:                        layout.NewRing(layout.RegionTree, layout.RegionAutomations, layout.RegionProjects),
		zones:                       zones.NewRegistry(),
		mouseClock:                  time.Now,
		snapshotFetcher:             snapshotThroughDaemon,
		previewFetcher:              previewThroughDaemon,
		pauseStatusPoll:             pauseStatusPollThroughDaemon,
		resumeStatusPoll:            resumeStatusPollThroughDaemon,
		appConfig:                   appConfig,
		programChoice:               newProgramChoice(program, configuredDefault, appConfig),
		repoID:                      repoID,
		repoRoot:                    repoRoot,
		projectPathResolutions:      make(map[string]projectPathResolution),
		registeredProjectIdentities: make(map[string]registeredProjectIdentity),
		state:                       stateDefault,
		appState:                    appState,
	}
	h.sidebar = ui.NewSidebar(proj)
	h.wireZoneRegistry()
	// No panes are open at startup: the focus ring is tree → automations →
	// projects until the first pane opens (relayout rebuilds the ring's pane
	// entries thereafter; the first instance selection auto-opens its pane).
	//
	// In registry mode (launched outside a repo, #2477) the session tree is empty
	// — there is no active project — so land focus on the Projects section, the
	// surface the user picks a project from, rather than the empty tree. The ring
	// position survives the first relayout (RegionProjects stays visible), and
	// there is no saved view state to restore over it (repoID is empty).
	// Deferred until after refreshSidebarProjects below: focusing this section is
	// only useful when it HAS a row to pick. With none, it is a captive list where
	// Enter is a no-op and ctrl+p is suppressed (#1620), so landing there gives a
	// first-time user a section with no live key at all (#2830).
	h.syncFocus()

	// Cold-start the projection from the daemon's authoritative Snapshot (#960 PR 6).
	// The TUI no longer reads instances.json — the daemon is the sole writer/owner
	// of session state, so startup mirrors the same projection the refresh tick
	// reconciles against. A warming daemon (#829) is waited out, not raced.
	//
	// Skipped in registry mode (launched outside a repo, #2477): with no active
	// project the session rail is intentionally empty until one is selected.
	// Cold-starting here would use an empty repoID, which the daemon answers with
	// the CROSS-REPO snapshot — listing sessions the empty active scope cannot act
	// on, because every per-session op is keyed to m.repoID and would silently
	// no-op (resolveSessionActionTarget requires target.repoID == m.repoID). An
	// empty rail keeps the mode coherent; switchProject cold-starts the chosen
	// project's sessions when one is selected.
	if repoRoot != "" {
		if err := h.coldStartFromSnapshot(); err != nil {
			log.WarningLog.Printf("failed to load sessions from daemon: %v", err)
		}
	}

	h.restoreTUIViewStateOnLaunch()
	// Populate the sidebar's Projects section from the cross-repo discovery so it
	// renders (collapsed) at launch with the active project marked.
	h.refreshSidebarProjects()
	// In registry mode (launched outside a repo, #2477) the session tree is empty
	// — there is no active project — so land focus on the Projects section, the
	// surface the user picks one from. Only when it has rows: see above. The ring
	// position survives the first relayout (RegionProjects stays visible), and
	// there is no saved view state to restore over it (repoID is empty).
	if repoRoot == "" && h.projects.HasProjects() {
		h.ring.Focus(layout.RegionProjects)
		h.syncFocus()
	}

	// Load tasks for sidebar display. Skipped in registry mode (#2477): tasks are
	// per-project and LoadTasksForCurrentRepo needs a cwd repo, so with no active
	// project the automations strip stays empty — not errored — until one is
	// selected, matching the empty session rail. switchProject loads the chosen
	// project's tasks then.
	if repoRoot != "" {
		tasks, err := task.LoadTasksForCurrentRepo()
		if err != nil {
			log.WarningLog.Printf("failed to load tasks: %v", err)
			h.automations.TaskPane().SetUnavailable(err)
		} else {
			h.store.SetTasks(tasks)
			// Load tasks into the automations strip's task manager.
			if len(tasks) > 0 {
				h.automations.TaskPane().SetTasks(tasks)
			}
		}
	}

	// Load hooks for the hooks overlay. ResolveConfig applies the in-repo
	// .agent-factory/config.json over the legacy per-repo file. Skipped in
	// registry mode (no active repo, #2477): there are no repo-scoped hooks to
	// show until a project is selected, and switchProject re-resolves them then.
	if repo != nil {
		repoCfg, err := config.ResolveConfigForRepo(repo)
		if err != nil {
			log.WarningLog.Printf("failed to resolve repo config: %v", err)
		} else {
			h.store.SetHookCount(len(repoCfg.PostWorktreeCommands))
			h.hooksPane.SetCommands(repoCfg.PostWorktreeCommands)
		}
	}

	return h
}

func (m *home) Init() tea.Cmd {
	return tea.Batch(
		func() tea.Msg {
			time.Sleep(100 * time.Millisecond)
			return previewTickMsg{}
		},
		tickRefreshExternalCmd,
	)
}
