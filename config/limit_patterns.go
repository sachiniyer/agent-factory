package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// defaultLimitRetryInterval is the fallback cadence for auto-resuming a
// usage-limit-blocked session whose banner carried no parseable reset time
// (#1146 PR3). Only consulted when limit_auto_resume is enabled, so it is
// harmless while the feature is off by default.
const defaultLimitRetryInterval = "30m"

// LimitRetryIntervalDuration returns the parsed limit_retry_interval (#1146
// PR3), or 0 when it is unset or disables the fallback. The value is validated
// at load (sanitizeLimitRetryInterval), so a parse error here degrades safely to
// 0 — surface-only for a no-parseable-reset-time limit.
func (c *Config) LimitRetryIntervalDuration() time.Duration {
	// Trim before parsing so a whitespace-padded hand-edit still yields the
	// intended cadence rather than degrading to 0 (which silently disables the
	// fixed-interval fallback). sanitizeLimitRetryInterval already stores a
	// trimmed value, so this is defense-in-depth for any value that reaches the
	// consumer without passing through the loader (#2009).
	v := strings.TrimSpace(c.LimitRetryInterval)
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0
	}
	return d
}

// sanitizeLimitRetryInterval validates the limit_retry_interval duration string
// (#1146 PR3). An empty string is the explicit "no fallback" value and is kept.
// A non-empty value must parse as a non-negative Go duration; anything else
// warns and falls back to the default so a typo can neither silently disable
// auto-resume nor mis-time it.
func sanitizeLimitRetryInterval(raw, prettyConfigPath string) string {
	// Trim first: a stray leading/trailing space is an unambiguous hand-edit
	// typo, and time.ParseDuration rejects " 30s ". Without this the value would
	// fail to parse and be replaced by the default with the user's intended
	// interval silently thrown away (#2009). The parsed value is stored trimmed
	// so the consumer sees a canonical duration string.
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	d, err := time.ParseDuration(trimmed)
	if err != nil {
		log.WarningLog.Printf("Config issue in %s: limit_retry_interval=%q is not a valid duration (%v); using default %q",
			prettyConfigPath, raw, err, defaultLimitRetryInterval)
		return defaultLimitRetryInterval
	}
	if d < 0 {
		log.WarningLog.Printf("Config issue in %s: limit_retry_interval=%q is negative; using default %q",
			prettyConfigPath, raw, defaultLimitRetryInterval)
		return defaultLimitRetryInterval
	}
	return trimmed
}

// validateLimitRetryIntervalValue applies sanitizeLimitRetryInterval's rules as
// a hard error, for `af config set limit_retry_interval`: empty is the
// explicit "never retry" value and is kept; anything else must parse as a
// non-negative Go duration.
//
// It hard-errors where the loader warns-and-defaults, matching how the other
// settable keys treat their loader rules (worktree_root normalizes on load but
// rejects on set): a typo the loader would quietly turn into 30m should be told
// to the user at the moment they typed it, rather than silently mis-timing
// auto-resume later.
//
// Whitespace is intentionally NOT trimmed here, unlike the loader (#2009): a
// hand-edited config.toml with a stray space is honored leniently on load, but
// a value passed explicitly to `af config set` is strict input — rejecting
// " 30s " with a message naming the value is honest and actionable, not a
// silent default, so it stays a hard error.
func validateLimitRetryIntervalValue(value string) error {
	if value == "" {
		return nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("limit_retry_interval must be a duration like %q or \"1h\", or \"\" to never retry, got %q: %w",
			defaultLimitRetryInterval, value, err)
	}
	if d < 0 {
		return fmt.Errorf("limit_retry_interval must not be negative, got %q", value)
	}
	return nil
}

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
				agent, tmux.SupportedProgramsString())
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
