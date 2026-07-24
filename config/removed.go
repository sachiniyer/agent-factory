package config

import (
	"errors"
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

func removedAutoYesInShape(shape map[string]any) bool {
	if shape == nil {
		return false
	}
	if _, present := shape["auto_yes"]; present {
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
		if _, present := profile["auto_yes"]; present {
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
