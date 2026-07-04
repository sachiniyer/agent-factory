package keys

import (
	"strings"
	"testing"
)

// resetAfter restores the default maps when the test finishes, since
// ApplyOverrides mutates package globals.
func resetAfter(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if err := ApplyOverrides(nil); err != nil {
			t.Fatalf("restoring default keymap: %v", err)
		}
	})
}

// TestDefaultMapsMatchLegacyTable pins the generated defaults to the exact
// hand-written maps this package carried before the spec registry (#1026):
// a regression here means a user's muscle memory changed without a rebind.
func TestDefaultMapsMatchLegacyTable(t *testing.T) {
	legacyStrings := map[string]KeyName{
		"up":         KeyUp,
		"k":          KeyUp,
		"down":       KeyDown,
		"j":          KeyDown,
		"shift+up":   KeyShiftUp,
		"shift+down": KeyShiftDown,
		"N":          KeyNewRemote,
		"enter":      KeyEnter,
		"o":          KeyAttach,
		"n":          KeyNew,
		"D":          KeyKill,
		"q":          KeyQuit,
		"tab":        KeyTab,
		"shift+tab":  KeyShiftTab,
		"t":          KeyNewTab,
		"w":          KeyCloseTab,
		"?":          KeyHelp,
		"s":          KeyOpenPane,
		"x":          KeyHidePane,
		"S":          KeyTaskList,
		"/":          KeySearch,
		"p":          KeyOpenPR,
		"P":          KeyCopyPR,
		"H":          KeyHooks,
		"h":          KeyLeft,
		"left":       KeyLeft,
		"l":          KeyRight,
		"right":      KeyRight,
		"]":          KeyNextSection,
		"[":          KeyPrevSection,
	}
	if len(GlobalKeyStringsMap) != len(legacyStrings) {
		t.Fatalf("GlobalKeyStringsMap has %d entries, want %d", len(GlobalKeyStringsMap), len(legacyStrings))
	}
	for k, want := range legacyStrings {
		if got, ok := GlobalKeyStringsMap[k]; !ok || got != want {
			t.Fatalf("GlobalKeyStringsMap[%q] = %v (present=%v), want %v", k, got, ok, want)
		}
	}

	helpChecks := map[KeyName]string{
		KeyUp:                "↑/k",
		KeyDown:              "↓/j",
		KeyShiftUp:           "⇧↑",
		KeyShiftDown:         "⇧↓",
		KeyEnter:             "↵",
		KeyManageAutomations: "↵",
		KeyJumpTab:           "1-9",
		KeySubmitName:        "enter",
		KeyChangeProgram:     "tab",
		KeyExitInteractive:   "ctrl+]",
		KeyLeft:              "h/←",
		KeyRight:             "l/→",
		KeyQuit:              "q",
	}
	for name, want := range helpChecks {
		if got := GlobalKeyBindings[name].Help().Key; got != want {
			t.Fatalf("help label for %v = %q, want %q", name, got, want)
		}
	}
	if len(GlobalKeyBindings) != len(specs) {
		t.Fatalf("GlobalKeyBindings has %d entries, want one per spec (%d)", len(GlobalKeyBindings), len(specs))
	}
}

func TestApplyOverridesRebindsDispatchAndHelp(t *testing.T) {
	resetAfter(t)
	err := ApplyOverrides(map[string][]string{
		"quit": {"Q"},
		"up":   {"u", "ctrl+p"},
	})
	if err != nil {
		t.Fatalf("ApplyOverrides: %v", err)
	}

	if got := GlobalKeyStringsMap["Q"]; got != KeyQuit {
		t.Fatalf("Q should dispatch KeyQuit, got %v", got)
	}
	if _, still := GlobalKeyStringsMap["q"]; still {
		t.Fatalf("an override replaces the default binding entirely; q must be unbound")
	}
	if got := GlobalKeyStringsMap["ctrl+p"]; got != KeyUp {
		t.Fatalf("ctrl+p should dispatch KeyUp, got %v", got)
	}
	if _, still := GlobalKeyStringsMap["k"]; still {
		t.Fatalf("k must be unbound after the up override")
	}

	if got := GlobalKeyBindings[KeyQuit].Help().Key; got != "Q" {
		t.Fatalf("help label must reflect the rebind, got %q", got)
	}
	if got := GlobalKeyBindings[KeyUp].Help().Key; got != "u/ctrl+p" {
		t.Fatalf("multi-key help label = %q, want u/ctrl+p", got)
	}
	// Unlisted actions keep their defaults.
	if got := GlobalKeyStringsMap["D"]; got != KeyKill {
		t.Fatalf("unlisted actions must keep defaults; D = %v", got)
	}
}

