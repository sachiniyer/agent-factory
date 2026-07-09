package keys

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/key"
)

type KeyName int

const (
	KeyUp KeyName = iota
	KeyDown
	KeyEnter
	KeyNew
	KeyKill
	KeyArchive    // Archive a live session / restore an archived, Lost, or Dead one (#1028, #1300)
	KeyLimitRetry // Retry a session blocked at a usage-limit wall (#1146)
	KeyQuit
	KeyErrorDetails // Show the full last error when the status line is truncated (#1423).

	KeyTab        // Tab cycles the workspace focus ring (tree → pane → automations).
	KeyShiftTab   // ShiftTab cycles the focus ring in reverse.
	KeySubmitName // SubmitName is a special keybinding for submitting the name of a new instance.

	KeyNewTab   // NewTab spawns a new shell tab in the selected instance (#930 PR 4).
	KeyCloseTab // CloseTab closes the active (non-agent) tab (#930 PR 4).
	KeyJumpTab  // JumpTab is the 1-9 number-key jump hint; dispatched manually, menu/help only.

	KeyNewRemote // Key for creating a new remote instance
	KeyHelp      // Key for showing help screen

	// Diff keybindings
	KeyShiftUp
	KeyShiftDown

	KeyTaskList

	KeySearch // Key for searching sessions

	KeyOpenPR // Key for opening PR in browser
	KeyCopyPR // Key for copying PR URL to clipboard
	KeyHooks  // Key for editing post-worktree hooks

	KeyChangeProgram // Key for changing the program during new instance naming
	KeyCancelName    // Display-only cancel hint during new instance naming

	// Sidebar navigation
	KeyLeft        // Collapse section / move to parent
	KeyRight       // Expand section
	KeyNextSection // Jump to next section header
	KeyPrevSection // Jump to previous section header

	// N-pane workspace verbs (#1088, replaces the PR-5 A/B split): `s` opens
	// the selected tab as a new vertical-split pane (or focuses its pane when
	// already open); `S` commits a #1321 preview beside the owner pane; `x`
	// hides the focused pane back to the background — the tab keeps running,
	// nothing is killed.
	KeyOpenPane  // OpenPane: open the selected tab as a pane / focus its pane.
	KeySplitPane // SplitPane commits the active preview as a pane beside the owner.
	KeyHidePane  // HidePane hides the focused pane back to the background.
	KeyPanePrev  // PanePrev focuses the previous visible workspace pane.
	KeyPaneNext  // PaneNext focuses the next visible workspace pane.

	// KeyManageAutomations is the automations-section display alias of Enter
	// (menu/help only): with the in-rail section focused, Enter opens the task
	// manager overlay. Dispatch is root-routed (handleAutomationsFocus), so
	// this name never appears in GlobalKeyStringsMap.
	KeyManageAutomations

	// Interactive mode (#1089, RFC §2.3): Enter on a live pane enters it —
	// every subsequent keystroke (including Tab) forwards to the agent/shell
	// in-pane, no full-screen takeover. KeyAttach keeps the full-screen tmux
	// attach reachable on its own key (`o`, previously an Enter alias).
	KeyAttach
	// KeyExitInteractive is the ONLY host-reserved key while interactive
	// (menu/help display only): Ctrl-] returns to nav mode. Dispatch is
	// root-routed before any key map (handleInteractiveKey), so this name
	// never appears in GlobalKeyStringsMap.
	KeyExitInteractive
)

// spec is one action's canonical binding definition: its default keys, help
// rendering, and — when configKey is non-empty — the name under which the
// `[keys]` table in config.toml may rebind it (#1030/#1026).
type spec struct {
	name KeyName
	// configKey is the action's name in the [keys] config table; "" marks a
	// fixed binding that config cannot touch (structural keys like enter/tab
	// and root-routed keys like ctrl+]).
	configKey string
	// keys are the default key strings (bubbletea tea.KeyMsg.String() forms).
	keys []string
	// helpLabel, when non-empty, pins the help-column text for the DEFAULT
	// binding where the generic rendering would differ (e.g. "1-9").
	// Overridden bindings always derive their label from the actual keys.
	helpLabel string
	// desc is the help-column description.
	desc string
	// dispatch marks actions routed through GlobalKeyStringsMap. Display-only
	// entries (KeyJumpTab, KeySubmitName, ...) never enter the strings map —
	// their dispatch is root-routed or overlay-local.
	dispatch bool
	// contextual marks actions routed by a focused UI region before
	// GlobalKeyStringsMap. They are still validated, rebindable, and rendered
	// in help/status surfaces, but they may share a physical key with one
	// explicitly disjoint global action.
	contextual bool
}

