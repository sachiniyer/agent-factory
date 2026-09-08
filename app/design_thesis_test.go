package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The thesis is reviewed at the minimum everyday terminal size, alongside the
// wider component matrix. These are real Update/View fixtures, not a live agent.
func TestDesignThesisScenes(t *testing.T) {
	configureDesignStillsOutput(t)
	for _, mode := range []string{"light", "dark"} {
		for _, scene := range []string{"sessions", "preview"} {
			t.Run(scene+"-"+mode, func(t *testing.T) {
				h, inst := newDesignDriverSceneHome(t, mode, nil)
				h.termWidth, h.termHeight = 80, 24
				h.relayout()
				setPreviewText(inst, "Design roles are applied.\nAgent-owned output stays intact.")
				pane := openTestPane(t, h, inst, 0)
				window := h.paneWindows[pane.ID()]
				if scene == "preview" {
					window.SetPreview(inst, 0, "Origin session")
				}
				require.IsType(t, panesRefreshedMsg{}, refreshPaneBindingCmd(window, inst, 0, window.ContentSeq())())
				frame := h.View()
				svg := recoverySVG(frame, mode, 80, 24)
				name := "thesis-" + scene + "-" + mode + ".svg"
				if out := os.Getenv("AF_TUI_DESIGN_CAPTURE"); out != "" {
					require.NoError(t, os.MkdirAll(out, 0755))
					require.NoError(t, os.WriteFile(filepath.Join(out, name), []byte(svg), 0644))
					return
				}
				golden, err := os.ReadFile(filepath.Join("testdata", "design", name))
				require.NoError(t, err)
				require.Equal(t, string(golden), svg, "recapture the 80×24 thesis scenes in the container")
			})
		}
	}
}
