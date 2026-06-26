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
	m.SetActiveTab(1)

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
	m.SetActiveTab(0)

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

// TestMenuRemoteInstanceOmitsUnsupportedTabKeys guards against regressing #988:
// remote instances block `t` (new tab) and `w` (close tab) — those handlers
// reject IsRemote() with an error — so the footer menu must only surface the
// tab keys that actually work (cycle / 1-9 jump), while local instances keep
// the full set.
func TestMenuRemoteInstanceOmitsUnsupportedTabKeys(t *testing.T) {
	remote := &session.Instance{Status: session.Ready}
	remote.SetBackend(&session.HookBackend{})
	if !remote.IsRemote() {
		t.Fatal("sanity: instance should report as remote")
	}

	m := NewMenu()
	m.SetInstance(remote)

	var gotNewTab, gotCloseTab, gotTab, gotJump int
	for _, k := range m.options {
		switch k {
		case keys.KeyNewTab:
			gotNewTab++
		case keys.KeyCloseTab:
			gotCloseTab++
		case keys.KeyTab:
			gotTab++
		case keys.KeyJumpTab:
			gotJump++
		}
	}
	if gotNewTab != 0 || gotCloseTab != 0 {
		t.Errorf("remote menu must not surface t/w tab keys; got newTab=%d closeTab=%d", gotNewTab, gotCloseTab)
	}
	if gotTab != 1 || gotJump != 1 {
		t.Errorf("remote menu should still surface tab cycle and 1-9 jump; got tab=%d jump=%d", gotTab, gotJump)
	}

	local := &session.Instance{Status: session.Ready}
	if local.IsRemote() {
		t.Fatal("sanity: instance should report as local")
	}
	m.SetInstance(local)
	var localNewTab, localCloseTab int
	for _, k := range m.options {
		switch k {
		case keys.KeyNewTab:
			localNewTab++
		case keys.KeyCloseTab:
			localCloseTab++
		}
	}
	if localNewTab != 1 || localCloseTab != 1 {
		t.Errorf("local menu should surface t/w tab keys; got newTab=%d closeTab=%d", localNewTab, localCloseTab)
	}
}