func TestValidateOverridesErrors(t *testing.T) {
	cases := []struct {
		name      string
		overrides map[string][]string
		wantErr   string
	}{
		{"unknown action", map[string][]string{"warp": {"z"}}, "unknown action"},
		{"empty key list", map[string][]string{"quit": {}}, "no keys"},
		{"invalid key string", map[string][]string{"quit": {"space bar"}}, "not a valid key"},
		{"reserved enter", map[string][]string{"quit": {"enter"}}, "reserved"},
		{"reserved tab-jump digit", map[string][]string{"quit": {"3"}}, "reserved"},
		{"reserved interactive exit", map[string][]string{"quit": {"ctrl+]"}}, "reserved"},
		{"conflict with another default", map[string][]string{"kill": {"q"}}, "bound to both"},
		{"conflict between two overrides", map[string][]string{"kill": {"z"}, "quit": {"z"}}, "bound to both"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateOverrides(tc.overrides)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q should contain %q", err.Error(), tc.wantErr)
			}
		})
	}

	// Swapping two defaults in one table is legal: the conflict check runs
	// on the effective keys, not the defaults.
	if err := ValidateOverrides(map[string][]string{"quit": {"D"}, "kill": {"q"}}); err != nil {
		t.Fatalf("swapping two bindings must validate, got: %v", err)
	}
}

func TestValidateOverridesLeavesGlobalsUntouched(t *testing.T) {
	if err := ValidateOverrides(map[string][]string{"quit": {"Q"}}); err != nil {
		t.Fatalf("ValidateOverrides: %v", err)
	}
	if _, rebound := GlobalKeyStringsMap["Q"]; rebound {
		t.Fatalf("ValidateOverrides must not mutate the global maps")
	}
	if got := GlobalKeyStringsMap["q"]; got != KeyQuit {
		t.Fatalf("q must still dispatch KeyQuit after a validate-only call")
	}
}

func TestValidKeySpec(t *testing.T) {
	valid := []string{"q", "Q", "?", "/", "[", "å", "ctrl+a", "alt+x", "shift+up", "ctrl+shift+up", "f5", "space", "pgup", "ctrl+enter"}
	for _, s := range valid {
		if !validKeySpec(s) {
			t.Fatalf("validKeySpec(%q) = false, want true", s)
		}
	}
	invalid := []string{"", " ", "space bar", "qq", "ctrl+", "ctrl+alt+", "control+a", "\t"}
	for _, s := range invalid {
		if validKeySpec(s) {
			t.Fatalf("validKeySpec(%q) = true, want false", s)
		}
	}
}

func TestEffectiveBindings(t *testing.T) {
	infos, err := EffectiveBindings(map[string][]string{"quit": {"Q"}})
	if err != nil {
		t.Fatalf("EffectiveBindings: %v", err)
	}
	if len(infos) != len(specs) {
		t.Fatalf("got %d infos, want one per spec (%d)", len(infos), len(specs))
	}
	// Rebindable actions come first, sorted; fixed bindings trail.
	seenFixed := false
	byAction := map[string]BindingInfo{}
	var prev string
	for _, info := range infos {
		if info.Action == "" {
			seenFixed = true
			continue
		}
		if seenFixed {
			t.Fatalf("rebindable action %q listed after a fixed binding", info.Action)
		}
		if prev != "" && info.Action <= prev {
			t.Fatalf("rebindable actions not sorted: %q after %q", info.Action, prev)
		}
		prev = info.Action
		byAction[info.Action] = info
	}
	if !seenFixed {
		t.Fatal("fixed bindings missing from the output")
	}

	quit := byAction["quit"]
	if !quit.Rebound || len(quit.Keys) != 1 || quit.Keys[0] != "Q" || len(quit.Default) != 1 || quit.Default[0] != "q" {
		t.Fatalf("quit info = %+v, want rebound Q with default q", quit)
	}
	up := byAction["up"]
	if up.Rebound || len(up.Keys) != 2 || up.Keys[0] != "up" || up.Keys[1] != "k" {
		t.Fatalf("up info = %+v, want default up/k not rebound", up)
	}

	// A broken table reports the same error the TUI would refuse to start
	// with, instead of printing a keymap that is not in effect.
	if _, err := EffectiveBindings(map[string][]string{"kill": {"q"}}); err == nil {
		t.Fatal("EffectiveBindings must reject a conflicting table")
	}

	// The global maps are untouched by introspection.
	if got := GlobalKeyStringsMap["q"]; got != KeyQuit {
		t.Fatalf("EffectiveBindings must not mutate the global maps")
	}
}

func TestRebindableActionsSortedAndComplete(t *testing.T) {
	actions := RebindableActions()
	if len(actions) == 0 {
		t.Fatal("no rebindable actions")
	}
	for i := 1; i < len(actions); i++ {
		if actions[i-1] >= actions[i] {
			t.Fatalf("actions not sorted/unique at %q >= %q", actions[i-1], actions[i])
		}
	}
	// The structural/reserved actions must NOT be rebindable.
	forbidden := map[string]bool{"enter": true, "tab": true, "shift_tab": true, "jump_tab": true, "submit_name": true, "change_program": true, "exit_interactive": true, "manage_automations": true}
	for _, a := range actions {
		if forbidden[a] {
			t.Fatalf("action %q must not be rebindable", a)
		}
	}
}
