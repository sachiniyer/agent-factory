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

// TestHandleStateNewKeySpaceWidthLimit ensures that pressing the spacebar
// while naming a new instance respects the same 32-cell width cap that
// regular character input enforces.
func TestHandleStateNewKeySpaceWidthLimit(t *testing.T) {
	h := &home{
		ctx:       context.Background(),
		state:     stateNew,
		appConfig: config.DefaultConfig(),
		errBox:    ui.NewErrBox(),
	}

	// Build an instance whose title is exactly at the 32-cell limit.
	instance, err := session.NewInstance(session.InstanceOptions{
		Title:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", // 32 characters
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)

	h.namingInstance = instance

	keyMsg := tea.KeyMsg{Type: tea.KeySpace}
	model, _ := h.handleStateNew(keyMsg)
	homeModel, ok := model.(*home)
	require.True(t, ok)

	assert.Equal(t, 32, len(homeModel.namingInstance.Title),
		"Title should remain at 32 characters, but got %d: %q",
		len(homeModel.namingInstance.Title), homeModel.namingInstance.Title)
}

// TestHandleStateNewKeySpaceUnderLimit ensures that a spacebar press is
// still accepted when the resulting title stays within the 32-cell cap.
func TestHandleStateNewKeySpaceUnderLimit(t *testing.T) {
	h := &home{
		ctx:       context.Background(),
		state:     stateNew,
		appConfig: config.DefaultConfig(),
		errBox:    ui.NewErrBox(),
	}

	instance, err := session.NewInstance(session.InstanceOptions{
		Title:   "hello",
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)

	h.namingInstance = instance

	keyMsg := tea.KeyMsg{Type: tea.KeySpace}
	model, _ := h.handleStateNew(keyMsg)
	homeModel, ok := model.(*home)
	require.True(t, ok)

	assert.Equal(t, "hello ", homeModel.namingInstance.Title)
}

func TestHandleMenuHighlightingDoesNotInterceptNamingText(t *testing.T) {
	h := newTestHome(t)
	h.state = stateNew

	cmd, returnEarly := h.handleMenuHighlighting(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})

	assert.False(t, returnEarly)
	assert.Nil(t, cmd)
}

func TestHandleStateNewRejectsDuplicateTitle(t *testing.T) {
	h := newTestHome(t)
	h.state = stateNew
	h.errBox.SetSize(120, 1)

	existing, err := session.NewInstance(session.InstanceOptions{
		Title:   "fix-bug",
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	h.sidebar.AddInstance(existing)

	naming, err := session.NewInstance(session.InstanceOptions{
		Title:   "fix-bug",
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	h.namingInstance = naming

	_, _ = h.handleStateNew(tea.KeyMsg{Type: tea.KeyEnter})

	assert.Equal(t, stateNew, h.state)
	require.NotNil(t, h.namingInstance)
	assert.Contains(t, h.errBox.String(), "fix-bug")
}

func TestHandleStateNewRejectsRemoteSlugCollision(t *testing.T) {
	h := newTestHome(t)
	h.state = stateNew
	h.errBox.SetSize(120, 1)

	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, _ string) (session.Backend, error) {
		if opts.ForceRemote {
			return &session.HookBackend{Hooks: config.RemoteHooks{}}, nil
		}
		return &session.LocalBackend{}, nil
	})
	defer restore()

	existing, err := session.NewInstance(session.InstanceOptions{
		Title:       "myapp",
		Path:        t.TempDir(),
		Program:     "claude",
		ForceRemote: true,
	})
	require.NoError(t, err)
	h.sidebar.AddInstance(existing)

	naming, err := session.NewInstance(session.InstanceOptions{
		Title:       "my_app",
		Path:        t.TempDir(),
		Program:     "claude",
		ForceRemote: true,
	})
	require.NoError(t, err)
	h.namingInstance = naming

	_, _ = h.handleStateNew(tea.KeyMsg{Type: tea.KeyEnter})

	assert.Equal(t, stateNew, h.state)
	require.NotNil(t, h.namingInstance)
	assert.Equal(t, "my_app", h.namingInstance.Title)
	assert.Contains(t, h.errBox.String(), "myapp")
}
