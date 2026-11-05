package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// AccountHandoffConfig is the [account_handoff] table — the configuration
// contract for the automatic account-handoff engine staged in #6776.
//
// NOTHING consumes these values yet: this is the parser/schema slice. The
// engine (evidence, shadow decisions, live moves) lands in later slices, so a
// resolved table is staged configuration only and an absent table means the
// same thing it always has — off.
//
// The mode leaf is the opt-in. Its compiled default is "off", NOT "shadow":
// the design's invariant is "absent table ≡ off ≡ today", and the resolver has
// no presence-dependent leaf defaults, so an absent table — and a present
// table that never writes mode — both resolve to off. Writing
// mode = "shadow" is the deliberate gesture that enables shadow once the
// engine exists. mode = "live" is a recognized spelling but rejected at
// validation until the live-mover slice ships: a config asking for behavior
// that does not exist yet must fail loudly rather than degrade silently.
//
// The table is global + personal-project only, never checked-in repo config —
// identity policy (whose accounts a machine may spend) is machine-local, the
// same boundary limit_account_candidates established in #3174. Personal
// leaves merge field-wise over the global table (MergeTableByField), so a
// project can pin just the leaves it means to override.
type AccountHandoffConfig struct {
	// Mode selects the engine state: "off" (default), "shadow", or — once its
	// slice ships — "live". The table only exists as staged configuration
	// until then.
	Mode string `json:"mode" toml:"mode"`
	// IncludePinned opts sessions pinned to an account (--account or a manual
	// handoff) INTO handoff governance. Default false: a pin holds the session
	// in place. With true the pin no longer holds — exclude_sessions is the
	// way to keep a named session out of automatic movement.
	IncludePinned bool `json:"include_pinned" toml:"include_pinned"`
	// TargetOrder picks candidates: "listed" (default — the
	// limit_account_candidates order, minimal diff from today) or "emptiest"
	// (sort by worst window percent then effective occupancy).
	TargetOrder string `json:"target_order" toml:"target_order"`
	// ConfirmWall requires fresh limit evidence before a move rather than
	// trusting a stale ambient reading. Default true.
	ConfirmWall bool `json:"confirm_wall" toml:"confirm_wall"`
	// WallPercent is the usage percent (any window) at which an account is
	// considered walled. Range 50–100, default 97.
	WallPercent int `json:"wall_percent" toml:"wall_percent"`
	// MoveIdleAtPercent is the ambient-reading percent at which an idle
	// session may move proactively. Range 50–100, default 95; 0 disables
	// proactive moves.
	MoveIdleAtPercent int `json:"move_idle_at_percent" toml:"move_idle_at_percent"`
	// PreferBelowPercent is an ordering preference, not a threshold: prefer
	// accounts whose worst window sits below this percent. Range 50–100,
	// default 85.
	PreferBelowPercent int `json:"prefer_below_percent" toml:"prefer_below_percent"`
	// ProbeTTL caches a cheap usage probe per account for this long. Default
	// "10m"; "" or a non-positive duration disables probes.
	ProbeTTL string `json:"probe_ttl" toml:"probe_ttl"`
	// DefaultLogin governs sessions running on the agent's ambient login (no
	// recorded account): "ignore" (default) or "move" (include them when the
	// engine is live).
	DefaultLogin string `json:"default_login" toml:"default_login"`
	// RequireCarry refuses a move when the conversation cannot be carried to
	// the new account, rather than falling back to a fresh start. Default
	// false keeps today's fresh-start fallback.
	RequireCarry bool `json:"require_carry" toml:"require_carry"`
	// ExcludeSessions names session titles the engine must never move — the
	// explicit opt-out that keeps working when include_pinned is true.
	ExcludeSessions []string `json:"exclude_sessions" toml:"exclude_sessions"`
	// MaxMoves caps automatic moves per session within MaxMovesWindow. Default
	// 3; must be at least 1 — 0 is rejected because "no moves" is mode = "off"
	// and many readers take 0 for unlimited.
	MaxMoves int `json:"max_moves" toml:"max_moves"`
	// MaxMovesWindow is the window MaxMoves counts within. Default "30m"; ""
	// or a non-positive duration means unlimited.
	MaxMovesWindow string `json:"max_moves_window" toml:"max_moves_window"`
	// RefusedBackoff is how long a session is left alone after it refused
	// every candidate. Default "2h"; "" or a non-positive duration disables
	// the backoff.
	RefusedBackoff string `json:"refused_backoff" toml:"refused_backoff"`
	// OnFailure decides engine behavior after a failed move: "halt" (default —
	// stop all automatic moves until cleared) or "skip" (skip that session,
	// keep governing the others).
	OnFailure string `json:"on_failure" toml:"on_failure"`
	// ReportTo names a session to prompt with handoff events, as
	// "<project>/<title>" or a unique bare title. Default "" logs only.
	ReportTo string `json:"report_to" toml:"report_to"`
}

