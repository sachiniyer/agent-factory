package session

import (
	"errors"
	"testing"
)

// resolverRoster is a three-tab session whose selectors deliberately disagree,
// so a resolution that silently used the wrong one is visible rather than
// coincidentally right: the tab named "shell" is NOT at the ordinal a caller
// would guess, and its label ("Terminal") is not its name.
func resolverRoster() []*Tab {
	return []*Tab{
		{ID: "id-agent", Name: "agent", Kind: TabKindAgent},
		{ID: "id-btop", Name: "btop", Kind: TabKindProcess},
		{ID: "id-shell", Name: "shell", Kind: TabKindShell},
	}
}

// TestResolveTabIndex_Precedence pins the order every tab verb shares: id, then
// name, then ordinal. Each case supplies selectors that point at DIFFERENT tabs,
// so the winner identifies which rule actually ran.
func TestResolveTabIndex_Precedence(t *testing.T) {
	tabs := resolverRoster()

	for _, c := range []struct {
		name    string
		tabID   string
		tabName string
		ordinal int
		want    int
		why     string
	}{
		{"id beats name and ordinal", "id-shell", "btop", 0, 2,
			"the stable id is the only non-reusable handle, so it wins outright"},
		{"name beats ordinal", "", "btop", 0, 1,
			"a name outranks a slot: the slot shifts on every close and reorder"},
		{"ordinal only when nothing else is given", "", "", 1, 1,
			"positional addressing is the legacy fallback, not a co-equal selector"},
		{"ordinal zero is a real answer", "", "", 0, 0,
			"slot 0 is the agent tab, not an 'unset' sentinel"},
	} {
		got, err := ResolveTabIndex(tabs, c.tabID, c.tabName, c.ordinal)
		if err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: resolved slot %d, want %d — %s", c.name, got, c.want, c.why)
		}
	}
}

// TestResolveTabIndex_RefusesRatherThanFallsBack is the #1779/#1929 rule and the
// reason this function exists as one definition rather than one per caller.
//
// A supplied id or name that does not resolve must be an ERROR. Falling back to
// the ordinal would address whatever tab has since shifted into that slot — the
// exact misroute the stable id exists to prevent, wearing a backward-compatible
// face. It is the quiet kind of wrong: the caller asked for a specific tab, got a
// different one, and nothing failed.
func TestResolveTabIndex_RefusesRatherThanFallsBack(t *testing.T) {
	tabs := resolverRoster()

	// An id that no longer resolves, with a perfectly valid ordinal alongside it.
	if _, err := ResolveTabIndex(tabs, "id-closed", "", 1); !errors.Is(err, ErrTabIDNotFound) {
		t.Errorf("unresolvable id returned %v, want ErrTabIDNotFound — it must never fall back "+
			"to ordinal 1, which is a different, still-live tab", err)
	}
	// Same for a name, and note the ordinal here is valid too.
	if _, err := ResolveTabIndex(tabs, "", "gone", 1); !errors.Is(err, ErrTabNameNotFound) {
		t.Errorf("unresolvable name returned %v, want ErrTabNameNotFound — no silent fall back "+
			"to the ordinal", err)
	}
	// A bad ordinal, when it IS the selector, is its own miss.
	for _, bad := range []int{-1, 3, 99} {
		if _, err := ResolveTabIndex(tabs, "", "", bad); !errors.Is(err, ErrTabIndexOutOfRange) {
			t.Errorf("ordinal %d returned %v, want ErrTabIndexOutOfRange", bad, err)
		}
	}
	// An empty roster resolves nothing, rather than panicking on tabs[0].
	if _, err := ResolveTabIndex(nil, "", "", 0); !errors.Is(err, ErrTabIndexOutOfRange) {
		t.Errorf("empty roster returned %v, want ErrTabIndexOutOfRange", err)
	}
}

// TestResolveTabIndex_DoesNotAcceptTheDisplayLabel ties the shared resolver to
// the #1986 split: the label is presentation-only and must never address a tab,
// on THIS path as much as on the daemon's. A resolver that accepted it would
// reintroduce two strings for one tab through the back door — and this is the
// path `af sessions preview --tab-name` now runs on, which did not exist when
// #1986 landed.
func TestResolveTabIndex_DoesNotAcceptTheDisplayLabel(t *testing.T) {
	tabs := resolverRoster()
	shell := tabs[2]

	label := TabLabel(shell) // "Terminal"
	if label == shell.Name {
		t.Skip("TabLabel now equals Name; there is no second string to reject")
	}
	if _, err := ResolveTabIndex(tabs, "", label, 0); !errors.Is(err, ErrTabNameNotFound) {
		t.Errorf("the display label %q resolved a tab; it is presentation-only and must never be "+
			"an identifier (#1986). Only the name %q addresses it.", label, shell.Name)
	}
	// …and the real name still works, so the rejection above is not blanket failure.
	if got, err := ResolveTabIndex(tabs, "", shell.Name, 0); err != nil || got != 2 {
		t.Errorf("ResolveTabIndex by name %q = (%d, %v), want (2, nil)", shell.Name, got, err)
	}
}
