package app

import (
	"context"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
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

// TestHandleStateNewRejectsRemoteSlugCollision regression-tests issue #312:
// when a user names a new remote session whose Title reduces to the same
// hook-script slug as an existing remote session, Enter must refuse the
// name rather than silently producing a colliding slug. Local sessions
// are unaffected because slugify only drives the remote hook protocol.
func TestHandleStateNewRejectsRemoteSlugCollision(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	errBox := ui.NewErrBox()
	// ErrBox only renders text when it has non-zero size; set something
	// reasonable so String() returns the error for the assertion below.
	errBox.SetSize(120, 1)
	h := &home{
		ctx:       context.Background(),
		state:     stateNew,
		appConfig: config.DefaultConfig(),
		errBox:    errBox,
		sidebar:   ui.NewSidebar(&spin, false),
		menu:      ui.NewMenu(),
	}

	// Stub the backend factory so NewInstance can mint remote instances
	// without a real repo / remote_hooks config on disk.
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, _ string) (session.Backend, error) {
		if opts.ForceRemote {
			return &session.HookBackend{Hooks: config.RemoteHooks{}}, nil
		}
		return &session.LocalBackend{}, nil
	})
	defer restore()

	// Pre-existing remote session whose Title slugifies to "myapp".
	existing, err := session.NewInstance(session.InstanceOptions{
		Title:       "myapp",
		Path:        t.TempDir(),
		Program:     "claude",
		ForceRemote: true,
	})
	require.NoError(t, err)
	h.sidebar.AddInstance(existing)()

	// The instance currently being named: underscore is stripped, so this
	// also slugifies to "myapp" and must be rejected.
	naming, err := session.NewInstance(session.InstanceOptions{
		Title:       "my_app",
		Path:        t.TempDir(),
		Program:     "claude",
		ForceRemote: true,
	})
	require.NoError(t, err)
	h.namingInstance = naming
	h.newInstanceFinalizer = func() {}

	_, _ = h.handleStateNew(tea.KeyMsg{Type: tea.KeyEnter})

	// The naming flow must stay in stateNew with the title preserved, and
	// the errBox should carry a collision message naming the existing title.
	assert.Equal(t, stateNew, h.state, "collision should not advance past stateNew")
	require.NotNil(t, h.namingInstance)
	assert.Equal(t, "my_app", h.namingInstance.Title)
	assert.Contains(t, h.errBox.String(), "myapp",
		"error message should name the colliding existing slug")
}

// TestHandleStateNewRejectsDuplicateLocalTitle covers the duplicate-Title
// path for local sessions: tmux would later reject the Start with "tmux
// session already exists", but the naming flow catches it earlier with a
// clean error. Same path applies to remote exact-duplicate titles.
func TestHandleStateNewRejectsDuplicateLocalTitle(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	errBox := ui.NewErrBox()
	errBox.SetSize(120, 1)
	h := &home{
		ctx:       context.Background(),
		state:     stateNew,
		appConfig: config.DefaultConfig(),
		errBox:    errBox,
		sidebar:   ui.NewSidebar(&spin, false),
		menu:      ui.NewMenu(),
	}

	existing, err := session.NewInstance(session.InstanceOptions{
		Title:   "fix-bug",
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	h.sidebar.AddInstance(existing)()

	naming, err := session.NewInstance(session.InstanceOptions{
		Title:   "fix-bug",
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	h.namingInstance = naming
	h.newInstanceFinalizer = func() {}

	_, _ = h.handleStateNew(tea.KeyMsg{Type: tea.KeyEnter})

	assert.Equal(t, stateNew, h.state, "duplicate Title should not advance past stateNew")
	require.NotNil(t, h.namingInstance)
	assert.Equal(t, "fix-bug", h.namingInstance.Title)
	assert.Contains(t, h.errBox.String(), "fix-bug",
		"error message should name the duplicate Title")
}
