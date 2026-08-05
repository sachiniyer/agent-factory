package app

import (
	"errors"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

func (m *home) handleError(err error) tea.Cmd {
	log.ErrorLog.Printf("%v", err)
	if err == nil {
		return nil
	}
	return m.clearTransientMessageAfterDelay(m.setTransientFailure(err))
}

// handleNotice reports why af deliberately declined or redirected a user
// action. The same transient UI surface carries both notices and failures, but
// their logs must not — ERROR is reserved for an operation that actually failed
// — and neither must the details overlay's title (#2618).
func (m *home) handleNotice(notice error) tea.Cmd {
	log.InfoLog.Printf("%v", notice)
	if notice == nil {
		return nil
	}
	return m.clearTransientMessageAfterDelay(m.setTransientNotice(notice))
}

func (m *home) showTransientMessage(message string) tea.Cmd {
	if strings.TrimSpace(message) == "" {
		return nil
	}
	return m.clearTransientMessageAfterDelay(m.setTransientNotice(errors.New(message)))
}

// setTransientNotice raises informational guidance: an action af declined, work
// it started, a layout decision it made on the user's behalf.
func (m *home) setTransientNotice(err error) uint64 {
	m.transientNoticeID++
	m.errBox.SetNotice(err)
	return m.transientNoticeID
}

// setTransientFailure raises a real operation failure. Same bar, same timer —
// only the category differs, which is what the details overlay titles itself
// from.
func (m *home) setTransientFailure(err error) uint64 {
	m.transientNoticeID++
	m.errBox.SetError(err)
	return m.transientNoticeID
}

func (m *home) clearTransientMessageAfterDelay(noticeID uint64) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-m.ctx.Done():
		case <-time.After(3 * time.Second):
		}
		return hideErrMsg{noticeID: noticeID}
	}
}

// showErrorDetails opens the last notice in full. It reads the RETAINED notice,
// not the one currently painted: the bar drops a notice after 3 seconds, and
// binding `E` to what is still on screen made the key silently dead in exactly
// the case it exists for — a message the bar had to clip (#2618).
func (m *home) showErrorDetails() (tea.Model, tea.Cmd) {
	full, failure := m.errBox.RetainedNotice()
	if full == "" {
		return m, nil
	}
	// af hiding a pane and saying how to get it back is designed behavior. Filing
	// it under "Last error" would report af working as intended as a fault, which
	// is the same miscategorization #2575 found in the logs.
	title := "Last notice"
	if failure {
		title = "Last error"
	}
	m.textOverlay = overlay.NewTextOverlay(title + "\n\n" + full)
	m.textOverlayDismissAnyKey = false
	m.textOverlayScrollable = true
	m.textOverlayDismissPolicy = nil
	m.replayHelpDismissKey = false
	m.layoutTextOverlay()
	m.state = stateHelp
	return m, nil
}

func (m *home) confirmAction(message string, action tea.Cmd) tea.Cmd {
	return m.confirmActionWithDetail(message, "", action)
}

