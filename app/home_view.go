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
	return m.showTransientError(err)
}

func (m *home) showTransientError(err error) tea.Cmd {
	if err == nil {
		return nil
	}
	return m.clearTransientMessageAfterDelay(m.setTransientNotice(err))
}

func (m *home) showTransientMessage(message string) tea.Cmd {
	if strings.TrimSpace(message) == "" {
		return nil
	}
	return m.clearTransientMessageAfterDelay(m.setTransientNotice(errors.New(message)))
}

func (m *home) setTransientNotice(err error) uint64 {
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

func (m *home) showErrorDetails() (tea.Model, tea.Cmd) {
	full := m.errBox.FullError()
	if full == "" {
		return m, nil
	}
	m.textOverlay = overlay.NewTextOverlay("Last error\n\n" + full)
	m.textOverlayDismissAnyKey = false
	m.textOverlayDismissPolicy = nil
	m.replayHelpDismissKey = false
	m.layoutTextOverlay()
	m.state = stateHelp
	return m, nil
}

func (m *home) confirmAction(message string, action tea.Cmd) tea.Cmd {
	m.state = stateConfirm
	m.confirmationOverlay = overlay.NewConfirmationOverlay(message)
	m.confirmationOverlay.SetWidth(50)
	m.layoutConfirmationOverlay()

	m.confirmationOverlay.OnConfirm = func() {
		m.state = stateDefault
		if action != nil {
			if msg := action(); msg != nil {
				if err, ok := msg.(error); ok {
					log.ErrorLog.Printf("confirmation action failed: %v", err)
					m.setTransientNotice(err)
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
	} else if m.state == stateSwitchProject {
		if m.projectPickerOverlay == nil {
			log.ErrorLog.Printf("project picker overlay is nil")
		}
		return overlay.PlaceOverlay(0, 0, m.projectPickerOverlay.Render(), mainView, true)
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
