package app

import (
	"encoding/base64"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
)

func TestHandoffOffersAgentAccounts(t *testing.T) {
	h := newTestHome(t)
	h.store.AddInstance(handoffActionInstance(t, "worker", "claude"))
	h.sidebar.SetSelectedInstance(0)
	restore := SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{Entries: []daemon.AccountEntry{{Agent: "claude", Name: "work"}, {Agent: "codex", Name: "foreign"}, {Agent: "claude", Name: "personal"}}, Defaults: map[string]string{"claude": "personal"}}, nil
	})
	defer restore()
	_, cmd := h.handleHandoff()
	require.NotNil(t, cmd, "handoff must load the selected agent's account choices")
	h.Update(cmd())
	require.Contains(t, h.selectionOverlay.Render(), "personal")
	require.NotContains(t, h.selectionOverlay.Render(), "foreign")
	require.Contains(t, h.selectionOverlay.Render(), "project default")
}

// 80x24 before/after evidence uses the same deterministic renderer and clock
// as the design stills. Capture only inside the test container.
func TestHandoffAccountDesignScenes(t *testing.T) {
	configureDesignStillsOutput(t)
	for _, mode := range []string{"dark", "light"} {
		t.Run(mode, func(t *testing.T) {
			h, _ := newDesignDriverSceneHome(t, mode, nil)
			inst := handoffActionInstance(t, "Continue migration", "claude")
			inst.CreatedAt = designStillsNow(t)
			h.store.AddInstance(inst)
			h.sidebar.SelectInstance(inst)
			h.termWidth, h.termHeight = 80, 24
			h.relayout()
			restore := SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
				return daemon.ListAccountsResponse{Entries: []daemon.AccountEntry{{Agent: "claude", Name: "personal"}}, Defaults: map[string]string{"claude": "personal"}}, nil
			})
			defer restore()
			_, cmd := h.handleHandoff()
			require.NotNil(t, cmd)
			for _, phase := range []string{"before", "after"} {
				if phase == "after" {
					h.Update(cmd())
				}
				frame := h.View()
				svg := recoverySVG(frame, mode, 80, 24)
				name := "handoff-account-" + phase + "-" + mode + ".svg"
				if out := os.Getenv("AF_TUI_DESIGN_CAPTURE"); out != "" {
					require.NoError(t, os.MkdirAll(out, 0755))
					require.NoError(t, os.WriteFile(filepath.Join(out, name), []byte(svg), 0644))
					continue
				}
				golden, err := os.ReadFile(filepath.Join("testdata", "design", name))
				if err != nil || string(golden) != svg {
					t.Logf("CAPTURE %s %s", name, base64.StdEncoding.EncodeToString([]byte(svg)))
				}
				require.NoError(t, err)
				require.Equal(t, string(golden), svg)
			}
		})
	}
}
