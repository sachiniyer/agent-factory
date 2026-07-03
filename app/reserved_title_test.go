package app

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
)

// TestHandleStateNewRejectsReservedRootTitle covers the TUI side of the
// #1106 name reservation: submitting the naming overlay with the reserved
// "root" title (any casing) must keep the user in the naming flow with an
// error instead of sending the create to the daemon and surfacing its
// rejection after the fact. The daemon's reserveCreate stays authoritative;
// this mirrors the #936 collision pre-check.
func TestHandleStateNewRejectsReservedRootTitle(t *testing.T) {
	for _, title := range []string{"root", "Root", "ROOT"} {
		h := &home{
			ctx:       context.Background(),
			state:     stateNew,
			appConfig: config.DefaultConfig(),
			errBox:    ui.NewErrBox(),
		}
		instance, err := session.NewInstance(session.InstanceOptions{
			Title:   title,
			Path:    t.TempDir(),
			Program: "claude",
		})
		require.NoError(t, err)
		h.namingInstance = instance

		model, _ := h.handleStateNew(tea.KeyMsg{Type: tea.KeyEnter})
		homeModel, ok := model.(*home)
		require.True(t, ok)

		assert.Equal(t, stateNew, homeModel.state,
			"title %q: submit must be rejected and the naming overlay kept open", title)
		assert.Same(t, instance, homeModel.namingInstance,
			"title %q: the naming instance must survive the rejection", title)
	}
}
