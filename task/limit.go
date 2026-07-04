package task

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// This file is the usage-limit detection + reset-time parsing layer (#1146,
// PR1/4). It is the sibling of isReadyContent in runner.go: a pure per-agent
// matcher over captured tmux pane content, with no I/O and no daemon/TUI
// wiring. Later PRs consume it — PR2 wires isLimitContent into the daemon's
// single-writer status refresh and adds a LimitReached status; PR3 schedules
// an auto-resume off the parsed reset time; PR4 parks (not fails) a task run
// that hits a limit mid-flight. Keeping this layer pure and table-tested is
// what lets those PRs build on a trusted detector.
//
// Scope of what we schedule against: only the subscription-plan agents
// (claude, codex) stall at a dead prompt with a parseable reset window, so
// only they get a matcher here. gemini/aider are API-key-metered — a "limit"
// there is a transient 429 the CLI already retries, with no plan-reset
// timestamp to schedule against — so isLimitContent returns hit=false for
// them in v1. The matcher map is structured so a surface-only entry for them
// (detect set, parseReset nil) drops in later without touching the core.

// agentLimitMatcher is the per-agent recipe for recognizing a usage-limit
// banner and, when present, extracting its reset time.
type agentLimitMatcher struct {
	// detect matches the usage-limit banner anywhere in captured pane content.
	// A match means hit=true regardless of whether a reset time can be parsed
	// — detection must never depend on successful time parsing, so a reworded
	// or truncated reset phrase still surfaces as a limit hit.
	detect *regexp.Regexp
	// parseReset extracts and parses the reset timestamp from content that
	// detect has already matched, returning an absolute UTC reset time. It
	// returns ok=false when no reset time is present or it cannot be parsed;
	// detection still stands. now is the injected clock used to resolve
	// relative and time-of-day-only forms into an absolute instant — the
	// tested path never calls time.Now() itself, so tests are deterministic.
	// nil for a detect-only (surface-only) agent; there are none in v1.
	parseReset func(content string, now time.Time) (time.Time, bool)
}

var (
	// claudeLimitDetect matches Claude Code's stall banner on its invariant
	// lead sentence, e.g.
	//   Claude usage limit reached. Your limit will reset at 2pm (America/New_York)
	// Detection keys only on "Claude usage limit reached." so a reworded reset
	// clause still surfaces as a hit; parseClaudeReset separately scans for the
	// "reset at <tail>" phrase, symmetric with the codex detect/parse split.
	claudeLimitDetect = regexp.MustCompile(`Claude usage limit reached\.`)

	// codexLimitDetect matches Codex's stall banner. The reset phrase ("try
	// again at <ts>" / "try again in <duration>") is often on a later line, so
	// detection keys only on the invariant lead clause and reset extraction is
	// a separate scan (parseCodexReset) over the whole capture. The trailing
	// punctuation varies — "…usage limit." (weekly) vs "…usage limit, try
	// again in…" (relative, openai/codex#3031) — so it is intentionally not
	// anchored here.
	codexLimitDetect = regexp.MustCompile(`You've hit your usage limit`)
)

// builtinLimitMatchers returns a fresh map of the shipped per-agent matchers.
// A fresh map each call keeps resolveLimitMatchers free to swap detect regexes
// per config without mutating shared state.
func builtinLimitMatchers() map[string]agentLimitMatcher {
	return map[string]agentLimitMatcher{
		tmux.ProgramClaude: {detect: claudeLimitDetect, parseReset: parseClaudeReset},
		tmux.ProgramCodex:  {detect: codexLimitDetect, parseReset: parseCodexReset},
	}
}

