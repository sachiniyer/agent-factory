package task

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	xansi "github.com/charmbracelet/x/ansi"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// This file is the usage-limit detection + reset-time parsing layer (#1146).
// It is the sibling of isReadyContent in runner.go: a pure per-agent matcher
// over captured tmux pane content, with no I/O and no daemon/TUI wiring. Its
// consumers are live — the daemon's single-writer status refresh flips
// LiveLimitReached off it, daemon/limitresume.go schedules the auto-resume off
// the parsed reset time, and daemon/taskrun.go parks (not fails) a task run
// that hits a limit mid-flight. Keeping this layer pure and table-tested is
// what those consumers build on.
//
// Scope of what we schedule against: the subscription-plan agents (claude,
// codex) stall at a dead prompt with a parseable reset window, so they get a
// FULL matcher (detect + parseReset). devin gets a DETECT-ONLY matcher (#2411):
// its exhaustion banner is recognizable but its reset format is uncharacterized,
// so it surfaces the stall (badge + manual resume) with no reset time — the
// surface-only shape (detect set, parseReset nil) this map was built to allow.
// The remaining agents (gemini, aider, amp, opencode) have no matcher at all —
// API-key-metered or no known plan-reset banner, a "limit" there being a
// transient CLI/API failure with nothing to schedule against — so isLimitContent
// returns hit=false for them.

// agentLimitMatcher is the per-agent recipe for recognizing a usage-limit
// banner and, when present, extracting its reset time.
type agentLimitMatcher struct {
	// detect matches the usage-limit banner anywhere in captured pane content.
	// A match means hit=true regardless of whether a reset time can be parsed
	// — detection must never depend on successful time parsing, so a reworded
	// or truncated reset phrase still surfaces as a limit hit.
	detect *regexp.Regexp
	// lineAnchored requires the banner to OPEN its line: only whitespace and the
	// agent's own result/error marker glyphs may precede it (bannerLeadMarkers). It
	// is the positive evidence that the provider rendered the text, as opposed to
	// the words appearing in something the agent or user wrote (#4721). Built-in
	// matchers set it; a config limit_patterns override does not, because that is
	// the user's own regex and may legitimately match mid-line.
	lineAnchored bool
	// lead, when set, replaces the default bannerLeadMarkers allow-list as the
	// description of what may precede an anchored banner on its row. It is
	// matched against the ANSI-stripped text from the row start to the banner.
	// A matcher whose live rendering was never captured declares the prefixes
	// its evidence admits here instead of dropping the anchor (#6246).
	lead *regexp.Regexp
	// parseReset extracts and parses the reset timestamp from content starting
	// at detect's match, returning an absolute UTC reset time and the reset
	// phrase it consumed. The phrase is the parse's durable identity: a poll
	// that re-reads an unchanged banner re-derives the same phrase, so the
	// session can tell "same evidence re-read" from "new reset observation"
	// and keep the first (nearest-to-the-wall) instant (#6765). It returns
	// ok=false — and no evidence — when no reset time is present or it cannot
	// be parsed; detection still stands. now is the injected clock used to
	// resolve relative and time-of-day-only forms into an absolute instant —
	// the tested path never calls time.Now() itself, so tests are
	// deterministic. nil for a detect-only (surface-only) agent — devin
	// (#2411), whose exhaustion banner is recognizable but whose reset format
	// is uncharacterized.
	parseReset func(content string, now time.Time) (resetAt time.Time, evidence string, ok bool)
}

var (
	// claudeLimitDetect matches Claude Code's stall banner on its invariant
	// lead sentence, e.g.
	//   Claude usage limit reached. Your limit will reset at 2pm (America/New_York)
	// Detection keys only on "Claude usage limit reached." so a reworded reset
	// clause still surfaces as a hit; parseClaudeReset separately scans for the
	// "reset at <tail>" phrase, symmetric with the codex detect/parse split.
	claudeLimitDetect = regexp.MustCompile(`Claude usage limit reached\.`)

	// codexLimitDetect matches Codex's stall banner. Keep the full leading
	// sentence: captured panes can contain ordinary prose using "hit your usage
	// limit", and treating that fragment alone as a banner would park a healthy
	// session. Codex has rendered the contraction's apostrophe as both ASCII '
	// and typographic U+2019, so only that rune is flexible. The reset phrase
	// ("try again at <ts>" / "try again in <duration>") is often on a later line,
	// so reset extraction is a separate scan (parseCodexReset) over the whole
	// capture. The trailing punctuation varies — "…usage limit." (weekly) vs
	// "…usage limit, try again in…" (relative, openai/codex#3031) — so it is
	// intentionally not anchored here. Healthy prose containing the shorter
	// fragment is pinned as a non-match in the tests.
	codexLimitDetect = regexp.MustCompile(`You['’]ve hit your usage limit`)

	// devinLimitDetect matches devin's usage-quota exhaustion banner (#2411).
	//
	// INFERRED FROM THE BINARY, NOT OBSERVED LIVE. #2410 gave devin no matcher
	// because its exhausted-state pane was never captured (the account was not
	// driven to exhaustion). This anchors on the INVARIANT half of that banner,
	// verified against the devin 3000.2.17 binary: devin renders the exhaustion as
	// "Quota exhausted: <message>" (an error-Display prefix) and also as a bare
	// "Quota exhausted" heading in the rendered exhaustion UI (beside "Upgrade your
	// plan or wait for your quota to reset"). The <message> is BACKEND-SUPPLIED and
	// only falls back to the literal "usage quota has been exhausted" when the API
	// response carries none — so anchoring on the message half (as this first did)
	// would MISS every backend-worded exhaustion. "quota exhausted" is the prefix
	// present in BOTH renderings and BOTH message cases, matched case-insensitively
	// for display-casing resilience.
	//
	// It must NOT fire on devin's HEALTHY quota-status displays, which reuse the
	// "quota" vocabulary — "59% remaining", "(resets in 3d)", "Quota used:",
	// "Quota resets" — none of which contain "quota exhausted". Those non-matches
	// are pinned in the tests.
	//
	// Detect-only, parseReset nil: the exhausted-state reset format is likewise
	// uncharacterized, so this surfaces the stall (badge + manual resume) without
	// claiming a reset time it cannot parse — the same posture claude/codex fall
	// back to when their reset clause is unparseable. Auto-resume is deliberately
	// NOT wired here; it needs a trustworthy reset time, which this does not have.
	// When a real exhausted-state banner is captured, add a parseReset.
	devinLimitDetect = regexp.MustCompile(`(?i)quota exhausted`)

	// devinLeadPrefix is what may open a row before devin's banner. INFERRED
	// FROM THE BINARY, NOT OBSERVED LIVE: devin's alert is drawn with a warning
	// glyph, U+26A0 followed by the text-presentation selector U+FE0E, or the
	// ASCII fallback "[!]". The glyph and its selector are both optional so a
	// bare line (or a build that drops the glyph) still matches. No box-draw
	// border is admitted: nothing in the evidence implies a bordered render.
	// Anything else before the words (a bullet, a quote mark, a "└" branch)
	// still means the text sits inside something else (#4721).
	devinLeadPrefix = regexp.MustCompile(`^[ \t\r]*(?:\x{26A0}\x{FE0E}?|\[!\])?[ \t\r]*$`)
)

