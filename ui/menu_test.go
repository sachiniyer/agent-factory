package ui

import (
	"testing"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
)

// TestMenuTerminalTabShowsBothScrollKeys verifies that when the terminal tab
// is active, the instance menu surfaces both shift+up and shift+down scroll
// shortcuts. Regression test for issue #270.
func TestMenuTerminalTabShowsBothScrollKeys(t *testing.T) {
	m := NewMenu()
	// Use a non-loading instance so addInstanceOptions renders the full menu.
	m.SetInstance(&session.Instance{Status: session.Ready})
	m.SetActiveTab(TerminalTab)

	var gotShiftUp, gotShiftDown int
	for _, k := range m.options {
		switch k {
		case keys.KeyShiftUp:
			gotShiftUp++
		case keys.KeyShiftDown:
			gotShiftDown++
		}
	}

	if gotShiftUp != 1 {
		t.Errorf("expected exactly 1 KeyShiftUp in terminal tab menu, got %d", gotShiftUp)
	}
	if gotShiftDown != 1 {
		t.Errorf("expected exactly 1 KeyShiftDown in terminal tab menu, got %d", gotShiftDown)
	}
}

// TestMenuPreviewTabShowsBothScrollKeys verifies that the preview tab also
// surfaces shift+up and shift+down — preview supports scrolling identically to
// terminal, and the help screen documents the shortcuts for both. Regression
// test for issue #467.
func TestMenuPreviewTabShowsBothScrollKeys(t *testing.T) {
	m := NewMenu()
	m.SetInstance(&session.Instance{Status: session.Ready})
	m.SetActiveTab(PreviewTab)

	var gotShiftUp, gotShiftDown int
	for _, k := range m.options {
		switch k {
		case keys.KeyShiftUp:
			gotShiftUp++
		case keys.KeyShiftDown:
			gotShiftDown++
		}
	}

	if gotShiftUp != 1 {
		t.Errorf("expected exactly 1 KeyShiftUp in preview tab menu, got %d", gotShiftUp)
	}
	if gotShiftDown != 1 {
		t.Errorf("expected exactly 1 KeyShiftDown in preview tab menu, got %d", gotShiftDown)
	}
}