// resolveLimitMatchers layers per-agent config overrides on top of the
// built-in matchers: for each entry in overrides (agent name -> regexp
// string), it replaces that agent's detect pattern while keeping the built-in
// reset-time parser. An override for an agent with no built-in matcher
// (gemini/aider in v1) is ignored — there is no reset parser to pair it with
// yet, and detection-only surfacing lands in a later PR. An uncompilable
// pattern is logged and skipped so the built-in default stands; validateConfig
// already drops such entries, so this is defense in depth for hand-built
// configs and non-config callers.
//
// PR1 has no live call site; the daemon (PR2) will call this once with
// cfg.LimitPatterns and reuse the result across poll ticks. Taking a plain
// map keeps the task package decoupled from the config package.
func resolveLimitMatchers(overrides map[string]string) map[string]agentLimitMatcher {
	matchers := builtinLimitMatchers()
	for agent, pattern := range overrides {
		if pattern == "" {
			continue
		}
		base, ok := matchers[agent]
		if !ok {
			// No built-in matcher (and thus no reset parser) for this agent
			// yet; a detection override alone would surface as an unparseable
			// hit with no scheduling value. Ignore until surface-only support
			// exists.
			log.WarningLog.Printf("limit_patterns override for %q ignored: no built-in usage-limit matcher for that agent yet", agent)
			continue
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			log.WarningLog.Printf("limit_patterns override for %q is not a valid regexp (%v); using built-in default", agent, err)
			continue
		}
		base.detect = re
		matchers[agent] = base
	}
	return matchers
}

// isLimitContent reports whether the captured pane content shows a usage-limit
// banner for the given agent and, when present, the absolute UTC time the
// limit resets. It is the usage-limit sibling of isReadyContent: callers
// resolve the canonical agent the pane actually runs
// (session.Instance.ResolvedAgent) and pass it here.
//
// Return contract:
//   - hit=false: no banner for this agent (or an agent with no matcher, e.g.
//     gemini/aider in v1). resetAt/hasResetTime are zero.
//   - hit=true, hasResetTime=true: banner detected and a reset time parsed;
//     resetAt is that time in UTC.
//   - hit=true, hasResetTime=false: banner detected but the reset time was
//     absent or unparseable. Detection never depends on parsing succeeding.
//
// matchers is the resolved set (built-ins with config overrides applied);
// now is the injected clock used to turn time-of-day-only and relative reset
// phrases into an absolute instant.
func isLimitContent(content, agent string, matchers map[string]agentLimitMatcher, now time.Time) (hit bool, resetAt time.Time, hasResetTime bool) {
	matcher, ok := matchers[agent]
	if !ok || matcher.detect == nil {
		return false, time.Time{}, false
	}
	if !matcher.detect.MatchString(content) {
		return false, time.Time{}, false
	}
	if matcher.parseReset == nil {
		return true, time.Time{}, false
	}
	if reset, parsed := matcher.parseReset(content, now); parsed {
		return true, reset.UTC(), true
	}
	return true, time.Time{}, false
}

// --- reset-time parsers ---------------------------------------------------

var (
	// clockTime matches a 12-hour clock token like "2pm", "2:30 PM", "11am".
	clockTime = regexp.MustCompile(`(?i)\b(\d{1,2})(?::(\d{2}))?\s*([ap])m\b`)
	// parenTZ captures an IANA timezone inside parentheses, e.g.
	// "(America/New_York)".
	parenTZ = regexp.MustCompile(`\(([A-Za-z]+(?:/[A-Za-z_+\-]+)+)\)`)
	// monthDay matches an optional-year calendar date like "Jul 25th, 2026",
	// "Nov 6", "November 6 2026". The ordinal suffix and year are optional.
	monthDay = regexp.MustCompile(`(?i)\b(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?\s+(\d{1,2})(?:st|nd|rd|th)?(?:,?\s+(\d{4}))?`)
	// weekday matches a leading weekday name for the claude weekly variant
	// when it names a day rather than a date.
	weekday = regexp.MustCompile(`(?i)\b(sun|mon|tue|wed|thu|fri|sat)[a-z]*\b`)

	monthIndex = map[string]time.Month{
		"jan": time.January, "feb": time.February, "mar": time.March,
		"apr": time.April, "may": time.May, "jun": time.June,
		"jul": time.July, "aug": time.August, "sep": time.September,
		"oct": time.October, "nov": time.November, "dec": time.December,
	}
	weekdayIndex = map[string]time.Weekday{
		"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday,
		"wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday,
		"sat": time.Saturday,
	}
)