const (
	AccountHandoffModeOff    = "off"
	AccountHandoffModeShadow = "shadow"
	// accountHandoffModeLive is recognized but rejected at validation until
	// the live-mover slice ships.
	accountHandoffModeLive = "live"

	AccountHandoffTargetOrderListed   = "listed"
	AccountHandoffTargetOrderEmptiest = "emptiest"

	AccountHandoffDefaultLoginIgnore = "ignore"
	AccountHandoffDefaultLoginMove   = "move"

	AccountHandoffOnFailureHalt = "halt"
	AccountHandoffOnFailureSkip = "skip"
)

const (
	accountHandoffPercentMin = 50
	accountHandoffPercentMax = 100
)

// defaultAccountHandoffConfig returns the compiled table — what an absent
// [account_handoff] resolves to, which is byte-for-byte today's behaviour:
// handoff off, every threshold at its conservative design value.
func defaultAccountHandoffConfig() AccountHandoffConfig {
	return AccountHandoffConfig{
		Mode:               AccountHandoffModeOff,
		IncludePinned:      false,
		TargetOrder:        AccountHandoffTargetOrderListed,
		ConfirmWall:        true,
		WallPercent:        97,
		MoveIdleAtPercent:  95,
		PreferBelowPercent: 85,
		ProbeTTL:           "10m",
		DefaultLogin:       AccountHandoffDefaultLoginIgnore,
		RequireCarry:       false,
		// A non-nil empty slice: the structural render marshals a nil slice as
		// JSON null, which rejectStructuredNulls (TOML has no null) would then
		// refuse to accept back — [] keeps the editor form round-trippable.
		ExcludeSessions: []string{},
		MaxMoves:        3,
		MaxMovesWindow:  "30m",
		RefusedBackoff:  "2h",
		OnFailure:       AccountHandoffOnFailureHalt,
		ReportTo:        "",
	}
}

// validateAccountHandoff checks a COMPLETE table (defaults already merged in,
// the shape parseConfigTOML produces) and returns a leaf-named error for the
// first invalid value. The checks are hard errors — like default_accounts and
// limit_account_candidates — because the table selects identity policy and a
// silent fallback could weaken an explicit safety opt-in.
func validateAccountHandoff(table AccountHandoffConfig) error {
	if err := validateAccountHandoffEnum("mode", table.Mode,
		AccountHandoffModeOff, AccountHandoffModeShadow); err != nil {
		return err
	}
	if err := validateAccountHandoffEnum("target_order", table.TargetOrder,
		AccountHandoffTargetOrderListed, AccountHandoffTargetOrderEmptiest); err != nil {
		return err
	}
	if err := validateAccountHandoffEnum("default_login", table.DefaultLogin,
		AccountHandoffDefaultLoginIgnore, AccountHandoffDefaultLoginMove); err != nil {
		return err
	}
	if err := validateAccountHandoffEnum("on_failure", table.OnFailure,
		AccountHandoffOnFailureHalt, AccountHandoffOnFailureSkip); err != nil {
		return err
	}
	if err := accountHandoffPercentValue("wall_percent", table.WallPercent, false); err != nil {
		return err
	}
	if err := accountHandoffPercentValue("move_idle_at_percent", table.MoveIdleAtPercent, true); err != nil {
		return err
	}
	if err := accountHandoffPercentValue("prefer_below_percent", table.PreferBelowPercent, false); err != nil {
		return err
	}
	// The ordering rule is conditioned on proactive moves being enabled:
	// prefer_below < move_idle < wall only binds when move_idle_at_percent is
	// nonzero — with moves disabled the hint has no referent.
	if table.MoveIdleAtPercent != 0 &&
		!(table.PreferBelowPercent < table.MoveIdleAtPercent && table.MoveIdleAtPercent < table.WallPercent) {
		return fmt.Errorf("account_handoff: prefer_below_percent (%d) must be < move_idle_at_percent (%d) must be < wall_percent (%d)",
			table.PreferBelowPercent, table.MoveIdleAtPercent, table.WallPercent)
	}
	if table.MaxMoves < 1 {
		return fmt.Errorf("account_handoff.max_moves must be at least 1, got %d — set mode = \"off\" to disable automatic handoffs", table.MaxMoves)
	}
	for _, leaf := range []struct {
		name  string
		value string
	}{
		{"probe_ttl", table.ProbeTTL},
		{"max_moves_window", table.MaxMovesWindow},
		{"refused_backoff", table.RefusedBackoff},
	} {
		if err := accountHandoffDurationValue(leaf.name, leaf.value); err != nil {
			return err
		}
	}
	for i, title := range table.ExcludeSessions {
		if strings.TrimSpace(title) == "" {
			return fmt.Errorf("account_handoff.exclude_sessions[%d] must not be empty", i)
		}
	}
	return nil
}

