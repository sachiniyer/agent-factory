package config

import (
	"regexp"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// sanitizeLimitPatterns validates the limit_patterns override map in place,
// dropping any entry that names an unknown agent or whose value is not a
// compilable Go regexp and logging one warning per drop (#1146).
//
// Warn-and-drop, not hard-error: an optional usage-limit detection tweak must
// never block config load — and thus the whole TUI/CLI — the way a bad key
// would. The built-in default for that agent simply stands. This mirrors the
// warn-on-unknown-key posture elsewhere in the loader; the [keys] table is the
// deliberate hard-error exception. Dropping the bad entry (rather than leaving
// it in place) guarantees the detector's resolver only ever sees valid
// overrides, so it never has to re-validate.
func sanitizeLimitPatterns(config *Config) {
	for agent, pattern := range config.LimitPatterns {
		if !isSupportedProgram(agent) {
			log.WarningLog.Printf("limit_patterns key %q is not one of [%s]; ignoring this override",
				agent, strings.Join(tmux.SupportedPrograms, ", "))
			delete(config.LimitPatterns, agent)
			continue
		}
		if _, err := regexp.Compile(pattern); err != nil {
			log.WarningLog.Printf("limit_patterns[%q]=%q is not a valid regexp (%v); using the built-in default",
				agent, pattern, err)
			delete(config.LimitPatterns, agent)
		}
	}
}

// isSupportedProgram reports whether name is one of the canonical agent
// programs (tmux.SupportedPrograms).
func isSupportedProgram(name string) bool {
	for _, supported := range tmux.SupportedPrograms {
		if name == supported {
			return true
		}
	}
	return false
}
