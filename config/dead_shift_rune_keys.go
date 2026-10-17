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
		kept := make([]string, 0, len(keyList))
		for _, k := range keyList {
			if keys.IsDeadShiftRuneSpec(k) {
				warnDeadShiftRuneBinding(prettyConfigPath, action, k)
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
			// KNOWN action whose only bindings were dead warns, drops them
			// all, and resolves to its default keys — the upgrade case
			// (`quit = ["shift+a"]`) the warn-and-skip exists for — so it is
			// left out of cleaned as before, and dropped from raw so the
			// editor does not pre-fill the dead spec.
			if !keys.IsRebindableAction(action) {
				cleaned[action] = keyList
			} else if raw != nil {
				delete(raw, action)
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
func warnDeadShiftRuneBinding(prettyConfigPath, action, key string) {
	source := prettyConfigPath + ":" + action + ":" + key
	if _, seen := deadShiftRuneWarned.LoadOrStore(source, struct{}{}); seen {
		return
	}
	msg := fmt.Sprintf("config %s: keys.%q = %q will never fire — Bubble Tea has no Shift field for a plain rune (Shift+A is emitted as the rune \"A\", never a \"shift+\" spelling); ignoring this binding. Remove it from [keys] to silence this warning.", prettyConfigPath, action, key)
	log.WarningLog.Print(msg)
	writeInteractiveWarning(msg)
}

// resetDeadShiftRuneWarnings clears the once-per-source memo, so a test
// observing the warning log sees the notice fire regardless of whether an
// earlier test in the same process already warned for the same source.
func resetDeadShiftRuneWarnings() {
	deadShiftRuneWarned.Clear()
}