// validateAccountHandoffEnum checks one enum leaf. mode = "live" gets its own
// message: it is a known spelling reserved for the live-mover slice, and the
// refusal should say that rather than imply the enum is wrong.
func validateAccountHandoffEnum(leaf, value string, allowed ...string) error {
	if leaf == "mode" && value == accountHandoffModeLive {
		return fmt.Errorf("account_handoff.mode = %q is rejected for now — live moves are not implemented yet; use %q or %q",
			value, AccountHandoffModeOff, AccountHandoffModeShadow)
	}
	for _, candidate := range allowed {
		if value == candidate {
			return nil
		}
	}
	return fmt.Errorf("account_handoff.%s must be one of [%s], got %q", leaf, strings.Join(allowed, ", "), value)
}

// validateAccountHandoffLeaf validates one raw leaf spelling for the `af
// config set account_handoff.<leaf>` path — the same rules validateAccountHandoff
// applies to the decoded struct, expressed per leaf name.
func validateAccountHandoffLeaf(leaf, value string) error {
	switch leaf {
	case "mode":
		return validateAccountHandoffEnum("mode", value, AccountHandoffModeOff, AccountHandoffModeShadow)
	case "target_order":
		return validateAccountHandoffEnum("target_order", value, AccountHandoffTargetOrderListed, AccountHandoffTargetOrderEmptiest)
	case "default_login":
		return validateAccountHandoffEnum("default_login", value, AccountHandoffDefaultLoginIgnore, AccountHandoffDefaultLoginMove)
	case "on_failure":
		return validateAccountHandoffEnum("on_failure", value, AccountHandoffOnFailureHalt, AccountHandoffOnFailureSkip)
	case "wall_percent", "prefer_below_percent":
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("account_handoff.%s must be an integer, got %q", leaf, value)
		}
		return accountHandoffPercentValue(leaf, n, false)
	case "move_idle_at_percent":
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("account_handoff.%s must be an integer, got %q", leaf, value)
		}
		return accountHandoffPercentValue(leaf, n, true)
	case "max_moves":
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("account_handoff.max_moves must be an integer, got %q", value)
		}
		if n < 1 {
			return fmt.Errorf("account_handoff.max_moves must be at least 1, got %d — set mode = \"off\" to disable automatic handoffs", n)
		}
		return nil
	case "probe_ttl", "max_moves_window", "refused_backoff":
		return accountHandoffDurationValue(leaf, value)
	case "exclude_sessions":
		for i, entry := range splitListValue(value) {
			if strings.TrimSpace(entry) == "" {
				return fmt.Errorf("account_handoff.exclude_sessions[%d] must not be empty", i)
			}
		}
		return nil
	default:
		return fmt.Errorf("account_handoff.%s is not a settable leaf", leaf)
	}
}

func accountHandoffPercentValue(leaf string, value int, allowZero bool) error {
	if allowZero && value == 0 {
		return nil
	}
	if value < accountHandoffPercentMin || value > accountHandoffPercentMax {
		if allowZero {
			return fmt.Errorf("account_handoff.%s must be 0 (disabled) or between %d and %d, got %d",
				leaf, accountHandoffPercentMin, accountHandoffPercentMax, value)
		}
		return fmt.Errorf("account_handoff.%s must be between %d and %d, got %d",
			leaf, accountHandoffPercentMin, accountHandoffPercentMax, value)
	}
	return nil
}

// accountHandoffDurationValue applies the limit_retry_interval codec's rules
// to the table's three duration leaves: "" or a non-positive duration is the
// documented disabled/unlimited state; anything else must parse as a Go
// duration.
func accountHandoffDurationValue(leaf, value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	if _, err := time.ParseDuration(trimmed); err != nil {
		return fmt.Errorf("account_handoff.%s must be a Go duration like \"30m\" (or \"\" to disable), got %q: %w",
			leaf, value, err)
	}
	return nil
}