// claudeResetAnchor locates Claude's reset clause and captures the tail after
// "reset " up to end of line, e.g. "2pm (America/New_York)" (5h) or "Monday at
// 9am (…)" / "Nov 6, 2026 9am (…)" (weekly). The "at" is optional because the
// weekday variant reads "reset Monday at 9am" — "reset <day>" — while the 5h
// variant reads "reset at 2pm". Kept separate from detection so a reworded
// reset clause still detects (hit=true) even when this yields no parse.
var claudeResetAnchor = regexp.MustCompile(`(?i)reset\s+(?:at\s+)?([^\r\n]+)`)

// parseClaudeReset scans content for Claude's "reset at <tail>" clause and
// resolves the tail, in order: an optional timezone in parens (falling back to
// now's location when absent — Claude renders in the account tz, which we
// cannot know without the paren, so the injected clock's zone is the
// deterministic stand-in), a required 12-hour clock time, and an optional
// calendar date or weekday (the weekly variant). Without any date it mints the
// next occurrence of that clock time strictly after now. Returns ok=false when
// there is no reset clause or no clock time in it, so an unrecognized/reworded
// tail degrades to hit=true, hasResetTime=false.
func parseClaudeReset(content string, now time.Time) (time.Time, bool) {
	anchor := claudeResetAnchor.FindStringSubmatch(content)
	if anchor == nil {
		return time.Time{}, false
	}
	reset := anchor[1]

	loc := now.Location()
	if m := parenTZ.FindStringSubmatch(reset); m != nil {
		if l, err := time.LoadLocation(m[1]); err == nil {
			loc = l
		}
	}

	hour, minute, ok := parseClockTime(reset)
	if !ok {
		return time.Time{}, false
	}

	// Explicit calendar date wins (weekly variant that carries a date).
	if m := monthDay.FindStringSubmatch(reset); m != nil {
		month := monthIndex[strings.ToLower(m[1][:3])]
		day, _ := strconv.Atoi(m[2])
		year := now.In(loc).Year()
		if m[3] != "" {
			year, _ = strconv.Atoi(m[3])
			return time.Date(year, month, day, hour, minute, 0, 0, loc).UTC(), true
		}
		// No year in the banner: pick the year that puts the date in the
		// future relative to now (this year, else next).
		candidate := time.Date(year, month, day, hour, minute, 0, 0, loc)
		if !candidate.After(now) {
			candidate = time.Date(year+1, month, day, hour, minute, 0, 0, loc)
		}
		return candidate.UTC(), true
	}

	// Weekday name (weekly variant that names a day): next such weekday at the
	// clock time, strictly after now.
	if m := weekday.FindStringSubmatch(reset); m != nil {
		target := weekdayIndex[strings.ToLower(m[1][:3])]
		return nextWeekday(now, loc, target, hour, minute).UTC(), true
	}

	// Time-only (5h window): next occurrence of the clock time after now.
	return nextTimeOfDay(now, loc, hour, minute).UTC(), true
}

var (
	// codexResetAt captures the absolute reset timestamp after "try again at",
	// ending at the AM/PM marker so trailing prose or punctuation is excluded.
	// Handles both "Jul 25th, 2026 5:55 PM" (weekly) and "6:34 AM" (5h).
	codexResetAt = regexp.MustCompile(`(?is)try again at\s+(.+?[ap]m)\b`)
	// codexResetIn captures a relative countdown, e.g.
	// "try again in 4 days 2 hours 46 minutes" — a real Codex variant
	// (openai/codex#3031). Parsed against the injected clock.
	codexResetIn = regexp.MustCompile(`(?is)try again in\s+([0-9a-z\s]+?)\.`)
	// dayHourMin pulls day/hour/minute counts out of a relative countdown.
	relDays  = regexp.MustCompile(`(?i)(\d+)\s*day`)
	relHours = regexp.MustCompile(`(?i)(\d+)\s*hour`)
	relMins  = regexp.MustCompile(`(?i)(\d+)\s*min`)
	// ordinalSuffix strips "st"/"nd"/"rd"/"th" off a day number so Go's
	// reference-layout parser can read the date.
	ordinalSuffix = regexp.MustCompile(`(?i)(\d{1,2})(st|nd|rd|th)`)
)

