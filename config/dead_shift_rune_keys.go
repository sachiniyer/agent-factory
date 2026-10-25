package config

import (
	"fmt"
	"sync"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/log"
)

// deadShiftRuneWarned memoizes which config (source, action, key) dead
// shift+<rune> bindings have already been warned about. The daemon reloads
// config on every session-create — the same pattern that made an unmemoized
// notice the largest source of WARNING noise in agent-factory.log (#2496) — so
// a dead binding deserves ONE warning per source, not one per load. The key
// embeds the config path, the action, and the key string, so two distinct
// dead bindings each still warn while a re-read of the same one stays silent.
// Mirrors removedPRKeysWarned and unknownTableLeafWarned.
var deadShiftRuneWarned sync.Map

// discardDeadShiftRuneOverrides applies the "warn now, reject later" policy
// (#4599) to [keys] overrides a user already has on disk. Each shift+<rune>
// binding Bubble Tea can never emit — Key has no Shift field, so Shift+A is
// emitted as "A", never "shift+a" — is logged once as a warning that names the
// key and says it will never fire, then dropped, so a config that already
// contains one upgrades without refusing to start. The binding was inert
// before this change and stays inert after it, just loudly.
//
// Every other [keys] defect (unknown action, malformed key, reserved key,
// conflict) stays a hard error — keys.ValidateOverrides below still rejects
// those — and writing a NEW dead binding via `af config set keys` still rejects,
// because that path calls keys.ValidateOverrides directly. The hard error for
// an existing dead binding is a separate, later change.
//
// raw is the on-disk [keys] table the editor (CurrentValue/ManifestWithValues)
// serializes back to the config panes, and that an unchanged pane save routes
// through SetGlobalConfigValue — whose structured keys validation calls
// keys.ValidateOverrides and would reject the dead spec the loader just
// warned and skipped. Dropping the dead bindings only from the local overrides
// would leave the raw value pre-filling the editor with the dead spec, so an
// upgrade config that loaded could not be saved unchanged. The dead bindings
// are dropped from raw too, in the same shape normalizeKeyOverrides reads
// (string for a single binding, []any for a list), so the editor shows exactly
// what the loader applied and an unchanged save round-trips.
func discardDeadShiftRuneOverrides(raw map[string]any, overrides map[string][]string, prettyConfigPath string) map[string][]string {
	if len(overrides) == 0 {
		return overrides
	}
	cleaned := make(map[string][]string, len(overrides))
	for action, keyList := range overrides {
		// allDeadKnown is true only for a KNOWN action whose EVERY binding is a
		// dead shift+<rune> spec. For such an action the loader keeps an empty
		// key list (the disabled state) instead of falling back to the default,
		// so the warning's remediation must NOT tell the user to remove the
		// override — removing it would make buildMaps restore the default on the
		// next load, undoing the disabled state this change preserves. Tell the
		// user to replace it with a reachable binding (or [] to keep it
		// disabled) instead. A PARTIAL-dead action (some reachable bindings
		// remain) and an UNKNOWN action both still say "remove it": the partial
		// case keeps its live bindings either way, and the unknown case is
		// rejected by ValidateOverrides (the dead binding is not the load's
		// reason for keeping it).
		deadCount := 0
		for _, k := range keyList {
			if keys.IsDeadShiftRuneSpec(k) {
				deadCount++
			}
		}
		allDeadKnown := deadCount > 0 && deadCount == len(keyList) && keys.IsRebindableAction(action)
		kept := make([]string, 0, len(keyList))
		for _, k := range keyList {
			if keys.IsDeadShiftRuneSpec(k) {
				warnDeadShiftRuneBinding(prettyConfigPath, action, k, allDeadKnown)
				continue
			}
			kept = append(kept, k)
		}
		switch {
		case len(kept) == len(keyList):
			cleaned[action] = keyList
		case len(kept) > 0:
			cleaned[action] = kept
			cleanRawKeysTableEntry(raw, action, kept)
		default:
			// Every binding was a dead shift+<rune> spec. Dropping them all
			// would omit this action from cleaned, and a typo'd action such
			// as `typo = "shift+a"` would reach keys.ValidateOverrides as an
			// empty map and load — the unknown-action hard error that should
			// catch the typo gets hidden behind the dead-key warning. Preserve
			// the original entry for an UNKNOWN action so the validator can
			// still reject it (its unknown-action check runs before any key
			// validation, so the dead spec never reaches normalizeKeySpec). A
			// KNOWN action whose only bindings were dead warns and drops them,
			// but it must not fall back to its default keys: an existing
			// [keys] table that bound a known action to a dead spec (e.g.
			// `kill = "shift+x"`) was effectively disabling the action (the
			// dead binding never fired and displaced the default), and
			// silently re-enabling the default on upgrade would be a behaviour
			// change the user did not make. Preserve an explicit
			// no-active-keys state instead: an empty key list marks the action
			// as overridden with no keys, so buildMaps suppresses its defaults
			// and the action stays disabled. The raw [keys] table is updated
			// to match, so the editor (CurrentValue) shows the disabled state
			// and an unchanged save round-trips through SetGlobalConfigValue.
			if !keys.IsRebindableAction(action) {
				cleaned[action] = keyList
			} else {
				cleaned[action] = []string{}
				if raw != nil {
					raw[action] = []any{}
				}
			}
		}
	}
	return cleaned
}

