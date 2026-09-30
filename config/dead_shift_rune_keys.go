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
func discardDeadShiftRuneOverrides(overrides map[string][]string, prettyConfigPath string) map[string][]string {
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
			// left out of cleaned as before.
			if !keys.IsRebindableAction(action) {
				cleaned[action] = keyList
			}
		}
	}
	return cleaned
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
	msg := fmt.Sprintf("config %s: keys.%s = %q will never fire — Bubble Tea has no Shift field for a plain rune (Shift+A is emitted as the rune \"A\", never a \"shift+\" spelling); ignoring this binding. Remove it from [keys] to silence this warning.", prettyConfigPath, action, key)
	log.WarningLog.Print(msg)
	writeInteractiveWarning(msg)
}

// resetDeadShiftRuneWarnings clears the once-per-source memo, so a test
// observing the warning log sees the notice fire regardless of whether an
// earlier test in the same process already warned for the same source.
func resetDeadShiftRuneWarnings() {
	deadShiftRuneWarned.Clear()
}
