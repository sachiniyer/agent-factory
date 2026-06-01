package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// recordingAttachBackend wraps a FakeBackend and records the title of the
// instance whose Attach() is invoked, so tests can prove which instance the
// deferred attach actually connected to.
type recordingAttachBackend struct {
	*session.FakeBackend
	title string
	log   *[]string
}

func (b *recordingAttachBackend) Attach(*session.Instance) (chan struct{}, error) {
	*b.log = append(*b.log, b.title)
	ch := make(chan struct{})
	close(ch)
	return ch, nil
}

// TestHandleEnterAttachesCapturedInstanceAfterSelectionDrift is the regression
// guard for issue #716. For first-time attachers the attach is deferred until
// the attach help overlay is dismissed. The old code captured a method value
// (m.sidebar.Attach) that re-read the live selection at dismiss time, so a
// background refresh that drifted the selection onto a different instance while
// the overlay was open caused the attach to connect to the wrong instance.
//
// The fix captures the instance at Enter-press time (the synchronous moment the
// selection is provably current) and attaches to that captured instance. This
// test selects instance-a, presses Enter, drifts the selection to instance-b
// while the help overlay is open, then dismisses it and asserts the attach
// targeted instance-a.
func TestHandleEnterAttachesCapturedInstanceAfterSelectionDrift(t *testing.T) {
	var attachLog []string
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, _ string) (session.Backend, error) {
		return &recordingAttachBackend{
			FakeBackend: session.NewFakeBackend(),
			title:       opts.Title,
			log:         &attachLog,
		}, nil
	})
	defer restore()

	h := newTestHome(t)

	a, err := session.NewInstance(session.InstanceOptions{Title: "instance-a", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	a.SetStatus(session.Running)
	b, err := session.NewInstance(session.InstanceOptions{Title: "instance-b", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	b.SetStatus(session.Running)

	h.sidebar.AddInstance(a)
	h.sidebar.AddInstance(b)
	// User presses Enter on instance-a.
	h.sidebar.SetSelectedInstance(0)

	model, _ := h.handleEnter()
	h = model.(*home)
	require.Equal(t, stateHelp, h.state, "first-time attach must show the help overlay")
	require.NotNil(t, h.textOverlay, "help overlay should be installed")

	// Background refresh drifts the selection onto instance-b while the overlay
	// is open.
	h.sidebar.SetSelectedInstance(1)
	require.Same(t, b, h.sidebar.GetSelectedInstance(),
		"precondition: selection must have drifted onto instance-b")

	// Dismissing the overlay runs the deferred attach callback.
	_, _ = h.handleHelpState(tea.KeyMsg{Type: tea.KeyEnter})

	require.Equal(t, []string{"instance-a"}, attachLog,
		"attach must target the instance captured at Enter-press time, not the drifted selection")
}