// cleanRawKeysTableEntry rewrites the on-disk [keys] entry for action so it
// holds only the reachable bindings the loader kept, in the shape
// normalizeKeyOverrides reads (string for a single binding, []any for a list).
// The dead shift+<rune> spec has already been dropped from overrides; this
// mirrors that in raw so the editor (CurrentValue) shows the cleaned value and
// an unchanged pane save (SetGlobalConfigValue → keys.ValidateOverrides) does
// not reject the dead spec the load just warned and skipped.
func cleanRawKeysTableEntry(raw map[string]any, action string, kept []string) {
	if raw == nil {
		return
	}
	switch raw[action].(type) {
	case []any:
		list := make([]any, 0, len(kept))
		for _, k := range kept {
			list = append(list, k)
		}
		raw[action] = list
	default:
		// A single-string entry with a dead binding never reaches the
		// len(kept) > 0 branch (one binding is either dead or it isn't), so
		// this only happens when a multi-element list collapsed to one; keep
		// the compact string form the editor round-trips.
		if len(kept) == 1 {
			raw[action] = kept[0]
		} else {
			list := make([]any, 0, len(kept))
			for _, k := range kept {
				list = append(list, k)
			}
			raw[action] = list
		}
	}
}

// warnDeadShiftRuneBinding emits the once-per-source warning for one dead
// shift+<rune> override through every surface that loads the config: the
// shared project logger (log.WarningLog), which the daemon log shows as
// WARNING and af doctor surfaces, and the interactive writer, which a CLI
// command mirrors to stderr. Mirrors warnUnknownTableLeaf.
//
// allDeadKnown is true when this dead binding is the ONLY binding for a KNOWN
// action: the loader keeps that action with an empty key list so buildMaps
// suppresses its default (the disabled state), and the warning must NOT tell
// the user to remove the override — removing it would restore the default on
// the next load, undoing the disabled state. Tell the user to replace it with
// a reachable binding (or [] to keep the action disabled) instead.
func warnDeadShiftRuneBinding(prettyConfigPath, action, key string, allDeadKnown bool) {
	source := prettyConfigPath + ":" + action + ":" + key
	if _, seen := deadShiftRuneWarned.LoadOrStore(source, struct{}{}); seen {
		return
	}
	remediation := "Remove it from [keys] to silence this warning."
	if allDeadKnown {
		remediation = "Replace it with a reachable binding, or set the action to an empty list ([]) to keep it disabled, to silence this warning. Removing the entry would restore its default key, undoing the disabled state."
	}
	msg := fmt.Sprintf("config %s: keys.%q = %q will never fire — Bubble Tea has no Shift field for a plain rune (Shift+A is emitted as the rune \"A\", never a \"shift+\" spelling); ignoring this binding. %s", prettyConfigPath, action, key, remediation)
	log.WarningLog.Print(msg)
	writeInteractiveWarning(msg)
}

// resetDeadShiftRuneWarnings clears the once-per-source memo, so a test
// observing the warning log sees the notice fire regardless of whether an
// earlier test in the same process already warned for the same source.
func resetDeadShiftRuneWarnings() {
	deadShiftRuneWarned.Clear()
}