// confirmActionWithDetail is confirmAction for a dialog whose copy splits into
// consequences (message) and elaboration (detail). The overlay then guarantees
// the consequences render or refuses the confirm outright, rather than clipping
// the tail and collecting a 'y' for something it never showed (#1973).
func (m *home) confirmActionWithDetail(message, detail string, action tea.Cmd) tea.Cmd {
	m.state = stateConfirm
	m.confirmationOverlay = overlay.NewConfirmationOverlay(message)
	if detail != "" {
		m.confirmationOverlay.SetDetail(detail)
	}
	m.confirmationOverlay.SetWidth(50)
	m.layoutConfirmationOverlay()

	m.confirmationOverlay.OnConfirm = func() {
		m.state = stateDefault
		if action != nil {
			if msg := action(); msg != nil {
				if err, ok := msg.(error); ok {
					log.ErrorLog.Printf("confirmation action failed: %v", err)
					m.setTransientFailure(err)
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
	if m.attachTransitioning {
		return blankFrame(m.termWidth, m.termHeight)
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
	if m.lastLayout.ProjectsVisible {
		railParts = append(railParts, m.renderProjectsRule(), m.projects.View())
	}
	rail := lipgloss.JoinVertical(lipgloss.Left, railParts...)
	cols := []string{rail}
	if len(m.visiblePanes) == 0 {
		switch {
		// Session count FIRST, and the order is load-bearing. "no panes open" and
		// "no project selected" answer different questions — why is this area
		// blank, versus what af is scoped to — and they are not alternatives: a
		// user who closed their panes has panes to reopen whatever the project
		// state is, so sending them off to pick a project would be a worse answer
		// than the one #2830 set out to fix.
		//
		// Registry mode CAN hold sessions it cannot act on: the refresh tick
		// fetches with an empty repoID, which the daemon answers with the
		// cross-repo snapshot, contradicting the launch path that skips exactly
		// that. Advertising `s` for a row that would no-op is a real problem, but
		// a PROJECTION one — those rows should not be in the store — and
		// re-prioritizing this message would paper over it in the wrong layer.
		case m.store.NumInstances() > 0:
			cols = append(cols, ui.EmptyWorkspace(m.lastLayout.Workspace))
		case m.repoRoot == "":
			// The empty rail, which is all #2830 is about: no sessions, and no
			// active project to create one in, so `n` cannot work from any focused
			// region here (#2477/#2764).
			cols = append(cols, ui.NoActiveProjectWorkspace(m.lastLayout.Workspace, switchProjectPickHint(m.enterPicksAProject(), m.projectsFocused())))
		default:
			cols = append(cols, ui.FirstRunWorkspace(m.lastLayout.Workspace))
		}
	}
	for i, p := range m.visiblePanes {
		if i > 0 {
			cols = append(cols, m.renderDivider(i-1))
		}
		if w := m.paneWindows[p.ID()]; w != nil {
			w.SetSidebarSelected(m.paneMatchesSelection(p))
			w.SetSelectionHint(m.paneSelectionHint(p))
			w.SetDropTarget(m.tabDragDropTargetRegion() == layout.PaneRegion(p.ID()))
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
	m.menu.SetStatusText(m.dragStatusText())
	viewParts = append(viewParts, top, m.statusBar.View())
	mainView := lipgloss.JoinVertical(lipgloss.Left, viewParts...)

	if m.state == stateHelp {
		if m.textOverlay == nil {
			log.ErrorLog.Printf("text overlay is nil")
		}
		return placeOverlay(m.textOverlay.Render(), mainView)
	} else if m.state == stateConfirm {
		if m.confirmationOverlay == nil {
			log.ErrorLog.Printf("confirmation overlay is nil")
		}
		fg := m.confirmationOverlay.Render()
		m.confirmationOverlay.RegisterZones(m.zones, overlayOrigin(fg, mainView))
		return placeOverlay(fg, mainView)
	} else if m.state == stateSearch {
		if m.searchOverlay == nil {
			log.ErrorLog.Printf("search overlay is nil")
		}
		fg := m.searchOverlay.Render()
		m.searchOverlay.RegisterZones(m.zones, overlayOrigin(fg, mainView))
		return placeOverlay(fg, mainView)
	} else if m.state == stateSwitchProject {
		if m.projectPickerOverlay == nil {
			log.ErrorLog.Printf("project picker overlay is nil")
		}
		return placeOverlay(m.projectPickerOverlay.Render(), mainView)
	} else if m.state == stateSelectProgram || m.state == stateSelectHandoffAgent || m.state == stateSelectBackend {
		if m.selectionOverlay == nil {
			log.ErrorLog.Printf("selection overlay is nil")
		}
		fg := m.selectionOverlay.Render()
		m.selectionOverlay.RegisterZones(m.zones, overlayOrigin(fg, mainView))
		return placeOverlay(fg, mainView)
	} else if m.state == stateSelectTabKind {
		if m.selectionOverlay == nil {
			log.ErrorLog.Printf("tab-kind selection overlay is nil")
			return mainView
		}
		fg := m.selectionOverlay.Render()
		m.selectionOverlay.RegisterZones(m.zones, overlayOrigin(fg, mainView))
		return placeOverlay(fg, mainView)
	} else if m.state == statePromptInput {
		if m.promptOverlay == nil {
			log.ErrorLog.Printf("prompt overlay is nil")
			return mainView
		}
		return placeOverlay(m.promptOverlay.Render(), mainView)
	} else if m.state == stateHooks {
		return placeOverlay(m.renderHooksOverlay(), mainView)
	} else if m.state == stateTasks {
		return placeOverlay(m.renderTasksOverlay(), mainView)
	} else if m.state == stateConfigEditor {
		return placeOverlay(m.renderConfigOverlay(), mainView)
	}

	return mainView
}

func blankFrame(width, height int) string {
	if width < 1 || height < 1 {
		return ""
	}
	line := strings.Repeat(" ", width)
	lines := make([]string, height)
	for i := range lines {
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}

// The View render helpers (overlay framing + rail/divider rules) live in
// render.go, extracted to keep app.go under its file-length ceiling (#1145).