// builtinLimitMatchers returns a fresh map of the shipped per-agent matchers.
// A fresh map each call keeps resolveLimitMatchers free to swap detect regexes
// per config without mutating shared state.
func builtinLimitMatchers() map[string]agentLimitMatcher {
	return map[string]agentLimitMatcher{
		tmux.ProgramClaude: {detect: claudeLimitDetect, parseReset: parseClaudeReset, lineAnchored: true},
		tmux.ProgramCodex:  {detect: codexLimitDetect, parseReset: parseCodexReset, lineAnchored: true},
		// devin is DETECT-ONLY (#2411): its exhaustion banner is inferred from the
		// binary (not captured live) and its reset format is uncharacterized — so no
		// parseReset and no auto-resume. See devinLimitDetect.
		tmux.ProgramDevin: {detect: devinLimitDetect, parseReset: nil, lineAnchored: true, lead: devinLeadPrefix},
	}
}

// resolveLimitMatchers layers per-agent config overrides on top of the
// built-in matchers: for each entry in overrides (agent name -> regexp
// string), it replaces that agent's detect pattern while keeping the built-in
// reset-time parser (which stays nil for a detect-only built-in like devin, so
// an override there swaps detection without inventing a reset time). An override
// for an agent with no built-in matcher (gemini/aider/amp/opencode) is ignored:
// af has not characterized a usage-limit posture for it, so there is nothing to
// layer the override onto. An uncompilable pattern is logged and skipped so the
// built-in default stands; validateConfig already drops such entries, so this is
// defense in depth for hand-built configs and non-config callers.
//
// The daemon reaches this through NewLimitDetector (daemon/manager.go), built
// once with cfg.LimitPatterns, reused across poll ticks, and rebuilt on config
// apply (daemon/config_apply.go). Taking a plain map keeps the task package
// decoupled from the config package.
func resolveLimitMatchers(overrides map[string]string) map[string]agentLimitMatcher {
	matchers := builtinLimitMatchers()
	for agent, pattern := range overrides {
		if pattern == "" {
			continue
		}
		base, ok := matchers[agent]
		if !ok {
			// No built-in matcher for this agent to layer the override onto. A
			// detect-only built-in like devin's CAN be overridden — it has an
			// entry; agents with no entry at all (gemini/aider/amp/opencode) are
			// not surfaced from a bare, unvetted user regex. Ignore it.
			log.WarningLog.Printf("limit_patterns override for %q ignored: no built-in usage-limit matcher for that agent", agent)
			continue
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			log.WarningLog.Printf("limit_patterns override for %q is not a valid regexp (%v); using built-in default", agent, err)
			continue
		}
		base.detect = re
		base.lineAnchored = false // the user's own regex decides where it may match
		matchers[agent] = base
	}
	return matchers
}

// LimitDetector is the exported entry point the daemon uses to check captured
// pane content for a usage-limit banner (#1146 PR2). It wraps the resolved
// per-agent matcher set (built-ins with config overrides applied) so the daemon
// never touches the internal matcher map type. Build it ONCE from
// cfg.LimitPatterns (NewLimitDetector compiles the override regexes) and reuse
// the value across poll ticks — it is immutable and safe for concurrent reads.
type LimitDetector struct {
	matchers map[string]agentLimitMatcher
}