// parseCodexReset scans the whole captured pane content for Codex's reset
// phrase. It prefers an absolute "try again at <ts>" timestamp (weekly
// date+time or 5h time-only), then falls back to a relative "try again in
// <duration>" countdown. Codex renders in local time with no timezone in the
// banner, so absolute forms are interpreted in now's location. Returns
// ok=false when neither form is found or parses, so a reworded banner degrades
// to hit=true, hasResetTime=false.
func parseCodexReset(content string, now time.Time) (time.Time, bool) {
	if m := codexResetAt.FindStringSubmatch(content); m != nil {
		if t, ok := parseCodexAbsolute(strings.TrimSpace(m[1]), now); ok {
			return t, true
		}
	}
	if m := codexResetIn.FindStringSubmatch(content); m != nil {
		if d, ok := parseRelativeDuration(m[1]); ok {
			return now.Add(d), true
		}
	}
	return time.Time{}, false
}

// parseCodexAbsolute parses a Codex "try again at" timestamp in now's
// location, trying the weekly date+time layout first, then the 5h time-only
// form (whose next occurrence after now is minted).
func parseCodexAbsolute(ts string, now time.Time) (time.Time, bool) {
	loc := now.Location()
	cleaned := ordinalSuffix.ReplaceAllString(ts, "$1")

	// Weekly: "Jul 25, 2026 5:55 PM" (post-ordinal-strip).
	if t, err := time.ParseInLocation("Jan 2, 2006 3:04 PM", cleaned, loc); err == nil {
		return t, true
	}

	// 5h window: "6:34 AM" — a clock time with no date. Mint the next
	// occurrence after now.
	if hour, minute, ok := parseClockTime(cleaned); ok {
		return nextTimeOfDay(now, loc, hour, minute), true
	}
	return time.Time{}, false
}

// parseRelativeDuration sums the day/hour/minute components of a Codex
// countdown like "4 days 2 hours 46 minutes". Returns ok=false when no
// component is found.
func parseRelativeDuration(s string) (time.Duration, bool) {
	var d time.Duration
	found := false
	if m := relDays.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		d += time.Duration(n) * 24 * time.Hour
		found = true
	}
	if m := relHours.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		d += time.Duration(n) * time.Hour
		found = true
	}
	if m := relMins.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		d += time.Duration(n) * time.Minute
		found = true
	}
	return d, found
}

// parseClockTime extracts the first 12-hour clock token from s and returns its
// 24-hour (hour, minute). "12am" maps to 0, "12pm" to 12.
func parseClockTime(s string) (hour, minute int, ok bool) {
	m := clockTime.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, false
	}
	hour, _ = strconv.Atoi(m[1])
	if hour < 1 || hour > 12 {
		return 0, 0, false
	}
	if m[2] != "" {
		minute, _ = strconv.Atoi(m[2])
		if minute > 59 {
			return 0, 0, false
		}
	}
	if strings.EqualFold(m[3], "p") {
		if hour != 12 {
			hour += 12
		}
	} else if hour == 12 {
		hour = 0
	}
	return hour, minute, true
}

// nextTimeOfDay returns the next instant in loc whose clock reads hour:minute
// and that is strictly after now — today if it has not passed, else tomorrow.
func nextTimeOfDay(now time.Time, loc *time.Location, hour, minute int) time.Time {
	base := now.In(loc)
	candidate := time.Date(base.Year(), base.Month(), base.Day(), hour, minute, 0, 0, loc)
	if !candidate.After(now) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return candidate
}

// nextWeekday returns the next instant in loc that falls on target at
// hour:minute and is strictly after now. When today is the target weekday but
// the clock time has already passed, it rolls a full week forward.
func nextWeekday(now time.Time, loc *time.Location, target time.Weekday, hour, minute int) time.Time {
	base := now.In(loc)
	daysAhead := (int(target) - int(base.Weekday()) + 7) % 7
	candidate := time.Date(base.Year(), base.Month(), base.Day(), hour, minute, 0, 0, loc).AddDate(0, 0, daysAhead)
	if !candidate.After(now) {
		candidate = candidate.AddDate(0, 0, 7)
	}
	return candidate
}
