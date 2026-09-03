package app

import (
	"context"
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/store"
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

// TestHandleStateNewRejectsTitleDerivingTheReservedName is the #3756 red. The
// overlay's pre-check asked IsReservedTitle — the IDENTITY question, which only
// TRIMS whitespace — while the create gate the user is about to hit asks the
// ADMISSION question, which since #3732 also refuses a title deriving the root
// agent's tmux session name. "ro ot" therefore passed the overlay and was
// refused a round trip later, with the naming flow already closed behind it:
// exactly the post-submit error the #936 pre-check exists to prevent.
func TestHandleStateNewRejectsTitleDerivingTheReservedName(t *testing.T) {
	for _, title := range []string{"ro ot", "r o o t"} {
		t.Run(title, func(t *testing.T) {
			h := &home{
				ctx:       context.Background(),
				state:     stateNew,
				appConfig: config.DefaultConfig(),
				errBox:    ui.NewErrBox(),
				// An empty projection, so the #936 collision loop just past the
				// pre-check finds nothing to refuse: the pre-check under test is
				// the only thing that can reject this title.
				store: store.NewProjection(),
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

			// The MESSAGE is the assertion, not merely the state. Keeping the
			// overlay open is something an unrelated refusal also does — a
			// missing agent binary at preflight, say — and on the unfixed code
			// that is precisely what happens, so a state-only test would pass
			// while the reserved name went unchecked.
			text, _ := homeModel.errBox.RetainedNotice()
			assert.Contains(t, text, fmt.Sprintf("%q", title),
				"the refusal must name the title the user typed")
			assert.Contains(t, text, fmt.Sprintf("%q", session.RootSessionTitle),
				"the refusal must name the reserved title it collides with: %q and %q look nothing alike on a sidebar row", title, session.RootSessionTitle)
		})
	}
}
