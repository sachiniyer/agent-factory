package app

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/keys"
)

func TestPaneHeaderStateCopyUsesSentenceCaseAtCompactSizes(t *testing.T) {
	for _, size := range []struct {
		width  int
		height int
	}{
		{width: 80, height: 24},
		{width: 72, height: 20},
	} {
		t.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(t *testing.T) {
			h := paneTestHome(t)
			beta := h.store.GetInstanceByTitle("beta")
			setPreviewText(beta, "BETA_PREVIEW_HISTORY")

			pressKey(t, h, "s")
			paneA := h.store.OpenPanes()[0]
			w := h.paneWindows[paneA.ID()]
			require.NotNil(t, w)

			h.sidebar.SetSelectedInstance(1)
			_ = h.selectionChanged()
			require.NotNil(t, h.panePreviewTxn)
			require.IsType(t, panesRefreshedMsg{},
				refreshPaneBindingCmd(w, beta, 0, h.panePreviewTxn.seq)())
			resizeHome(h, size.width, size.height)

			previewView := h.View()
			requireViewSized(t, previewView, size.width, size.height)
			assert.Contains(t, previewView, "Preview beta",
				"%dx%d: preview state follows the TUI sentence-case convention", size.width, size.height)
			assert.NotContains(t, previewView, "PREVIEW beta",
				"%dx%d: preview state must not caps-shout", size.width, size.height)

			scrollHome := paneTestHome(t)
			scrollAlpha := scrollHome.store.GetInstanceByTitle("alpha")
			pressKey(t, scrollHome, "s")
			scrollPane := scrollHome.store.OpenPanes()[0]
			scrollWindow := scrollHome.paneWindows[scrollPane.ID()]
			require.NotNil(t, scrollWindow)
			require.IsType(t, panesRefreshedMsg{},
				refreshPaneBindingCmd(scrollWindow, scrollAlpha, 0, scrollWindow.ContentSeq())())
			resizeHome(scrollHome, size.width, size.height)
			_, _ = scrollHome.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyCtrlU}, keys.KeyShiftUp)
			require.True(t, scrollWindow.IsInScrollMode(), "precondition: Ctrl-U enters pane scroll mode")

			scrollView := scrollHome.View()
			requireViewSized(t, scrollView, size.width, size.height)
			assert.Contains(t, scrollView, "Scroll",
				"%dx%d: scroll state follows the TUI sentence-case convention", size.width, size.height)
			assert.NotContains(t, scrollView, "SCROLL",
				"%dx%d: scroll state must not caps-shout", size.width, size.height)
		})
	}
}