// LimitSighting is everything one captured pane says about a usage-limit wall:
// Check's four claims plus the anchor that names WHICH banner produced them.
// The anchor is what a parked session compares against its recorded sighting to
// tell "the same banner, reset clause clipped out of the capture" from "a new
// banner printed after it" — the (resetAt, evidence) pair alone cannot make
// that distinction: both shapes degrade to (zero, "") identically (#6823).
type LimitSighting struct {
	// Hit reports whether a usage-limit banner matched at all.
	Hit bool
	// ResetAt is the parsed absolute UTC reset, zero when none parsed.
	ResetAt time.Time
	// HasResetTime reports whether ResetAt came from a parsed reset phrase.
	HasResetTime bool
	// Evidence is the reset phrase the parse consumed — identical whenever an
	// unchanged banner is re-read (#6765).
	Evidence string
	// BannerPrefix is the capture's bytes before the LAST banner match — the
	// match's absolute position inside this capture. Comparing it to the
	// prefix a parked session recorded is what proves the last match sits at
	// the same place (a clipped re-read) or moved (a later banner).
	BannerPrefix string
	// BannerCount is the number of banner matches in the capture. A capture can
	// legitimately hold several (a retry trips the limit again before the
	// first banner scrolls off); more than were recorded means a newer banner.
	BannerCount int
}

// NewLimitDetector builds a detector from per-agent detect-pattern overrides
// (config.LimitPatterns; nil/empty yields the built-in matchers). An override
// for an agent with no built-in matcher, or an uncompilable pattern, is logged
// and dropped, so the built-in default always stands (see resolveLimitMatchers).
func NewLimitDetector(overrides map[string]string) LimitDetector {
	return LimitDetector{matchers: resolveLimitMatchers(overrides)}
}

// Check reports whether content shows a usage-limit banner for agent and, when
// present, the absolute UTC reset time — the return contract of isLimitContent.
// now is the injected clock used to resolve time-of-day-only and relative reset
// phrases into an absolute instant; the daemon passes its injected clock, tests
// a fixed one. An agent with no matcher (gemini/aider/amp/opencode) returns
// hit=false.
func (d LimitDetector) Check(content, agent string, now time.Time) (hit bool, resetAt time.Time, hasResetTime bool, evidence string) {
	return isLimitContent(content, agent, d.matchers, now)
}

// Sight is Check's full form (#6823): the same detection plus the banner anchor
// (prefix and count) a parked session needs to tell a clipped re-read of its
// recorded banner from a genuinely new wall printed after it. The daemon poll
// uses this; callers that only need detection keep Check.
func (d LimitDetector) Sight(content, agent string, now time.Time) LimitSighting {
	return isLimitSighting(content, agent, d.matchers, now)
}

// isLimitContent reports whether the captured pane content shows a usage-limit
// banner for the given agent and, when present, the absolute UTC time the
// limit resets. It is the usage-limit sibling of isReadyContent: callers
// resolve the canonical agent the pane actually runs
// (session.Instance.ResolvedAgent) and pass it here.
//
// Return contract:
//   - hit=false: no banner for this agent (or an agent with no matcher, e.g.
//     gemini/aider/amp/opencode). resetAt/hasResetTime/evidence are zero.
//   - hit=true, hasResetTime=true: banner detected and a reset time parsed;
//     resetAt is that time in UTC and evidence is the reset phrase the parse
//     consumed — re-reading an unchanged banner reproduces it verbatim, so the
//     caller can recognize a re-read rather than a new observation (#6765).
//   - hit=true, hasResetTime=false: banner detected but the reset time was
//     absent or unparseable; evidence is empty. Detection never depends on
//     parsing succeeding.
//
// matchers is the resolved set (built-ins with config overrides applied);
// now is the injected clock used to turn time-of-day-only and relative reset
// phrases into an absolute instant.
func isLimitContent(content, agent string, matchers map[string]agentLimitMatcher, now time.Time) (hit bool, resetAt time.Time, hasResetTime bool, evidence string) {
	s := isLimitSighting(content, agent, matchers, now)
	return s.Hit, s.ResetAt, s.HasResetTime, s.Evidence
}

// isLimitSighting is the detection core behind Check and Sight. On a miss it
// returns the zero sighting; on a hit it fills the banner anchor — the bytes
// before the last match and the match count — so the caller can compare WHICH
// banner produced the claim, not just what it claimed (#6823).
func isLimitSighting(content, agent string, matchers map[string]agentLimitMatcher, now time.Time) LimitSighting {
	matcher, ok := matchers[agent]
	if !ok || matcher.detect == nil {
		return LimitSighting{}
	}
	// The captured pane can legitimately contain multiple genuine usage-limit
	// banners (e.g. the user hits the retry key and trips the limit again before
	// the first banner scrolls off). Anchor reset parsing to the LAST match so
	// resetAt reflects the most recent banner, not a stale earlier one (#1602).
	matches := matcher.detect.FindAllStringIndex(content, -1)
	if matcher.lineAnchored {
		matches = anchoredMatches(content, matches, matcher.lead)
	}
	if len(matches) == 0 {
		return LimitSighting{}
	}
	last := matches[len(matches)-1]
	s := LimitSighting{Hit: true, BannerPrefix: content[:last[0]], BannerCount: len(matches)}
	if matcher.parseReset == nil {
		return s
	}
	if reset, phrase, parsed := matcher.parseReset(content[last[0]:], now); parsed {
		s.ResetAt, s.HasResetTime, s.Evidence = reset.UTC(), true, phrase
	}
	return s
}

// bannerLeadMarkers are the glyphs an agent draws in front of its own error or
// result line: Codex's error cell ("■ You’ve hit your usage limit…", captured
// verbatim in #4718) and Claude Code's result gutter ("⎿  Claude usage limit
// reached…"). Anything else before the text means it is inside something else —
// a bullet or paragraph of the agent's reply, a quotation, a diff line, a numbered
// listing, a table cell — and is not the provider speaking.
const bannerLeadMarkers = "■⎿⚠✗✘✖×"

