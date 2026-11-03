package config

import (
	"errors"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/log"
)

// RemovedAutoYesMessage is shared by both compatibility warnings and new-input
// rejection for the removed auto_yes feature. Keeping one message prevents
// config, CLI, and HTTP callers from receiving different migration advice.
const RemovedAutoYesMessage = "auto_yes was removed (including --autoyes and --auto-yes); configure approval behavior directly in the agent command via program_overrides (for example, program_overrides.codex = \"codex --ask-for-approval never\")"

// RemovedAutoYesError returns the actionable migration error for a new write,
// removed CLI flag, or removed request field. Existing config files are a
// compatibility input: loaders ignore their stale key with a warning so an
// upgrade cannot prevent af or its daemon from starting.
func RemovedAutoYesError() error {
	return errors.New(RemovedAutoYesMessage)
}

func warnRemovedAutoYes(shape map[string]any, source string) {
	if removedAutoYesInShape(shape) {
		warnRemovedAutoYesAt(source)
	}
}

// warnRemovedAutoYesValue is warnRemovedAutoYes for the loaders that find the
// removed key themselves (in-repo, personal project) because they also have to
// strip it from their presence shape. They pass the decoded value so the same
// #2574 rule decides, rather than each re-deriving "is this worth saying".
func warnRemovedAutoYesValue(value any, source string) {
	if removedAutoYesWarrantsNotice(value) {
		warnRemovedAutoYesAt(source)
	}
}

// removedAutoYesWarrantsNotice reports whether a decoded auto_yes value earns
// the migration notice.
//
// `false` does not (#2574). The warning's job is to tell a user that behavior
// they asked for silently stopped happening: `auto_yes = true` genuinely
// changed meaning on upgrade, but `auto_yes = false` asked for nothing — the
// post-removal default IS "don't auto-approve", so that user already has
// exactly the behavior they configured. Pointing them at
// `program_overrides.codex = "codex --ask-for-approval never"` is advice to
// enable the thing they explicitly turned off. This is the difference between
// "your setting was dropped" and "your setting was redundant", and only the
// first is worth a notice on every af invocation forever.
//
// Anything that is not a boolean still warns: the value is malformed for a key
// that no longer exists, and silence there would hide a typo.
func removedAutoYesWarrantsNotice(value any) bool {
	enabled, isBool := value.(bool)
	return !isBool || enabled
}

// removedAutoYesWarned memoizes which sources have already received the
// auto_yes-removal heads-up. A genuinely removed key deserves ONE migration
// notice per source, not one per config load: the daemon loads config ~10x per
// session-create, which turned this single notice into 1046 lines over two days
// — the largest single source of WARNING noise in agent-factory.log (#2496).
//
// The key is the full source string, which already embeds the config kind and
// its path, so two distinct offending files each still get their own notice
// while a re-read of the same file stays silent. It is process-scoped: for the
// long-lived daemon that is "once per daemon lifetime"; a short-lived CLI
// invocation warns once and exits. sync.Map keeps the check-and-set atomic for
// the concurrent loads a session-create issues.
var removedAutoYesWarned sync.Map

func warnRemovedAutoYesAt(source string) {
	if _, seen := removedAutoYesWarned.LoadOrStore(source, struct{}{}); seen {
		return
	}
	log.WarningLog.Printf("%s: %s; the stale setting is ignored during upgrade", source, RemovedAutoYesMessage)
}

// resetRemovedAutoYesWarnings clears the once-per-source memo. Tests that assert
// the warning fires reset it first, so a source string already seen by an
// earlier test in the same process does not suppress the notice under test; it
// is wired into captureLog so every warning-observing test starts fresh.
func resetRemovedAutoYesWarnings() {
	removedAutoYesWarned.Clear()
}

// removedAutoYesInShape reports whether a decoded config carries an auto_yes
// worth warning about — at the top level or under any root_agents profile. It
// tests the VALUE, not mere presence: see removedAutoYesWarrantsNotice.
func removedAutoYesInShape(shape map[string]any) bool {
	if shape == nil {
		return false
	}
	if value, present := shape["auto_yes"]; present && removedAutoYesWarrantsNotice(value) {
		return true
	}
	rootAgents, ok := shape["root_agents"].(map[string]any)
	if !ok {
		return false
	}
	for _, rawProfile := range rootAgents {
		profile, ok := rawProfile.(map[string]any)
		if !ok {
			continue
		}
		if value, present := profile["auto_yes"]; present && removedAutoYesWarrantsNotice(value) {
			return true
		}
	}
	return false
}

