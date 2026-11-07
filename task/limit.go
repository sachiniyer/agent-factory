package task

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
	// phrase it consumed. rowLead is the capture's text from the matched row's
	// start to the match — the gutter a wrapped banner's continuation rows
	// align under (#6944); a parser that does not rejoin rows ignores it. The
	// phrase is the parse's durable identity: a poll that re-reads an
	// unchanged banner re-derives the same phrase, so the session can tell
	// "same evidence re-read" from "new reset observation" and keep the first
	// (nearest-to-the-wall) instant (#6765). It returns ok=false — and no
	// evidence — when no reset time is present or it cannot be parsed;
	// detection still stands. now is the injected clock used to resolve
	// relative and time-of-day-only forms into an absolute instant — the
	// tested path never calls time.Now() itself, so tests are deterministic.
	// nil for a detect-only (surface-only) agent — devin (#2411), whose
	// exhaustion banner is recognizable but whose reset format is
	// uncharacterized.
	parseReset func(rowLead, content string, now time.Time) (resetAt time.Time, evidence string, ok bool)
}

var (
	// claudeLimitDetect matches Claude Code's stall banners on their invariant
	// lead sentences. Two eras co-exist: the classic
	//   Claude usage limit reached. Your limit will reset at 2pm (America/New_York)
	// and the current refusal message Claude records (and renders) per limit
	// name, "You've hit your 5-hour limit · resets 9:40am (…)", "You've hit
	// your weekly limit · …". The third alternative is the native wait UI's
	// status text ("Usage limit reached"), which carries no clause of its own
	// but still marks the wall — the transcript's refusal record supplies the
	// reset either way (#6944). Detection never depends on the reset clause
	// parsing, so a reworded or wrapped clause still surfaces as a hit;
	// parseClaudeReset separately scans for the reset phrase, symmetric with
	// the codex detect/parse split.
	// Word gaps admit at most ONE newline plus horizontal whitespace: a
	// continuation break can land inside the banner's own words on a very
	// narrow pane, but a paragraph gap — or any row that is not a
	// continuation — cannot masquerade as the banner's.
	claudeLimitDetect = regexp.MustCompile(`Claude[ \t\r]*\n?[ \t]*usage[ \t\r]*\n?[ \t]*limit[ \t\r]*\n?[ \t]*reached\.|You['’]ve[ \t\r]*\n?[ \t]*hit[ \t\r]*\n?[ \t]*your[ \t\r]*\n?[ \t]*[\w-]+[ \t\r]*\n?[ \t]*limit\b|Usage[ \t\r]*\n?[ \t]*limit[ \t\r]*\n?[ \t]*reached\b`)

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
	// BannerText is the whitespace-normalized text of the LAST banner match —
	// the lead the detector anchored on ("You've hit your 5-hour limit"). It
	// names WHICH wall the pane shows, which is what lets a transcript
	// refusal record prove it is the same wall's: a record whose own text
	// does not carry the banner names a different limit's wall and can never
	// supply this one's reset (#6975 review).
	BannerText string
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
	s := LimitSighting{
		Hit:          true,
		BannerPrefix: content[:last[0]],
		BannerCount:  len(matches),
		// The match can span a wrap inside the banner's own words; normalize
		// so the sighting carries the lead's text, not its layout.
		BannerText: strings.Join(strings.Fields(xansi.Strip(content[last[0]:last[1]])), " "),
	}
	if matcher.parseReset == nil {
		return s
	}
	lineStart := strings.LastIndexByte(content[:last[0]], '\n') + 1
	if reset, phrase, parsed := matcher.parseReset(content[lineStart:last[0]], content[last[0]:], now); parsed {
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
// "reset " / "resets " up to end of line, e.g. "2pm (America/New_York)" (5h) or
// "Monday at 9am (…)" / "Nov 6, 2026 9am (…)" (weekly). The "at" is optional
// because the weekday variant reads "reset Monday at 9am" — "reset <day>" —
// while the 5h variant reads "reset at 2pm", and the s is optional because the
// current banner reads "resets 9:40am". The word boundary keeps a fused token
// ("preset", "resetting" never a clause opener here) from opening a tail. Kept
// separate from detection so a reworded reset clause still detects (hit=true)
// even when this yields no parse.
var claudeResetAnchor = regexp.MustCompile(`(?i)\bresets?\s+(?:at\s+)?([^\r\n]+)`)

// claudeResetClause rejoins the banner row's own continuation rows — the only
// rows a wrapped clause may continue into (#6944) — then bounds the reset
// clause at its sentence end. It returns the whitespace-normalized clause as
// the parse's evidence and the tail after "reset[s] [at] " the clause carries.
func claudeResetClause(rowLead, content string) (evidence, tail string) {
	joined := joinClaudeBannerRows(rowLead, content)
	m := claudeResetAnchor.FindStringSubmatchIndex(joined)
	if m == nil {
		return "", ""
	}
	tail = joined[m[2]:]
	end := claudeClauseEnd(tail)
	if g := claudeResetTailEnd(tail); g < end {
		end = g
	}
	return strings.Join(strings.Fields(joined[m[0]:m[2]+end]), " "),
		strings.TrimSpace(tail[:end])
}

// joinClaudeBannerRows rejoins the screen rows of ONE wrapped banner line into
// its single logical line. The layout rule, and the only rejoin rule the pane
// parse may apply: a row continues the banner's line only when it carries the
// continuation indent Claude's renderer gives it — exactly the gutter's text
// column, measured on the ANSI-stripped row lead. Any other row (a transcript
// echo, a user draft, a prompt, prose at column 0 or a different indent) is a
// different row and ends the clause: whatever clock, weekday, date, or
// timezone it carries can never supply the reset (#6944).
//
// Why this is complete: tmux captures with -J, which already rejoins tmux's
// own grid-level hard wraps, so the only multi-row banner shapes left in a
// capture are the renderer's. Claude renders with a word-wrap: a row breaks at
// a space, and only a token wider than the whole text region splits mid-token.
// No row shape proves that split, though — a complete word can fill its row
// exactly, so a full-width single-token row feeding the boundary renders
// identically to a mid-token fragment: "resets|9:40am" at a six-column wrap
// is indistinguishable from "(America/New_|York)". Fusing on that guess mints
// tokens the clause never carried — "resets9:40am" loses the reset outright.
// Every boundary therefore keeps its space. The cost is the genuine
// hard-split — the paren zone is the only banner token ever wider than a
// narrow region — which rejoins spaced; its candidate then fails to load
// and the clause records NO reset rather than resolving in a guessed host
// zone (#6975 review). Never a fabrication.
func joinClaudeBannerRows(rowLead, content string) string {
	lead := xansi.Strip(rowLead)
	indent := utf8.RuneCountInString(lead)
	row0, rest := cutLine(content)
	row0 = strings.TrimRight(xansi.Strip(row0), " \t\r")
	if indent == 0 {
		// A banner at column 0 has no gutter to hang continuations under —
		// the clause cannot leave its row.
		return row0
	}
	pad := strings.Repeat(" ", indent)
	rows := []string{row0}
	for rest != "" {
		var raw string
		raw, rest = cutLine(rest)
		stripped := strings.TrimRight(xansi.Strip(raw), " \t\r")
		if !strings.HasPrefix(stripped, pad) ||
			(len(stripped) > indent && stripped[indent] == ' ') {
			break // not the continuation indent — a different row
		}
		text := strings.TrimSpace(stripped)
		if text == "" {
			break
		}
		rows = append(rows, text)
	}
	return strings.Join(rows, " ")
}

// claudeClauseEnd returns the reset clause's length inside tail: the clause
// runs to the first sentence-ending period — one at end of text or followed
// by whitespace then a letter — else to the tail's end. A period inside the
// clause ("Nov. 6, 2026", "Sept. 6", "v2.1.317") is followed by a digit or a
// non-space character, so it cannot bound the clause; and once the clause's
// own sentence has ended, no later sentence on the rejoined line — an
// indented row carrying its own clock — can reach back into it (#6944).
func claudeClauseEnd(tail string) int {
	for i := 0; i < len(tail); i++ {
		if tail[i] != '.' {
			continue
		}
		j := i + 1
		if j == len(tail) {
			return len(tail)
		}
		if tail[j] != ' ' && tail[j] != '\t' {
			continue // mid-token period: a version, decimal, or glued abbrev
		}
		for j < len(tail) && (tail[j] == ' ' || tail[j] == '\t') {
			j++
		}
		if j == len(tail) {
			return len(tail)
		}
		if c := tail[j]; c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			return i + 1
		}
	}
	return len(tail)
}

// claudeResetTailEnd bounds the reset clause's tail at its own grammar: the
// first 12-hour clock token, an optional RUN of following parenthesized
// spans (Claude renders "(Pro) (America/New_York)" — plan then zone — so a
// second adjacent paren is still the clause's own), and an optional
// sentence-ending period. Anything beyond is text the banner's sentence
// cannot contain — a following sentence in the same message rides the same
// continuation indent and, when the banner carries no closing period, has
// no punctuation boundary to stop it: a tip row's "(Europe/Berlin)" or a
// later clause's "resets Mon 9am" would otherwise supply this wall's zone
// or weekday (#6975 review). An unclosed paren keeps the rest — it is
// adjacent to the clock but proves nothing, so the bound stays open rather
// than guessing where it ends.
func claudeResetTailEnd(tail string) int {
	m := clockTime.FindStringIndex(tail)
	if m == nil {
		return len(tail)
	}
	end := m[1]
	for {
		i := end
		for i < len(tail) && (tail[i] == ' ' || tail[i] == '\t') {
			i++
		}
		if i >= len(tail) || tail[i] != '(' {
			break
		}
		j := strings.IndexByte(tail[i:], ')')
		if j < 0 {
			return len(tail)
		}
		end = i + j + 1
	}
	if end < len(tail) && tail[end] == '.' &&
		(end+1 == len(tail) || tail[end+1] == ' ' || tail[end+1] == '\t') {
		end++
	}
	return end
}

// cutLine returns the first line and the remainder of s, splitting at the
// first newline.
func cutLine(s string) (line, rest string) {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// parseClaudeReset scans the banner's rejoined line for Claude's "reset[s]
// [at] <tail>" clause and resolves the tail, in order: an optional timezone in
// parens (falling back to now's location when ABSENT — Claude renders in the
// account tz, which we cannot know without the paren, so the injected clock's
// zone is the deterministic stand-in — while a candidate that fails to load
// is a REJECTED zone: the banner named a zone the parse cannot prove, so the
// clause yields no reset rather than resolving the clock in a guessed host
// zone — #6975 review), a required 12-hour clock time, and an optional
// calendar date or weekday (the weekly variant).
// Without any date it mints the next occurrence of that clock time strictly
// after now. The returned evidence is the whitespace-normalized clause itself
// — "reset at 2pm (…)" — identical whenever the banner is re-read unchanged,
// wrapped or not (#6765). Returns ok=false when there is no reset clause or no
// clock time in it, so an unrecognized, reworded, or unprovably-cut tail
// degrades to hit=true, hasResetTime=false (#6944: a clause that cannot prove
// its extent records what it can prove, never a guess from a neighbor row).
func parseClaudeReset(rowLead, content string, now time.Time) (time.Time, string, bool) {
	evidence, reset := claudeResetClause(rowLead, content)
	if reset == "" {
		return time.Time{}, "", false
	}

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
		// The banner carried a parenthesized zone and every candidate failed
		// to load — a hard-split rejoins spaced ("(America/New_ York)"), an
		// unknown name is unknown, and neither can prove the zone the banner
		// named. The wall records no reset rather than a host-zone guess
		// (#6975 review: a rejected zone candidate resolving in the host
		// zone resumed one session five hours early).
		warnClaudeTimezoneOnce(content, rejected)
		return time.Time{}, "", false
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
func parseCodexReset(_ string, content string, now time.Time) (time.Time, string, bool) {
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
