package app

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

func (m *home) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	defer m.persistTUIViewStateAfter(msg)
	// The double-click tracker is only valid within an unbroken run of
	// stateDefault: any modal/overlay excursion between two clicks must
	// invalidate a pre-modal press so it can't pair with a post-modal press
	// into a false double click (#1731). Update is the single chokepoint every
	// message (keyboard-driven modal open/close included) flows through.
	stateBefore := m.state
	defer m.clearStaleClickTrackerAfter(stateBefore)
	switch msg := msg.(type) {
	case hideErrMsg:
		if msg.noticeID == m.transientNoticeID {
			m.errBox.Clear()
		}
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
		// Hold/renew the daemon delivery-defer lease for the session the user is
		// typing into via the focused embedded interactive pane (#1586). Runs
		// after syncLiveTermPane has settled interactive mode; the RPC is
		// dispatched off the event loop.
		pausePollCmd := m.interactivePollPauseCmd()
		var cmd tea.Cmd
		if !m.attached.Load() {
			// Mark this selectionChanged as the idle refresh tick so the preview
			// path does not steal focus onto the selected instance's open pane
			// while the user drives the focus ring (#1558).
			m.inPreviewTick = true
			cmd = m.selectionChanged()
			m.inPreviewTick = false
		}
		return m, tea.Batch(
			cmd,
			pausePollCmd,
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
		return m, tea.Batch(tickUpdatePRInfoCmd, fetchPRInfoCmd(selected, m.repoID, true))
	case prInfoUpdatedMsg:
		detachTraceMark("prInfoUpdatedMsg-handler-entry")
		// Drop PR info fetched for a repo we have since switched away from
		// (#1780). An in-place project switch (#1461) resets the store and swaps
		// m.repoID, so the title-only re-resolution below can land the previous
		// project's result on a same-title session in the new project — and the
		// branch guard misses it when both sessions share a branch name. Gate on
		// the captured repo first, mirroring snapshotFetchedMsg. No tick re-arm
		// here: the PR-info tick re-arms itself in tickUpdatePRInfoMessage.
		if msg.repoID != m.repoID {
			return m, nil
		}
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
		// Drop a snapshot fetched for a repo we have since switched away from
		// (#1461): applying its old-repo sessions/tasks would bleed the previous
		// project into the new view. Still re-arm the pacing tick so polling of
		// the now-active repo continues.
		if msg.repoID != m.repoID {
			return m, tickRefreshExternalCmd
		}
		tickStart := time.Now()
		changed := m.handleSnapshot(msg)
		// Live-project out-of-band task changes on the same poll (#1168),
		// independent of the session snapshot's own error path.
		if m.refreshTasks(msg.tasks, msg.tasksErr) {
			changed = true
		}
		// Keep the always-visible Projects section's per-repo counts live from the
		// cross-repo snapshot fetched on the same poll (#1590), so a session
		// created/removed in another repo updates the rows without a project
		// switch. Its own error path leaves the last-known rows intact.
		if m.refreshSidebarProjectsFromSnapshot(msg.allRepos, msg.allReposErr) {
			changed = true
		}
		detachTrace(tickStart, "snapshotFetchedMsg-reconcile-returned")
		cmds := []tea.Cmd{tickRefreshExternalCmd}
		if changed {
			// A snapshot poll is a background refresh, not a user action, so its
			// selectionChanged must NOT steal focus onto the selected instance's
			// already-open pane — the same guard previewTickMsg uses (#1558). The
			// pane close/rebind for out-of-band tab/session removals already ran in
			// handleSnapshot above; this selectionChanged only re-derives the
			// preview, which is exactly the focus-steal we gate here (#1603).
			m.inPreviewTick = true
			cmds = append(cmds, m.selectionChanged())
			m.inPreviewTick = false
		}
		return m, tea.Batch(cmds...)
	case taskTriggeredMsg:
		// The daemon-side run (#1169) failed — surface it. On success, give a
		// short positive acknowledgement while the resulting session and updated
		// task status live-project in from the daemon snapshot.
		if msg.err != nil {
			return m, m.handleError(fmt.Errorf("failed to trigger task %q: %w", msg.title, msg.err))
		}
		return m, m.showTransientMessage(fmt.Sprintf("triggered %s", msg.title))
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
		return m, m.updateHandleWindowSizeEvent(msg)
	case error:
		return m, m.handleError(msg)
	case runOnEventLoopMsg:
		// Invoked only by test harnesses — lets them read model state from
		// the tea goroutine so it doesn't race with Update mutations.
		msg.fn(m)
		close(msg.done)
		return m, nil
	case enterInteractiveMsg:
		// Deferred by the first-time interactive help screen's dismiss cmd
		// (#1089 PR 2); the pane pointer is re-validated inside.
		cmd := m.activateInteractive(msg.pane)
		if msg.replay && m.interactive {
			_, replayCmd := m.handleInteractiveKey(msg.replayKey)
			cmd = tea.Batch(cmd, replayCmd)
		}
		return m, cmd
	case beginAttachMsg:
		if msg.run == nil {
			m.attachTransitioning = false
			return m, nil
		}
		return m, msg.run()
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
	case startDeleteProjectMsg:
		// Delete-project confirmed; run the daemon archive-then-remove off the
		// event loop (#1735), mirroring the archive dispatch.
		return m, m.deleteProjectCmd(msg)
	case projectDeletedMsg:
		return m.handleProjectDeleted(msg)
	case limitRetriedMsg:
		return m.handleLimitRetried(msg)
	case configAgentSpawnedMsg:
		return m.handleConfigAgentSpawned(msg)
	case configAgentDoneMsg:
		return m.handleConfigAgentDone(msg)
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

		_ = started.Transition(session.ConfirmLive())
		if !swapped && started.Capabilities().Workspace == session.WorkspaceLocalWorktree {
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
	}
	return m, nil
}

func (m *home) handleMenuHighlighting(msg tea.KeyMsg) (cmd tea.Cmd, returnEarly bool) {
	if m.keySent {
		m.keySent = false
		return nil, false
	}
	// While naming a new instance the menu only shows the submit-name (enter),
	// change-program (tab), and cancel (esc) options, so those keys are the only ones
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
		case "esc":
			name = keys.KeyCancelName
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
	// Don't highlight when a focused bottom rail section (automations or
	// projects) has the keyboard — its own cursor keys own the input.
	if active := m.ring.Active(); active == layout.RegionAutomations || active == layout.RegionProjects {
		return nil, false
	}
	name, ok := keys.GlobalKeyStringsMap[msg.String()]
	if !ok {
		return nil, false
	}

	if name == keys.KeyShiftDown || name == keys.KeyShiftUp {
		return nil, false
	}
	if name == keys.KeyErrorDetails {
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
	case stateSwitchProject:
		return m.handleStateSwitchProject(msg)
	case stateSelectProgram:
		return m.handleStateSelectProgram(msg)
	case stateHooks:
		return m.handleStateHooks(msg)
	case stateTasks:
		return m.handleStateTasks(msg)
	case stateConfigEditor:
		return m.handleStateConfigEditor(msg)
	}

	// The focused in-rail automations section owns its cursor keys; Enter/Esc
	// route here too (open the manager overlay / return to the tree), while
	// Tab/Shift-Tab, quit, and the global overlay keys (S/H/?) fall through.
	if mod, cmd, consumed := m.handleAutomationsFocus(msg); consumed {
		return mod, cmd
	}

	// The focused bottom Projects section owns its cursor keys; Enter switches
	// the rail to the cursor's project and Esc returns to the tree, while
	// Tab/Shift-Tab, quit, and the global overlay keys fall through (#1588
	// follow-up).
	if mod, cmd, consumed := m.handleProjectsFocus(msg); consumed {
		return mod, cmd
	}

	// Ctrl+C is an always-on hard exit — never rebindable, so it stays a
	// hardcoded check ahead of contextual handlers and the keymap. The quit
	// VERB (default q, or whatever [keys].quit rebinds it to) dispatches
	// through the generated table like every other rebindable action, via
	// keys.KeyQuit in handleDefaultKeyPress (#1026).
	if msg.String() == "ctrl+c" {
		return m.handleQuit()
	}

	// Pane-local nav shortcuts are contextual: LEFT/RIGHT switch visible
	// workspace panes only while a pane owns focus. With tree focus, those
	// same physical arrows continue through the global map as collapse/expand.
	if mod, cmd, consumed := m.handlePaneFocusKey(msg); consumed {
		return mod, cmd
	}

	// Exit scrolling mode when ESC is pressed (each pane keeps its own
	// scroll state, #1088)
	if msg.Type == tea.KeyEsc {
		if pane, bound := m.focusedContentPane(); pane != nil && pane.IsInScrollMode() {
			if err := pane.ResetToNormalMode(bound); err != nil {
				return m, m.handleError(err)
			}
			if m.panePreviewTxn != nil {
				m.suppressActivePanePreview()
				m.cancelPanePreview(true)
				return m, m.panesRefresh(m.attached.Load())
			}
			return m, m.selectionChanged()
		}
		if m.panePreviewTxn != nil {
			m.suppressActivePanePreview()
			m.cancelPanePreview(true)
			return m, m.panesRefresh(m.attached.Load())
		}
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