func removedAutoYesKeyPath(key []string) bool {
	if len(key) == 1 {
		return key[0] == "auto_yes"
	}
	return len(key) >= 3 && key[0] == "root_agents" && key[len(key)-1] == "auto_yes"
}

// RemovedBacklogMessage is shared by the compatibility warning and new-input
// rejection for the removed Backlog feature's [backlog] table (#6662). One
// message keeps config, CLI, and HTTP callers on the same migration advice.
const RemovedBacklogMessage = "the Backlog feature was removed (#6662); the [backlog] table is ignored — existing backlog.json, budget.json and plan/ files are left in place, and session records written by item sessions still load"

// RemovedBacklogError returns the actionable removal error for a new write of
// a backlog.* key. Existing config files are a compatibility input: loaders
// ignore their stale table with at most a one-time warning so an upgrade
// cannot prevent af or its daemon from starting.
func RemovedBacklogError() error {
	return errors.New(RemovedBacklogMessage)
}

// removedBacklogKey reports whether key (a canonical config path like
// "backlog", "backlog.plan_usage" or "backlog.prices.claude-x.input") belongs
// to the removed [backlog] table.
func removedBacklogKey(key string) bool {
	return key == "backlog" || strings.HasPrefix(key, "backlog.")
}

// removedBacklogTableWarrantsNotice reports whether a decoded [backlog] table
// held settings that changed behavior — plan_usage on, or a prices override.
// A table holding only plan_usage=false asked for nothing: the post-removal
// default is the same nothing, so it earns no notice (the #2574 rule).
func removedBacklogTableWarrantsNotice(table map[string]any) bool {
	if enabled, isBool := table["plan_usage"].(bool); isBool && enabled {
		return true
	}
	for key := range table {
		if key != "plan_usage" {
			return true
		}
	}
	return false
}

// removedBacklogInShape reports whether a decoded config carries a [backlog]
// table worth the notice.
func removedBacklogInShape(shape map[string]any) bool {
	table, ok := shape["backlog"].(map[string]any)
	return ok && removedBacklogTableWarrantsNotice(table)
}

// removedBacklogWarned memoizes the once-per-source notice, mirroring
// removedAutoYesWarned: the daemon loads config ~10x per session-create, and a
// removed key deserves one migration notice per daemon lifetime, not one per
// load (#2496).
var removedBacklogWarned sync.Map

// warnRemovedBacklog emits the one-time notice when a global config source's
// shape carries a [backlog] table that did something.
func warnRemovedBacklog(shape map[string]any, source string) {
	if removedBacklogInShape(shape) {
		warnRemovedBacklogAt(source)
	}
}

// warnRemovedBacklogValue is warnRemovedBacklog for the loaders that strip the
// table from their presence shape themselves (in-repo, personal project).
func warnRemovedBacklogValue(value any, source string) {
	if table, ok := value.(map[string]any); ok && removedBacklogTableWarrantsNotice(table) {
		warnRemovedBacklogAt(source)
	}
}

func warnRemovedBacklogAt(source string) {
	if _, seen := removedBacklogWarned.LoadOrStore(source, struct{}{}); seen {
		return
	}
	log.WarningLog.Printf("%s: %s; the stale setting is ignored during upgrade", source, RemovedBacklogMessage)
}

// resetRemovedBacklogWarnings clears the once-per-source memo for tests, the
// same role resetRemovedAutoYesWarnings plays for auto_yes.
func resetRemovedBacklogWarnings() {
	removedBacklogWarned.Clear()
}

// removedKeyError returns the migration error for a canonical config key a
// removed feature owned — auto_yes, or anything under [backlog] — or nil when
// the key is live. The set/unset paths call it once instead of enumerating
// tombstones.
func removedKeyError(key string) error {
	if key == "auto_yes" {
		return RemovedAutoYesError()
	}
	if removedBacklogKey(key) {
		return RemovedBacklogError()
	}
	return nil
}

// removedBacklogKeyPath reports whether a TOML strict-decode key path (e.g.
// ["backlog"], ["backlog" "plan_usage"], ["backlog" "prices" "m" "input"])
// is inside the removed table, so the generic unknown-key warning skips it —
// the specific notice above already named it.
func removedBacklogKeyPath(key []string) bool {
	return len(key) > 0 && key[0] == "backlog"
}