// specs is the canonical binding table. Order is stable but carries no
// meaning; lookups go through the generated maps.
var specs = []spec{
	{name: KeyUp, configKey: "up", keys: []string{"up", "k"}, desc: "up", dispatch: true},
	{name: KeyDown, configKey: "down", keys: []string{"down", "j"}, desc: "down", dispatch: true},
	{name: KeyShiftUp, configKey: "scroll_up", keys: []string{"ctrl+u"}, desc: "scroll", dispatch: true},
	{name: KeyShiftDown, configKey: "scroll_down", keys: []string{"ctrl+d"}, desc: "scroll", dispatch: true},
	{name: KeyEnter, keys: []string{"enter"}, desc: "interact", dispatch: true},
	// "o" was an Enter alias until #1089 PR 2 split the verbs: Enter enters
	// the pane (interactive), o keeps the old full-screen attach.
	{name: KeyAttach, configKey: "attach", keys: []string{"o"}, desc: "attach", dispatch: true},
	{name: KeyExitInteractive, keys: []string{"ctrl+]"}, desc: "nav mode"},
	{name: KeyNew, configKey: "new", keys: []string{"n"}, desc: "new", dispatch: true},
	{name: KeyKill, configKey: "kill", keys: []string{"D"}, desc: "kill", dispatch: true},
	{name: KeyArchive, configKey: "archive", keys: []string{"a"}, desc: "archive/restore", dispatch: true},
	{name: KeyLimitRetry, configKey: "limit_retry", keys: []string{"c"}, desc: "retry limit", dispatch: true},
	{name: KeyHelp, configKey: "help", keys: []string{"?"}, desc: "help", dispatch: true},
	{name: KeyQuit, configKey: "quit", keys: []string{"q"}, desc: "quit", dispatch: true},
	{name: KeyErrorDetails, configKey: "error_details", keys: []string{"E"}, desc: "details", dispatch: true},
	{name: KeyNewRemote, configKey: "new_remote", keys: []string{"N"}, desc: "new remote", dispatch: true},
	{name: KeyTab, keys: []string{"tab"}, desc: "focus", dispatch: true},
	{name: KeyShiftTab, keys: []string{"shift+tab"}, desc: "focus prev", dispatch: true},
	{name: KeyNewTab, configKey: "new_tab", keys: []string{"t"}, desc: "new tab", dispatch: true},
	{name: KeyCloseTab, configKey: "close_tab", keys: []string{"w"}, desc: "close tab", dispatch: true},
	{name: KeyJumpTab, keys: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"}, helpLabel: "1-9", desc: "jump"},
	{name: KeyTaskList, configKey: "tasks", keys: []string{"m"}, desc: "tasks", dispatch: true},
	{name: KeyManageAutomations, keys: []string{"enter"}, desc: "manage"},
	{name: KeyOpenPane, configKey: "open_pane", keys: []string{"s"}, desc: "open pane", dispatch: true},
	{name: KeySplitPane, configKey: "split_pane", keys: []string{"S"}, desc: "split pane", dispatch: true},
	{name: KeyHidePane, configKey: "hide_pane", keys: []string{"x"}, desc: "hide pane", dispatch: true},
	{name: KeyPanePrev, configKey: "pane_prev", keys: []string{"left"}, desc: "prev pane", contextual: true},
	{name: KeyPaneNext, configKey: "pane_next", keys: []string{"right"}, desc: "next pane", contextual: true},
	{name: KeySearch, configKey: "search", keys: []string{"/"}, desc: "search", dispatch: true},
	{name: KeyOpenPR, configKey: "open_pr", keys: []string{"p"}, desc: "open PR", dispatch: true},
	{name: KeyCopyPR, configKey: "copy_pr", keys: []string{"y"}, desc: "copy PR URL", dispatch: true},
	{name: KeyHooks, configKey: "hooks", keys: []string{"e"}, desc: "worktree hooks", dispatch: true},
	{name: KeyLeft, configKey: "collapse", keys: []string{"h", "left"}, desc: "collapse", dispatch: true},
	{name: KeyRight, configKey: "expand", keys: []string{"l", "right"}, desc: "expand", dispatch: true},
	{name: KeyNextSection, configKey: "next_section", keys: []string{"]"}, desc: "next section", dispatch: true},
	{name: KeyPrevSection, configKey: "prev_section", keys: []string{"["}, desc: "prev section", dispatch: true},

	// -- Special keybindings --
	{name: KeySubmitName, keys: []string{"enter"}, helpLabel: "enter", desc: "submit name"},
	{name: KeyChangeProgram, keys: []string{"tab"}, desc: "change program"},
	{name: KeyCancelName, keys: []string{"esc"}, desc: "cancel"},
}

// reservedKeys are key strings the config may never bind an action to: they
// are either structural (enter/tab/shift+tab drive interaction and the focus
// ring, esc is the root-routed overlay-cancel key), root-routed before any
// key map (ctrl+], the one host-reserved key in interactive mode — rebinding
// it could lock a user inside a forwarded pane), or dispatched manually (the
// 1-9 tab jump).
var reservedKeys = map[string]string{
	"enter":     "it is the interact/submit key",
	"tab":       "it cycles the focus ring",
	"shift+tab": "it cycles the focus ring",
	"esc":       "it cancels overlays",
	"ctrl+]":    "it is the reserved exit from interactive mode",
	"1":         "1-9 jump to tabs",
	"2":         "1-9 jump to tabs",
	"3":         "1-9 jump to tabs",
	"4":         "1-9 jump to tabs",
	"5":         "1-9 jump to tabs",
	"6":         "1-9 jump to tabs",
	"7":         "1-9 jump to tabs",
	"8":         "1-9 jump to tabs",
	"9":         "1-9 jump to tabs",
}

// keyDisplayNames maps key strings to their compact help-column glyphs.
// Anything absent renders as itself.
var keyDisplayNames = map[string]string{
	"up":         "↑",
	"down":       "↓",
	"left":       "←",
	"right":      "→",
	"enter":      "↵",
	"shift+up":   "⇧↑",
	"shift+down": "⇧↓",
	" ":          "space",
	"ctrl+@":     "ctrl+space",
}

// namedKeys are the non-rune key names bubbletea produces (tea.KeyMsg.String()
// forms) that a [keys] value may use, optionally behind ctrl+/alt+/shift+
// modifiers.
var namedKeys = map[string]bool{
	"up": true, "down": true, "left": true, "right": true,
	"home": true, "end": true, "pgup": true, "pgdown": true,
	"space": true, "backspace": true, "delete": true, "insert": true,
	// enter/tab/esc are valid key NAMES (so modifier combos parse) even
	// though their bare forms sit in reservedKeys.
	"enter": true, "tab": true, "esc": true,
	"f1": true, "f2": true, "f3": true, "f4": true, "f5": true, "f6": true,
	"f7": true, "f8": true, "f9": true, "f10": true, "f11": true, "f12": true,
}

// GlobalKeyStringsMap is a global map from key string to action, generated
// from specs (plus any [keys] overrides applied at startup). Read-only after
// ApplyOverrides; the TUI treats it as immutable.
var GlobalKeyStringsMap map[string]KeyName

// GlobalKeyBindings is a global map of KeyName to keybinding, generated from
// specs (plus any [keys] overrides applied at startup). Read-only after
// ApplyOverrides; the TUI treats it as immutable.
var GlobalKeyBindings map[KeyName]key.Binding

func init() {
	// Defaults must always build; a panic here means the specs table itself
	// is inconsistent, which no config can cause.
	strings, bindings, err := buildMaps(nil)
	if err != nil {
		panic(fmt.Sprintf("keys: default binding table is invalid: %v", err))
	}
	GlobalKeyStringsMap, GlobalKeyBindings = strings, bindings
}

// ValidateOverrides checks a [keys] override table (action name → key list)
// without applying it: unknown actions, empty or malformed key strings,
// reserved keys, and key conflicts between actions are all hard errors, so a
// bad keymap fails at config load with the file named — never as a dead key
// at runtime.
func ValidateOverrides(overrides map[string][]string) error {
	_, _, err := buildMaps(overrides)
	return err
}

// ApplyOverrides rebuilds the global binding maps with the given [keys]
// overrides layered over the defaults. Call once at TUI startup, before the
// bubbletea program runs — the maps are read concurrently afterwards. Help
// and menu labels are regenerated from the effective keys, so rebinds are
// reflected everywhere the binding is displayed.
func ApplyOverrides(overrides map[string][]string) error {
	stringsMap, bindings, err := buildMaps(overrides)
	if err != nil {
		return err
	}
	GlobalKeyStringsMap, GlobalKeyBindings = stringsMap, bindings
	return nil
}

// BindingInfo describes one action's effective binding for introspection
// (`af keys`, #1026).
type BindingInfo struct {
	// Action is the [keys] table name; "" for fixed bindings config cannot
	// touch.
	Action string
	// Desc is the help-column description.
	Desc string
	// Keys are the effective key strings (defaults or the override).
	Keys []string
	// Default are the built-in key strings.
	Default []string
	// Rebound reports whether an override replaced the default.
	Rebound bool
}

// EffectiveBindings returns every action's effective binding with the given
// [keys] overrides applied, in the canonical display order (rebindable
// actions sorted by name, then the fixed bindings in table order). The
// overrides are validated first, so a broken table reports the same error
// the TUI would refuse to start with.
func EffectiveBindings(overrides map[string][]string) ([]BindingInfo, error) {
	if err := ValidateOverrides(overrides); err != nil {
		return nil, err
	}
	byConfigKey := specsByConfigKey()
	normalizedOverrides, err := normalizeOverrides(overrides, byConfigKey)
	if err != nil {
		return nil, err
	}
	var rebindable, fixed []BindingInfo
	for _, sp := range specs {
		info := BindingInfo{Action: sp.configKey, Desc: sp.desc, Keys: sp.keys, Default: sp.keys}
		if sp.configKey != "" {
			if o, ok := normalizedOverrides[sp.configKey]; ok {
				info.Keys = o
				info.Rebound = true
			}
			rebindable = append(rebindable, info)
			continue
		}
		fixed = append(fixed, info)
	}
	sort.Slice(rebindable, func(i, j int) bool { return rebindable[i].Action < rebindable[j].Action })
	return append(rebindable, fixed...), nil
}

// RebindableActions returns the sorted [keys] table names of every action
// config may rebind, for validation error messages.
func RebindableActions() []string {
	var names []string
	for _, sp := range specs {
		if sp.configKey != "" {
			names = append(names, sp.configKey)
		}
	}
	sort.Strings(names)
	return names
}

// buildMaps generates the strings and bindings maps from specs with
// overrides applied, validating as it goes.
func buildMaps(overrides map[string][]string) (map[string]KeyName, map[KeyName]key.Binding, error) {
	byConfigKey := specsByConfigKey()
	normalizedOverrides, err := normalizeOverrides(overrides, byConfigKey)
	if err != nil {
		return nil, nil, err
	}

	stringsMap := make(map[string]KeyName, 64)
	bindings := make(map[KeyName]key.Binding, len(specs))
	// boundBy tracks which action owns each dispatch/contextual key, to name
	// both sides of a conflict. Fixed dispatch specs (enter/tab/shift+tab)
	// participate, so an override cannot silently shadow them either (they
	// are also in reservedKeys, which reports the clearer error first).
	boundBy := map[string]bindingOwner{}
	for _, sp := range specs {
		effective := sp.keys
		overridden := false
		if sp.configKey != "" {
			if o, ok := normalizedOverrides[sp.configKey]; ok {
				effective = o
				overridden = true
			}
		}

		if sp.dispatch || sp.contextual {
			for _, k := range effective {
				owner := bindingOwner{name: sp.name, action: ownerAction(sp)}
				if prev, taken := boundBy[k]; taken {
					if !contextualKeyOverlapAllowed(prev.name, owner.name) {
						return nil, nil, fmt.Errorf("keys: %q is bound to both %q and %q; each key can trigger only one action", k, prev.action, owner.action)
					}
				} else {
					boundBy[k] = owner
				}
				if !sp.dispatch {
					continue
				}
				stringsMap[k] = sp.name
			}
		}

		label := sp.helpLabel
		if label == "" || overridden {
			label = helpLabelFor(effective)
		}
		bindings[sp.name] = key.NewBinding(
			key.WithKeys(effective...),
			key.WithHelp(label, sp.desc),
		)
	}
	return stringsMap, bindings, nil
}

type bindingOwner struct {
	name   KeyName
	action string
}

func ownerAction(sp spec) string {
	if sp.configKey != "" {
		return sp.configKey
	}
	return sp.desc
}

func contextualKeyOverlapAllowed(a, b KeyName) bool {
	return (isPaneSwitchKey(a) && isTreeHorizontalKey(b)) ||
		(isPaneSwitchKey(b) && isTreeHorizontalKey(a))
}

func isPaneSwitchKey(name KeyName) bool {
	return name == KeyPanePrev || name == KeyPaneNext
}

func isTreeHorizontalKey(name KeyName) bool {
	return name == KeyLeft || name == KeyRight
}

func specsByConfigKey() map[string]spec {
	byConfigKey := make(map[string]spec, len(specs))
	for _, sp := range specs {
		if sp.configKey != "" {
			byConfigKey[sp.configKey] = sp
		}
	}
	return byConfigKey
}

func normalizeOverrides(overrides map[string][]string, byConfigKey map[string]spec) (map[string][]string, error) {
	if len(overrides) == 0 {
		return nil, nil
	}
	normalized := make(map[string][]string, len(overrides))
	for action, keyList := range overrides {
		if _, ok := byConfigKey[action]; !ok {
			return nil, fmt.Errorf("keys: unknown action %q (rebindable actions: %s)", action, strings.Join(RebindableActions(), ", "))
		}
		if len(keyList) == 0 {
			return nil, fmt.Errorf("keys: action %q has no keys; give it a key string or a list of key strings", action)
		}
		normalizedList := make([]string, 0, len(keyList))
		for _, k := range keyList {
			normalizedKey, ok := normalizeKeySpec(k)
			if !ok {
				return nil, fmt.Errorf("keys: action %q: %q is not a valid key (use a single character, a named key like \"up\" or \"f5\", or a ctrl+/alt+/shift+ combination)", action, k)
			}
			if reason, reserved := reservedKeys[normalizedKey]; reserved {
				return nil, fmt.Errorf("keys: action %q: %q is reserved — %s", action, normalizedKey, reason)
			}
			normalizedList = append(normalizedList, normalizedKey)
		}
		normalized[action] = normalizedList
	}
	return normalized, nil
}

// helpLabelFor renders a key list for the help/menu column: each key mapped
// through keyDisplayNames and joined with "/" (e.g. ["up","k"] → "↑/k").
func helpLabelFor(keyList []string) string {
	parts := make([]string, len(keyList))
	for i, k := range keyList {
		if display, ok := keyDisplayNames[k]; ok {
			parts[i] = display
		} else {
			parts[i] = k
		}
	}
	return strings.Join(parts, "/")
}

// validKeySpec reports whether s is a key string bubbletea can produce:
// an optional run of ctrl+/alt+/shift+ modifiers followed by a named key or
// a single character. Modifier order is accepted flexibly, but runtime maps
// store normalizeKeySpec's Bubble Tea spelling so override lookup compares
// against tea.KeyMsg.String() forms. Literal whitespace in config remains
// invalid; users spell the spacebar as "space", and normalizeKeySpec maps it
// to Bubble Tea's runtime spelling.
func validKeySpec(s string) bool {
	_, ok := normalizeKeySpec(s)
	return ok
}

func normalizeKeySpec(s string) (string, bool) {
	if s == "" || strings.ContainsAny(s, " \t\n") {
		return "", false
	}
	rest := s
	var ctrl, alt, shift bool
	for {
		switch {
		case strings.HasPrefix(rest, "ctrl+"):
			if ctrl {
				return "", false
			}
			ctrl = true
			rest = rest[len("ctrl+"):]
		case strings.HasPrefix(rest, "alt+"):
			if alt {
				return "", false
			}
			alt = true
			rest = rest[len("alt+"):]
		case strings.HasPrefix(rest, "shift+"):
			if shift {
				return "", false
			}
			shift = true
			rest = rest[len("shift+"):]
		default:
			if rest == "space" {
				if ctrl {
					rest = "@"
				} else if !alt && !shift {
					return " ", true
				}
			}
			if !namedKeys[rest] && utf8.RuneCountInString(rest) != 1 {
				return "", false
			}
			var b strings.Builder
			if alt {
				b.WriteString("alt+")
			}
			if ctrl {
				b.WriteString("ctrl+")
			}
			if shift {
				b.WriteString("shift+")
			}
			b.WriteString(rest)
			return b.String(), true
		}
		if rest == "" {
			return "", false
		}
	}
}