// anchoredMatches keeps the matches that open their screen row, once ANSI styling
// is stripped from what precedes them (#4721). The detectors used to search the
// whole capture, so a healthy session that merely DISPLAYED the banner's words —
// an agent quoting it, a fixture or a diff that contains it, a summary of work on
// this very feature — and then went idle was parked as limit-blocked, and with
// failover on would have been moved to another account. A capture where the text
// appears but never opens a row is therefore unknown, not a limit: the caller
// treats it as an ordinary idle pane.
//
// The residual: a tool result that prints a file whose own line begins with the
// banner still opens a row. That cannot be told from the real thing by position
// alone, and the cost of the stricter alternative (requiring the provider's colour
// or theme) is the silent miss of #4718, so it is left to a future signal.
func anchoredMatches(content string, matches [][]int, lead *regexp.Regexp) [][]int {
	kept := matches[:0:0]
	for _, m := range matches {
		lineStart := strings.LastIndexByte(content[:m[0]], '\n') + 1
		prefix := xansi.Strip(content[lineStart:m[0]])
		if lead != nil {
			if lead.MatchString(prefix) {
				kept = append(kept, m)
			}
		} else if strings.Trim(prefix, " \t\r"+bannerLeadMarkers) == "" {
			kept = append(kept, m)
		}
	}
	return kept
}

// --- reset-time parsers ---------------------------------------------------

