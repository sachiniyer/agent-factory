package app

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/stretchr/testify/require"
)

func TestDeleteTabFailureSchedulesNoticeExpiry(t *testing.T) {
	for _, failure := range []string{"rpc", "local-drop"} {
		t.Run(failure, func(t *testing.T) {
			h, inst := multiTabHome(t)
			h.store.SetActiveTab(2)
			t.Cleanup(SetTabCloserForTest(func(daemon.CloseTabRequest) error {
				if failure == "rpc" {
					return errors.New("tab deletion refused")
				}
				// Simulate a roster change between the daemon response and local drop.
				require.NoError(t, inst.DropClosedTab(2))
				require.NoError(t, inst.DropClosedTab(2))
				return nil
			}))
			_, _ = h.handleCloseTab()
			start := time.Now()
			_, cmd := h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
			require.Less(t, time.Since(start), 100*time.Millisecond, "confirmation must not run the error timer inline")
			require.Equal(t, stateDefault, h.state)
			require.Nil(t, h.confirmationOverlay)
			require.NotNil(t, cmd, "the event loop must receive the delayed hide command")
			notice := h.errBox.String()
			require.NotEmpty(t, strings.TrimSpace(notice), "failure must be visible immediately after confirmation")
			arrived := make(chan tea.Msg, 1)
			go func() { arrived <- cmd() }()
			select {
			case <-arrived:
				t.Fatal("notice expired immediately instead of waiting off the event loop")
			case <-time.After(100 * time.Millisecond):
			}
			require.Equal(t, notice, h.errBox.String())
			select {
			case msg := <-arrived:
				require.IsType(t, hideErrMsg{}, msg)
				require.Equal(t, notice, h.errBox.String(), "only Update may expire the notice")
				_, _ = h.Update(msg)
				require.Empty(t, strings.TrimSpace(h.errBox.String()))
			case <-time.After(5 * time.Second):
				t.Fatal("scheduled hide message never arrived")
			}
		})
	}
}