var (
	// clockTime matches a 12-hour clock token like "2pm", "2:30 PM", "11am".
	clockTime = regexp.MustCompile(`(?i)\b(\d{1,2})(?::(\d{2}))?\s*([ap])m\b`)
	// parenTZ captures a timezone candidate, including single-component names
	// and numeric offsets. LoadLocation validates it; capturing non-zones too
	// makes the fallback explicit instead of silently ignoring unknown text.
	parenTZ = regexp.MustCompile(`\(([^()\r\n]+)\)`)
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
// "reset " up to and including the clock time, an optional parenthesized
// timezone, and an optional trailing period, e.g. "2pm (America/New_York)"
// (5h) or "Monday at 9am (…)" / "Nov 6, 2026 9am (…)" (weekly). The "at" is
// optional because the weekday variant reads "reset Monday at 9am" — "reset
// <day>" — while the 5h variant reads "reset at 2pm". Kept separate from
// detection so a reworded reset clause still detects (hit=true) even when this
// yields no parse.
//
// The (?is) flags cross newlines in the tail capture, mirroring the codex
// reset regexes (codexResetAt/codexResetIn): Claude Code word-wraps its banner
// inside its TUI render, and a wrap that pushes the parenthesized timezone
// onto the next row must not truncate it out of the capture — without the
// paren the clock time silently resolves in the daemon host's zone instead of
// the account's. The tail is bounded by the clock token (\d{1,2}(:\d{2})?[ap]m)
// plus an optional "(tz)" rather than by the first period or end of pane: a
// period would mis-bound a date that abbreviates its month ("Nov. 6, 2026"),
// and end-of-pane would swallow trailing transcript rows after a clause that
// carries no trailing period, destabilizing the #6765 re-read evidence. The
// optional "(...)" spans newlines so a wrap inside the timezone token
// ("(America/\nNew_York)") stays within the capture.
//
// The pre-clock span (the "*?" before the clock token) crosses a wrap newline
// (mid-token) but not a clause boundary: a period immediately followed by a
// newline ends the reset clause, so a later pane row's clock — e.g.
// "reset at some point soon.\nuser draft: meet Monday at 2pm" — is NOT treated
// as the reset time. A period followed by a non-newline (the abbreviated month
// "Nov. 6") still extends the span, preserving wrapped dates. Without this
// bound the dot-all ".+?" would cross the completed clause and the later
// unrelated clock would yield timed evidence instead of the documented
// hit=true, hasResetTime=false result.
//
// Two refinements cover the real capture shape (tmux `capture-pane -e` keeps
// ANSI SGR escapes; the existing fixture shows "York).<ESC>[39m\n" at a styled
// line ending). First, a period immediately followed by an ANSI escape byte
// is treated as a styled line ending — i.e. a clause boundary — so an
// unparseable styled clause "...soon.<ESC>[39m\n" does NOT cross into a later
// row's clock: the inline-period branch excludes \x1b, so the span stops at
// the period and the parse degrades to hit=true, hasResetTime=false. Second,
// a period followed by a newline DOES end the clause EXCEPT when the period
// belongs to an abbreviated month ("Nov.\n6, 2026 9am"): the explicit
// month-abbreviation branch consumes "Nov.\n" (plus any trailing ANSI SGR
// escapes and an optional CR before the LF) AND requires the next row to begin
// with a digit (the day of a date continuation) as a unit, so the wrapped
// date continues to the clock, mirroring the unwrapped "Nov. 6" form. The
// branch admits the same longer spellings monthDay accepts (e.g. "Sept.\n6"),
// so a four-letter abbreviation that wraps after its period still continues.
// The digit guard stops an unparseable clause that merely ends with an
// abbreviated month — "reset sometime in Nov.\nuser draft: meet at 2pm" —
// from crossing into the transcript's clock: without it the month branch
// would consume "Nov.\n" and the span would adopt the unrelated "2pm". A
// period followed by a CR (the first half of a CRLF line ending) is treated
// the same as a period followed by a lone LF: the inline-period branch
// excludes \r as well as \n, so "reset at some point soon.\r\nuser draft:
// meet at 2pm" stops at the period and does not cross into the next row's
// clock. A plain period-plus-newline that is not a month abbreviation (or
// whose next row is not a date continuation) still terminates.
//
// The optional parenthesized timezone after the clock is bounded by row: a
// paren on the SAME row as the clock (or wrapping INSIDE the paren, e.g.
// "(America/\nNew_York)") is matched with only spaces/tabs before it, so the
// wrap-inside-paren case stays in the capture. A paren that opens on a LATER
// row is matched only as a wrapped continuation that ends in a period —
// "2pm\n(America/New_York).", the shipped wrapped-banner shape — so a
// no-period banner whose next row merely begins with a parenthesized
// location ("2pm\n(America/Los_Angeles)", where the paren is unrelated
// transcript text and the banner genuinely carries no zone) does NOT adopt
// that unrelated location: the clock still resolves in the injected clock's
// zone (the documented no-paren fallback) rather than the transcript's.
//
// The clock token itself accepts a wrap between the [ap] and m of its
// meridiem suffix — "2p\nm" — so a narrow pane that splits the suffix still
// captures the clock; normalizeResetWhitespace rejoins it to "2pm". The wrap
// also tolerates the continuation indent a real tmux wrap carries on the
// next row (the "  ⎿  " gutter pushes a 5-space indent,
// task/testdata/claude_working_tool.ansi:13) — "2p\n     m" — so an indented
// continuation row still captures the clock; normalizeResetWhitespace strips
// the indent and rejoins it to "2pm" the same way. An
// optional trailing period that wraps onto its own row — "2pm
// (America/New_York)\n." — is captured too (the only \n. the anchor can
// reach is this trailing period; a period-newline in the pre-clock span is a
// clause boundary the anchor stops at), so the evidence matches the
// unwrapped "(…)." render and the #6765 re-read identity holds across pane
// resizes. A next-row parenthesized timezone may carry its trailing period
// on either the paren's row ("(…)." ) or its own row ("(…)\n.") — both are
// the shipped wrapped-banner shape, so either is captured; a next-row paren
// with NO trailing period is treated as unrelated transcript text and is
// NOT captured (the clock resolves in the injected clock's zone).
//
// The leading "reset" keyword itself accepts a wrap between any of its
// letters — "re\nset" — so a narrow pane that hard-wraps inside the keyword
// still locates the clause; normalizeResetWhitespace rejoins it to "reset"
// (gated on the joined letters forming "reset") so the evidence matches the
// unwrapped render.
var claudeResetAnchor = regexp.MustCompile(`(?is)r(?:\r?\n[ \t]*)?e(?:\r?\n[ \t]*)?s(?:\r?\n[ \t]*)?e(?:\r?\n[ \t]*)?t\s+(?:at\s+)?((?:(?:jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.(?:\x1b\[[0-9;]*m)*\r?\n[ \t]*\d|[^.]|\.(?:[^\r\n\x1b]))*?\d{1,2}(?::\d{2})?\s*[ap](?:\r?\n[ \t]*)?m\b(?:[ \t]*\([^()]*\))*[ \t]*\.?(?:\r?\n[ \t]*\([^()]*\)(?:\.|\r?\n[ \t]*\.))?(?:\r?\n[ \t]*\.)?)`)

// normalizeResetWhitespace collapses the whitespace a wrapped Claude banner
// introduces, so the paren-timezone and clock scans in parseClaudeReset see a
// single-line form. Outside a parenthesized timezone, runs of whitespace
// (including the wrap newline between the clock time and the tz paren) collapse
// to a single space. Inside a parenthesized span only the wrap's newlines are
// dropped (an IANA zone name has no internal whitespace, so a wrap that splits
// the token — "(America/\nNew_York)" — must rejoin to "(America/New_York)",
// not "(America/ New_York)", or time.LoadLocation rejects it and the clock
// silently falls back to the host zone); genuine spaces inside a paren, e.g.
// the descriptor "(5h limit)", are kept verbatim. A wrap that splits a
// multi-digit token outside a paren — a calendar day ("Nov 1\n2, 2026" →
// "Nov 12, 2026"), a clock hour ("1\n1am" → "11am"), or a four-digit year
// ("Nov 6, 20\n27 9am" → "Nov 6, 2027 9am") — rejoins by dropping the
// newline rather than spacing it, so the day/hour/year resolves as the
// unwrapped form instead of the off-by-a-digit "Nov 1 2" / "1 1am" /
// "Nov 6, 20 27" parse. A wrap that splits a month-name token outside a
// paren ("No\nv" → "Nov") rejoins the same way, gated on the joined
// letters forming a month name (wrapSplitsMonth) so a word-boundary wrap
// ("reset\nat") takes a space rather than collapsing to "resetat" —
// keeping the unwrapped evidence ("reset at") stable for the #6765
// re-read identity. The same normalization is applied to the evidence so
// it stays stable for the #6765 re-read identity.
func normalizeResetWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	depth := 0
	outsideSpace := false  // whether the last char emitted outside a paren was a collapsed space
	lastDigit := false     // whether the last char emitted outside a paren was an ASCII digit
	lastLetter := false    // whether the last char emitted outside a paren was an ASCII letter
	lastColon := false     // whether the last char emitted outside a paren was a clock colon
	parenPostWrap := false // whether the last byte inside a paren was a dropped wrap newline (skip the continuation indent that follows)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '(':
			depth++
			b.WriteByte(c)
			outsideSpace = false
			lastDigit = false
			lastLetter = false
			lastColon = false
		case c == ')':
			if depth > 0 {
				depth--
			}
			b.WriteByte(c)
			outsideSpace = false
			lastDigit = false
			lastLetter = false
			lastColon = false
		case c == '\x1b':
			// tmux `capture-pane -e` keeps ANSI SGR escapes, and the anchor's
			// abbreviated-month branch admits them across a wrap, so a styled
			// date like "Nov.\x1b[39m\n6" reaches the normalizer with the SGR
			// bytes sitting between the month's period and the day. Strip the
			// escape sequence transparently — without resetting the token-state
			// flags or emitting a byte — so the wrap's rejoin/space decision
			// sees the surrounding tokens directly and monthDay matches
			// "Nov. 6" rather than "Nov.\x1b[39m 6" (which it cannot match, so
			// the reset degrades to time-only). Stripping applies inside a
			// paren too, so a styled zone "(America/\x1b[39m\nNew_York)"
			// still rejoins to "(America/New_York)".
			k := i + 1
			if k < len(s) && s[k] == '[' {
				k++
				for k < len(s) && s[k] >= 0x30 && s[k] <= 0x3F {
					k++
				}
				if k < len(s) && s[k] >= 0x40 && s[k] <= 0x7E {
					k++
				}
			}
			i = k - 1
			continue
		case depth > 0:
			// Inside a parenthesized span: drop the wrap's newlines so a split
			// zone token rejoins; keep every other byte (including the genuine
			// spaces of a non-zone descriptor like "5h limit") verbatim. A real
			// tmux wrap also indents the continuation row (the "  ⎿  " gutter
			// pushes a 5-space indent), so the leading spaces/tabs after a wrap
			// newline are the indent, not part of the zone name — strip them so
			// "(America/\n     New_York)" rejoins to "(America/New_York)" (which
			// time.LoadLocation accepts) rather than "(America/     New_York)"
			// (which it rejects, silently falling back to the host zone).
			if c == '\n' || c == '\r' {
				parenPostWrap = true
				continue
			}
			if parenPostWrap && (c == ' ' || c == '\t') {
				continue
			}
			parenPostWrap = false
			b.WriteByte(c)
			outsideSpace = false
			lastDigit = false
			lastLetter = false
			lastColon = false
		case c == ' ' || c == '\t':
			// Outside a paren: collapse runs of spaces/tabs to a single space.
			if !outsideSpace {
				b.WriteByte(' ')
				outsideSpace = true
			}
			lastDigit = false
			lastLetter = false
			lastColon = false
		case c == '\n' || c == '\r':
			// CRLF is a single wrap: the anchor accepts \r\n, so the captured
			// content may carry it. Process the pair as one wrap here — if the
			// \r and \n were handled in separate passes, the \r pass would emit
			// a space and clear the token-state flags (lastDigit/lastLetter/
			// lastColon) before the \n pass sees the continuation token, so a
			// token-internal CRLF wrap like "Nov 6, 20\r\n27 9am" normalized to
			// "20 27" (discarding the explicit year) instead of rejoining to
			// "2027". Skip the CR and let the LF iteration apply the rejoin
			// classifiers to the whole CRLF as a single wrap.
			if c == '\r' && i+1 < len(s) && s[i+1] == '\n' {
				continue
			}
			// A wrap newline outside a paren. Real tmux wraps sit under a
			// "  ⎿  " gutter and carry a 5-space indent on continuation rows
			// (task/testdata/claude_working_tool.ansi), so the byte after the
			// newline is the indent, not the continuation token. Skip the
			// indent before deciding whether to rejoin, then consume the
			// newline+indent so the rejoin/space decision sees the
			// continuation token directly. A wrap with no indent keeps the
			// prior behavior: j == i+1 and the checks are unchanged.
			j := i + 1
			for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
				j++
			}
			rejoin := false
			// A wrap that splits a multi-digit token (a calendar day, a clock
			// hour, or a four-digit year) drops the newline so the digits
			// rejoin — "Nov 1\n2" → "Nov 12", "1\n1am" → "11am",
			// "Nov 6, 20\n27" → "Nov 6, 2027" — otherwise the day/hour/year
			// resolves to the wrong instant (or monthDay misses the explicit
			// year and the evidence diverges from the unwrapped form). The
			// rejoin is gated on the joined digits forming a single
			// day/hour/year token (≤ 4 digits): a wrap BETWEEN separate
			// numeric tokens ("Nov 6\n2026" → 5 digits, "2026\n9am" → 5)
			// stays a space, or monthDay would consume "62" as the day and
			// time.Date would normalize it into a much later month. When the
			// post-run is immediately followed by a clock suffix ([ap]m), a
			// mid-token HOUR split ("1\n1am" → "11am") rejoins, but a wrap
			// BETWEEN a calendar day and the clock hour ("Nov 6\n9am") stays
			// a space — rejoining to "Nov 69am" makes parseClockTime reject
			// hour 69. A mid-token MINUTES split ("2:3\n0pm" → "2:30pm") is
			// preceded by a colon and rejoins regardless of the hour bound.
			if lastDigit && j < len(s) && s[j] >= '0' && s[j] <= '9' {
				pre := b.String()
				preRun := 0
				for preRun < len(pre) {
					ch := pre[len(pre)-1-preRun]
					if ch < '0' || ch > '9' {
						break
					}
					preRun++
				}
				postRun := 0
				for j+postRun < len(s) {
					ch := s[j+postRun]
					if ch < '0' || ch > '9' {
						break
					}
					postRun++
				}
				preStart := len(pre) - preRun
				prevIsColon := preStart > 0 && pre[preStart-1] == ':'
				postEnd := j + postRun
				followedByClockSuffix := postEnd+1 < len(s) &&
					(s[postEnd] == 'a' || s[postEnd] == 'A' || s[postEnd] == 'p' || s[postEnd] == 'P') &&
					(s[postEnd+1] == 'm' || s[postEnd+1] == 'M')
				if followedByClockSuffix {
					if prevIsColon {
						rejoin = true // mid-token minutes split — rejoin
					} else {
						joinedVal, _ := strconv.Atoi(pre[preStart:] + s[j:j+postRun])
						// A mid-token clock-hour split ("1\n1am" → "11am")
						// rejoins, but a wrap BETWEEN a calendar day and the
						// clock hour ("Nov 1\n2pm") must stay a space:
						// rejoining to "Nov 12pm" would let monthDay consume
						// "12" as the day and parseClockTime read "12pm" as
						// the clock. The day is always preceded by a month
						// name in the weekly-date grammar ("Nov 6"), so a
						// pre-run that is a calendar day
						// (preRunIsCalendarDay) marks a day+clock boundary
						// even when the joined digits fall in 1–12.
						if joinedVal >= 1 && joinedVal <= 12 && !preRunIsCalendarDay(pre, preStart) {
							rejoin = true // mid-token clock hour — rejoin
						}
						// else: day+clock boundary — stay a space
					}
				} else if preRunIsCalendarDay(pre, preStart) && postEnd < len(s) && s[postEnd] == ':' {
					// A wrap between a calendar day and a clock hour that
					// carries minutes ("Nov 1\n2:30pm") is a day+clock
					// boundary, not a mid-token day/year split: rejoining to
					// "Nov 12:30pm" would let monthDay consume "12" as the day
					// and the clock read "12:30pm", scheduling the reset on
					// the wrong day at the wrong hour. The colon prevents
					// followedByClockSuffix (it checks [ap]m), so this guard
					// applies the same calendar-day boundary check before the
					// generic digit rejoin below. A mid-token clock-hour
					// split ("1\n1:30am") is not a calendar day
					// (preRunIsCalendarDay is false), so it still rejoins.
				} else if preRun+postRun <= 4 {
					rejoin = true // mid-token day/year — rejoin
				}
			}
			if lastColon && j < len(s) && s[j] >= '0' && s[j] <= '9' {
				rejoin = true
			}
			if lastDigit && j < len(s) && s[j] == ':' {
				rejoin = true
			}
			if lastDigit && wrapSplitsClockDigitMeridiem(b.String(), s[j:]) {
				rejoin = true
			}
			// A wrap that splits a multi-letter token the normalizer must
			// rejoin — a month name ("No\nv" → "Nov"), a weekday
			// ("Th\nursday" → "Thursday"), the leading "reset" keyword
			// ("re\nset" → "reset"), the "at" preposition ("a\nt" → "at"),
			// the clock's [ap]m suffix ("2p\nm" → "2pm"), or an ordinal date
			// suffix ("12t\nh" → "12th") — drops the newline so the letters
			// rejoin; a word-boundary wrap between complete words
			// ("reset\nat") is byte-identical to a mid-token wrap at the
			// newline, so the distinction is semantic (the joined letters
			// forming the token) and anything else takes a space, preserving
			// the unwrapped evidence for the #6765 re-read identity.
			if lastLetter && j < len(s) && (s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z') && (wrapSplitsMonth(b.String(), s[j:]) || wrapSplitsClockSuffix(b.String(), s[j:]) || wrapSplitsWeekday(b.String(), s[j:]) || wrapSplitsResetKeyword(b.String(), s[j:]) || wrapSplitsOrdinalSuffix(b.String(), s[j:]) || wrapSplitsAtPreposition(b.String(), s[j:])) {
				rejoin = true
			}
			// A wrap between a day digit and its ordinal suffix
			// ("Nov 12\nth" → "Nov 12th") drops the newline so monthDay
			// keeps the explicit year rather than matching only "Nov 12" and
			// silently discarding it (scheduling the reset a year early).
			if lastDigit && j < len(s) && (s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z') && wrapSplitsDigitOrdinalSuffix(b.String(), s[j:]) {
				rejoin = true
			}
			// A wrap right before the date's comma ("Nov 6\n, 2027") drops
			// the newline rather than spacing to "Nov 6 , 2027": monthDay's
			// optional year requires the comma to immediately follow the day
			// (or its ordinal suffix), and the spaced form diverges the
			// evidence from the unwrapped "Nov 6, 2027" render (#6765). The
			// comma only appears in the pre-clock date span, so the drop is
			// safe. A wrap after a complete ordinal suffix ("Nov 12th\n,
			// 2027") rejoins for the same reason via preEndsOrdinalSuffix.
			if lastDigit && j < len(s) && s[j] == ',' {
				rejoin = true
			}
			if lastLetter && j < len(s) && s[j] == ',' && preEndsOrdinalSuffix(b.String()) {
				rejoin = true
			}
			// A wrap that pushes the trailing period onto its own row ("2pm
			// (America/New_York)\n.") drops the newline rather than spacing
			// to "(…) .": the period is sentence punctuation, not a new word,
			// and the only \n. the anchor captures is this trailing wrapped
			// period (a period-newline in the pre-clock span is a clause
			// boundary the anchor already stops at), so the drop is
			// unconditional.
			if j < len(s) && s[j] == '.' {
				rejoin = true
			}
			// Consume the newline and any continuation indent; the next loop
			// iteration processes s[j] (the continuation token).
			i = j - 1
			if rejoin {
				continue
			}
			// Otherwise collapse the wrap (and its indent) to a single space
			// (the wrap between the clock and the tz paren, or between
			// complete words).
			if !outsideSpace {
				b.WriteByte(' ')
				outsideSpace = true
			}
			lastDigit = false
			lastLetter = false
			lastColon = false
		default:
			b.WriteByte(c)
			outsideSpace = false
			lastDigit = c >= '0' && c <= '9'
			lastLetter = (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
			lastColon = c == ':'
		}
	}
	return b.String()
}

// parseClaudeReset scans the detector-matched tail for Claude's "reset at
// <tail>" clause and resolves the tail, in order: an optional timezone in parens
// (falling back to now's location when absent or invalid — Claude renders in the account
// tz, which we cannot know without the paren, so the injected clock's zone is
// the deterministic stand-in), a required 12-hour clock time, and an optional
// calendar date or weekday (the weekly variant). Without any date it mints the
// next occurrence of that clock time strictly after now. The returned evidence
// is the matched clause itself — "reset at 2pm (…)" — identical whenever the
// banner is re-read unchanged (#6765). Returns ok=false when there is no reset
// clause or no clock time in it, so an unrecognized/reworded tail degrades to
// hit=true, hasResetTime=false.
func parseClaudeReset(content string, now time.Time) (time.Time, string, bool) {
	anchor := claudeResetAnchor.FindStringSubmatch(content)
	if anchor == nil {
		return time.Time{}, "", false
	}
	// Collapse whitespace (including the newlines a wrapped banner introduces)
	// so the paren-timezone and clock scans see a single-line form. The evidence
	// is collapsed the same way, keeping it stable for the #6765 re-read
	// identity: the same banner always yields the same normalized phrase.
	// normalizeResetWhitespace (see above) rejoins a wrap inside the tz paren so
	// time.LoadLocation still accepts the zone.
	reset := strings.TrimSpace(normalizeResetWhitespace(anchor[1]))
	evidence := strings.TrimSpace(normalizeResetWhitespace(anchor[0]))

	loc := now.Location()
	candidates := parenTZ.FindAllStringSubmatch(reset, -1)
	var rejected []string
	for _, m := range candidates {
		if l, err := time.LoadLocation(m[1]); err == nil {
			loc = l
			break
		}
		rejected = append(rejected, m[1])
	}
	hour, minute, ok := parseClockTime(reset)
	if !ok {
		return time.Time{}, "", false
	}
	if len(rejected) > 0 && len(rejected) == len(candidates) {
		warnClaudeTimezoneOnce(content, rejected, loc)
	}

	// Scan the reset clause with the parenthesized timezone spans blanked out:
	// the paren tz is already consumed separately for loc above, so the only
	// thing left for month/weekday matching is tz noise. An IANA zone whose name
	// begins with a weekday or month abbreviation (America/Montreal, Europe/Monaco,
	// America/Thule, …) would otherwise satisfy the weekday regex and hijack the
	// weekly "reset <weekday>" branch, turning a same-day 5h reset into next
	// Monday/Thursday. Replace each paren span with equal-width spaces (not
	// excision) so match offsets and word boundaries are preserved verbatim.
	clause := parenTZ.ReplaceAllStringFunc(reset, func(match string) string {
		return strings.Repeat(" ", len(match))
	})

	// Explicit calendar date wins (weekly variant that carries a date).
	if m := monthDay.FindStringSubmatch(clause); m != nil {
		month := monthIndex[strings.ToLower(m[1][:3])]
		day, _ := strconv.Atoi(m[2])
		year := now.In(loc).Year()
		if m[3] != "" {
			year, _ = strconv.Atoi(m[3])
			return time.Date(year, month, day, hour, minute, 0, 0, loc).UTC(), evidence, true
		}
		// No year in the banner: pick the year that puts the date in the
		// future relative to now (this year, else next).
		candidate := time.Date(year, month, day, hour, minute, 0, 0, loc)
		if !candidate.After(now) {
			candidate = time.Date(year+1, month, day, hour, minute, 0, 0, loc)
		}
		return candidate.UTC(), evidence, true
	}

	// Weekday name (weekly variant that names a day): next such weekday at the
	// clock time, strictly after now. Run against the paren-stripped clause so a
	// weekday-prefixed tz name cannot satisfy this regex — see the clause note.
	if m := weekday.FindStringSubmatch(clause); m != nil {
		target := weekdayIndex[strings.ToLower(m[1][:3])]
		return nextWeekday(now, loc, target, hour, minute).UTC(), evidence, true
	}

	// Time-only (5h window): next occurrence of the clock time after now.
	return nextTimeOfDay(now, loc, hour, minute).UTC(), evidence, true
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

// parseCodexReset scans the detector-matched tail for Codex's reset phrase. It
// prefers an absolute "try again at <ts>" timestamp (weekly
// date+time or 5h time-only), then falls back to a relative "try again in
// <duration>" countdown. Codex renders in local time with no timezone in the
// banner, so absolute forms are interpreted in now's location. The returned
// evidence is the matched reset phrase itself — identical whenever the banner
// is re-read unchanged (#6765). Returns ok=false when neither form is found or
// parses, so a reworded banner degrades to hit=true, hasResetTime=false.
func parseCodexReset(content string, now time.Time) (time.Time, string, bool) {
	if m := codexResetAt.FindStringSubmatch(content); m != nil {
		if t, ok := parseCodexAbsolute(strings.TrimSpace(m[1]), now); ok {
			return t, strings.TrimSpace(m[0]), true
		}
	}
	if m := codexResetIn.FindStringSubmatch(content); m != nil {
		if d, ok := parseRelativeDuration(m[1]); ok {
			return now.Add(d), strings.TrimSpace(m[0]), true
		}
	}
	return time.Time{}, "", false
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
