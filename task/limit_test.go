package task

import (
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// nyLoc and fixedNow give every case a deterministic clock: Saturday
// 2026-07-04 10:00 America/New_York (14:00 UTC). All relative and
// time-of-day-only reset phrases resolve against this injected now, so no test
// touches the wall clock.
func nyLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load America/New_York: %v", err)
	}
	return loc
}

func TestIsLimitContent(t *testing.T) {
	loc := nyLoc(t)
	now := time.Date(2026, 7, 4, 10, 0, 0, 0, loc) // Sat 10:00 EDT / 14:00 UTC

	cases := []struct {
		name         string
		agent        string
		content      string
		wantHit      bool
		wantReset    bool
		wantResetUTC time.Time // only checked when wantReset
	}{
		{
			name:         "claude 5h window: time + tz, resets later today",
			agent:        tmux.ProgramClaude,
			content:      "Claude usage limit reached. Your limit will reset at 2pm (America/New_York)",
			wantHit:      true,
			wantReset:    true,
			wantResetUTC: time.Date(2026, 7, 4, 14, 0, 0, 0, loc).UTC(), // 18:00 UTC
		},
		{
			// Wrapped banner: the parenthesized timezone lands on the next row
			// (the 81-column daily banner overflows the stock 80-column detached
			// tmux pane). The fix's (?is) anchor crosses the newline so the paren
			// stays in the capture and the clock resolves in the account's zone,
			// not the daemon host's. Same reset as the unwrapped case above.
			name:  "claude 5h window: wrapped tz paren, resets later today",
			agent: tmux.ProgramClaude,
			content: "Claude usage limit reached. Your limit will reset at 2pm\n" +
				"(America/New_York).",
			wantHit:      true,
			wantReset:    true,
			wantResetUTC: time.Date(2026, 7, 4, 14, 0, 0, 0, loc).UTC(), // 18:00 UTC
		},
		{
			// Weekly variant, wrapped: same fix, same reset as the unwrapped
			// weekday case below.
			name:  "claude weekly: named weekday wrapped tz paren",
			agent: tmux.ProgramClaude,
			content: "Claude usage limit reached. Your limit will reset Monday at 9am\n" +
				"(America/New_York).",
			wantHit:      true,
			wantReset:    true,
			wantResetUTC: time.Date(2026, 7, 6, 9, 0, 0, 0, loc).UTC(), // Mon -> 13:00 UTC
		},
		{
			// Weekly variant with explicit date, wrapped: same fix, same reset
			// as the unwrapped dated case below.
			name:  "claude weekly: explicit date + year + wrapped tz paren",
			agent: tmux.ProgramClaude,
			content: "Claude usage limit reached. Your limit will reset at Nov 6, 2026 9am\n" +
				"(America/New_York).",
			wantHit:      true,
			wantReset:    true,
			wantResetUTC: time.Date(2026, 11, 6, 9, 0, 0, 0, loc).UTC(), // EST -> 14:00 UTC
		},
		{
			name:  "claude ignores earlier visible reset text before limit banner",
			agent: tmux.ProgramClaude,
			content: strings.Join([]string{
				"Running deployment notes: reset at 3pm (America/New_York)",
				"Build still in progress...",
				"Claude usage limit reached. Your limit will reset at 7pm (America/New_York)",
			}, "\n"),
			wantHit:      true,
			wantReset:    true,
			wantResetUTC: time.Date(2026, 7, 4, 19, 0, 0, 0, loc).UTC(), // 23:00 UTC
		},
		{
			// Two GENUINE banners in the pane (user tripped the limit, hit the
			// retry key, tripped it again). Reset must come from the LAST/most
			// recent banner (7pm), not the stale first one (2pm) — see #1602.
			name:  "claude two real banners: uses last banner's later reset time",
			agent: tmux.ProgramClaude,
			content: strings.Join([]string{
				"Claude usage limit reached. Your limit will reset at 2pm (America/New_York)",
				"c",
				"Claude usage limit reached. Your limit will reset at 7pm (America/New_York)",
			}, "\n"),
			wantHit:      true,
			wantReset:    true,
			wantResetUTC: time.Date(2026, 7, 4, 19, 0, 0, 0, loc).UTC(), // 23:00 UTC, not 18:00
		},
		{
			name:         "claude weekly: explicit date + year + tz",
			agent:        tmux.ProgramClaude,
			content:      "Claude usage limit reached. Your limit will reset at Nov 6, 2026 9am (America/New_York)",
			wantHit:      true,
			wantReset:    true,
			wantResetUTC: time.Date(2026, 11, 6, 9, 0, 0, 0, loc).UTC(), // EST -> 14:00 UTC
		},
		{
			name:         "claude weekly: named weekday rolls forward to next Monday",
			agent:        tmux.ProgramClaude,
			content:      "Claude usage limit reached. Your limit will reset Monday at 9am (America/New_York)",
			wantHit:      true,
			wantReset:    true,
			wantResetUTC: time.Date(2026, 7, 6, 9, 0, 0, 0, loc).UTC(), // Mon -> 13:00 UTC
		},
		{
			name:         "claude 5h window: no timezone falls back to now's location",
			agent:        tmux.ProgramClaude,
			content:      "Claude usage limit reached. Your limit will reset at 2:30pm",
			wantHit:      true,
			wantReset:    true,
			wantResetUTC: time.Date(2026, 7, 4, 14, 30, 0, 0, loc).UTC(), // 18:30 UTC
		},
		{
			name:         "codex 5h window: time-only rolls to tomorrow (already past today)",
			agent:        tmux.ProgramCodex,
			content:      "You've hit your usage limit. Please try again at 6:34 AM.",
			wantHit:      true,
			wantReset:    true,
			wantResetUTC: time.Date(2026, 7, 5, 6, 34, 0, 0, loc).UTC(), // 10:34 UTC
		},
		{
			name:         "codex weekly: full date + time with ordinal suffix",
			agent:        tmux.ProgramCodex,
			content:      "You've hit your usage limit. You can try again at Jul 25th, 2026 5:55 PM.",
			wantHit:      true,
			wantReset:    true,
			wantResetUTC: time.Date(2026, 7, 25, 17, 55, 0, 0, loc).UTC(), // 21:55 UTC
		},
		{
			name:  "codex current banner: typographic apostrophe and wrapped purchase clause",
			agent: tmux.ProgramCodex,
			content: "■ You’ve hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to\n" +
				"purchase more credits or try again at Sep 20th, 2026 9:16 AM.",
			wantHit:      true,
			wantReset:    true,
			wantResetUTC: time.Date(2026, 9, 20, 9, 16, 0, 0, loc).UTC(),
		},
		{
			name:  "codex weekly: purchase URL before wrapped reset clause",
			agent: tmux.ProgramCodex,
			content: "■ You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to\n" +
				"  purchase more credits or try again at Sep 17th, 2026 1:34 PM.",
			wantHit:      true,
			wantReset:    true,
			wantResetUTC: time.Date(2026, 9, 17, 13, 34, 0, 0, loc).UTC(),
		},
		{
			name:         "codex relative countdown: try again in N days/hours/minutes",
			agent:        tmux.ProgramCodex,
			content:      "You've hit your usage limit, try again in 4 days 2 hours 46 minutes.",
			wantHit:      true,
			wantReset:    true,
			wantResetUTC: now.Add(4*24*time.Hour + 2*time.Hour + 46*time.Minute).UTC(),
		},
		{
			name:      "claude banner present but reset time unparseable -> hit, no reset",
			agent:     tmux.ProgramClaude,
			content:   "Claude usage limit reached. Your limit will reset at some point soon.",
			wantHit:   true,
			wantReset: false,
		},
		// devin (#2411): detect-only. devin renders exhaustion as
		// "Quota exhausted: <message>" (the <message> is BACKEND-supplied) and as a
		// bare "Quota exhausted" UI heading, so the matcher anchors on the invariant
		// "quota exhausted" prefix, NOT the default message half the backend
		// overrides. Each is a hit with NO reset time (the format is
		// uncharacterized). devin's HEALTHY quota-status displays — which share the
		// "quota" vocabulary — must NOT trip it.
		{
			// The regression this matcher's anchor was corrected for: a
			// backend-worded message (NOT the "usage quota has been exhausted"
			// default) must still be caught by the "Quota exhausted:" prefix.
			name:      "devin exhaustion, backend-supplied message -> hit, no reset (detect-only)",
			agent:     tmux.ProgramDevin,
			content:   "Quota exhausted: You have used all of your ACUs for this billing cycle.",
			wantHit:   true,
			wantReset: false,
		},
		{
			name:      "devin exhaustion, default message -> hit, no reset",
			agent:     tmux.ProgramDevin,
			content:   "Quota exhausted: usage quota has been exhausted",
			wantHit:   true,
			wantReset: false,
		},
		{
			name:      "devin exhaustion, bare UI heading -> hit, no reset",
			agent:     tmux.ProgramDevin,
			content:   "Quota exhausted\nUpgrade your plan or wait for your quota to reset",
			wantHit:   true,
			wantReset: false,
		},
		{
			name:      "devin exhaustion banner is case-insensitive",
			agent:     tmux.ProgramDevin,
			content:   "QUOTA EXHAUSTED",
			wantHit:   true,
			wantReset: false,
		},
		{
			name:    "devin healthy: percent remaining is not a limit",
			agent:   tmux.ProgramDevin,
			content: "❭ 59% remaining, resets in 3d",
			wantHit: false,
		},
		{
			name:    "devin healthy: (resets clause is not a limit",
			agent:   tmux.ProgramDevin,
			content: "Quota: 12% used (resets in 3 days)",
			wantHit: false,
		},
		{
			name:    "devin healthy: Quota used: line is not a limit",
			agent:   tmux.ProgramDevin,
			content: "Quota used: 41%",
			wantHit: false,
		},
		{
			name:    "devin healthy: Quota resets line is not a limit",
			agent:   tmux.ProgramDevin,
			content: "Quota resets in 2 days",
			wantHit: false,
		},
		{
			name:      "codex banner present but no reset phrase -> hit, no reset",
			agent:     tmux.ProgramCodex,
			content:   "You've hit your usage limit. Upgrade your plan for more.",
			wantHit:   true,
			wantReset: false,
		},
		{
			name:    "claude ready prompt, no banner -> no hit",
			agent:   tmux.ProgramClaude,
			content: "❯ waiting for your next instruction",
			wantHit: false,
		},
		{
			name:    "codex working pane, no banner -> no hit",
			agent:   tmux.ProgramCodex,
			content: "› thinking...\nRunning tests",
			wantHit: false,
		},
		{
			name:    "codex healthy prose: conditional usage-limit explanation is not exhaustion",
			agent:   tmux.ProgramCodex,
			content: "If you hit your usage limit, af parks the session",
			wantHit: false,
		},
		{
			name:    "codex healthy prose: usage-limit badge documentation is not exhaustion",
			agent:   tmux.ProgramCodex,
			content: "Sessions that hit your usage limit are shown with a [limit] badge.",
			wantHit: false,
		},
		{
			name:    "gemini limit banner is surface-only in v1 -> no hit",
			agent:   tmux.ProgramGemini,
			content: "Error 429: RESOURCE_EXHAUSTED. Claude usage limit reached. Your limit will reset at 2pm (America/New_York)",
			wantHit: false,
		},
		{
			name:    "aider rate-limit is API-key metered in v1 -> no hit",
			agent:   tmux.ProgramAider,
			content: "litellm.RateLimitError: You've hit your usage limit. try again at 6:34 AM",
			wantHit: false,
		},
		{
			name:    "unknown/non-agent resolved command -> no hit",
			agent:   "",
			content: "Claude usage limit reached. Your limit will reset at 2pm (America/New_York)",
			wantHit: false,
		},
	}

	matchers := resolveLimitMatchers(nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hit, resetAt, hasReset, _ := isLimitContent(tc.content, tc.agent, matchers, now)
			if hit != tc.wantHit {
				t.Fatalf("hit = %v, want %v", hit, tc.wantHit)
			}
			if hasReset != tc.wantReset {
				t.Fatalf("hasResetTime = %v, want %v (resetAt=%v)", hasReset, tc.wantReset, resetAt)
			}
			if !tc.wantReset {
				if !resetAt.IsZero() {
					t.Fatalf("resetAt = %v, want zero when no reset time", resetAt)
				}
				return
			}
			if !resetAt.Equal(tc.wantResetUTC) {
				t.Fatalf("resetAt = %v, want %v", resetAt.UTC(), tc.wantResetUTC)
			}
			if resetAt.Location() != time.UTC {
				t.Fatalf("resetAt location = %v, want UTC", resetAt.Location())
			}
			if !resetAt.After(now) {
				t.Fatalf("resetAt = %v is not after now = %v", resetAt, now)
			}
		})
	}
}

// TestIsLimitContentPatternOverride proves a config-provided regex replaces the
// built-in detection pattern for an agent: content the built-in would miss is
// detected once the override is applied, and the built-in reset-time parser
// still runs against the same content.
func TestIsLimitContentPatternOverride(t *testing.T) {
	loc := nyLoc(t)
	now := time.Date(2026, 7, 4, 10, 0, 0, 0, loc)

	// A reworded banner the shipped claude pattern does not match.
	content := "PLAN LIMIT TRIPPED — reset at 3pm (America/New_York)"

	// Without an override the built-in claude matcher does not detect it.
	if hit, _, _, _ := isLimitContent(content, tmux.ProgramClaude, resolveLimitMatchers(nil), now); hit {
		t.Fatalf("built-in matcher unexpectedly detected reworded banner")
	}

	// With the override the reworded banner is detected, and the built-in
	// reset parser still resolves "reset at 3pm (America/New_York)".
	overrides := map[string]string{tmux.ProgramClaude: `PLAN LIMIT TRIPPED`}
	hit, resetAt, hasReset, _ := isLimitContent(content, tmux.ProgramClaude, resolveLimitMatchers(overrides), now)
	if !hit {
		t.Fatalf("override matcher did not detect reworded banner")
	}
	if !hasReset {
		t.Fatalf("expected reset time to be parsed from overridden banner")
	}
	want := time.Date(2026, 7, 4, 15, 0, 0, 0, loc).UTC() // 19:00 UTC
	if !resetAt.Equal(want) {
		t.Fatalf("resetAt = %v, want %v", resetAt.UTC(), want)
	}
}

// TestResolveLimitMatchers covers the resolver's guard rails: unknown-agent
// and uncompilable overrides are dropped (built-in stands), the built-ins are
// present for claude/codex (full) and devin (detect-only) but no other agent,
// and a valid override for an agent WITH a built-in is honored — replacing the
// detect while keeping its reset parser (nil for devin's detect-only entry).
func TestResolveLimitMatchers(t *testing.T) {
	base := resolveLimitMatchers(nil)
	if _, ok := base[tmux.ProgramClaude]; !ok {
		t.Fatalf("built-in claude matcher missing")
	}
	if _, ok := base[tmux.ProgramCodex]; !ok {
		t.Fatalf("built-in codex matcher missing")
	}
	// devin has a DETECT-ONLY matcher (#2411): present, but with no reset parser.
	devinMatcher, ok := base[tmux.ProgramDevin]
	if !ok {
		t.Fatalf("built-in devin matcher missing")
	}
	if devinMatcher.parseReset != nil {
		t.Fatalf("devin must be detect-only: parseReset must be nil (no auto-resume from an uncharacterized reset format)")
	}
	if _, ok := base[tmux.ProgramGemini]; ok {
		t.Fatalf("gemini should have no matcher in v1")
	}
	if _, ok := base[tmux.ProgramAider]; ok {
		t.Fatalf("aider should have no matcher in v1")
	}
	if _, ok := base[tmux.ProgramAmp]; ok {
		t.Fatalf("amp should have no matcher in v1")
	}
	// opencode is API-key metered (Anthropic/OpenAI keys), so it never stalls at
	// a dead prompt behind a plan-reset banner — see limit.go's scope note. A
	// "limit" there is a transient API failure carrying no reset timestamp to
	// schedule an auto-resume against, so it gets no matcher.
	if _, ok := base[tmux.ProgramOpencode]; ok {
		t.Fatalf("opencode is API-key metered and has no plan-reset banner to parse; it should have no matcher")
	}

	// An override for an agent with no built-in matcher is ignored — there is
	// nothing to layer it onto (gemini/amp/opencode).
	withGemini := resolveLimitMatchers(map[string]string{tmux.ProgramGemini: `whatever`})
	if _, ok := withGemini[tmux.ProgramGemini]; ok {
		t.Fatalf("override for unmatched agent should be ignored")
	}
	withAmp := resolveLimitMatchers(map[string]string{tmux.ProgramAmp: `whatever`})
	if _, ok := withAmp[tmux.ProgramAmp]; ok {
		t.Fatalf("override for amp should be ignored until amp has a built-in matcher")
	}
	withOpencode := resolveLimitMatchers(map[string]string{tmux.ProgramOpencode: `whatever`})
	if _, ok := withOpencode[tmux.ProgramOpencode]; ok {
		t.Fatalf("override for opencode should be ignored until opencode has a built-in matcher")
	}

	// An override for an agent WITH a built-in matcher is HONORED: it swaps the
	// detect pattern and keeps the built-in reset parser. For devin (#2411) that
	// parser is nil, so the override stays detect-only — it must not invent a reset
	// time. This is the config contract #2411 leans on: a user who can see their own
	// exhausted devin pane can point the matcher at the wording they actually
	// observe, even before a built-in capture exists. Before devin had a built-in
	// entry this override was dropped with a warning; it is now accepted.
	now := time.Date(2026, 7, 4, 10, 0, 0, 0, nyLoc(t))
	withDevin := resolveLimitMatchers(map[string]string{tmux.ProgramDevin: `(?i)rate ceiling hit`})
	dm, ok := withDevin[tmux.ProgramDevin]
	if !ok {
		t.Fatalf("valid devin override should be accepted, not dropped")
	}
	if dm.parseReset != nil {
		t.Fatalf("devin override must stay detect-only: parseReset must remain nil")
	}
	// The override REPLACES detection: the user's pattern hits...
	if hit, _, _, _ := isLimitContent("the provider says: RATE CEILING HIT", tmux.ProgramDevin, withDevin, now); !hit {
		t.Fatalf("devin override pattern should detect the user-supplied banner")
	}
	// ...and the built-in default ("quota exhausted") no longer does.
	if hit, _, _, _ := isLimitContent("Quota exhausted: usage quota has been exhausted", tmux.ProgramDevin, withDevin, now); hit {
		t.Fatalf("devin override should REPLACE the built-in detect, not augment it")
	}

	// An uncompilable regex is dropped and the built-in default stands: the
	// stock claude banner must still detect.
	bad := resolveLimitMatchers(map[string]string{tmux.ProgramClaude: `(unclosed`})
	hit, _, _, _ := isLimitContent("Claude usage limit reached. Your limit will reset at 2pm (America/New_York)", tmux.ProgramClaude, bad, now)
	if !hit {
		t.Fatalf("invalid override should fall back to built-in claude matcher")
	}
}

// TestLimitDetector covers the exported facade the daemon uses (#1146 PR2):
// NewLimitDetector + Check apply config overrides and return the same contract
// as the internal isLimitContent, and a non-scheduled agent never matches.
func TestLimitDetector(t *testing.T) {
	loc := nyLoc(t)
	now := time.Date(2026, 7, 4, 10, 0, 0, 0, loc)

	det := NewLimitDetector(nil)
	hit, resetAt, hasReset, _ := det.Check("Claude usage limit reached. Your limit will reset at 2pm (America/New_York)", tmux.ProgramClaude, now)
	if !hit || !hasReset {
		t.Fatalf("Check should detect the stock claude banner with a reset time; got hit=%v hasReset=%v", hit, hasReset)
	}
	want := time.Date(2026, 7, 4, 14, 0, 0, 0, loc).UTC()
	if !resetAt.Equal(want) {
		t.Fatalf("resetAt = %v, want %v", resetAt.UTC(), want)
	}

	// gemini is API-key-metered — no matcher, so never a hit.
	if hit, _, _, _ := det.Check("429 RESOURCE_EXHAUSTED", tmux.ProgramGemini, now); hit {
		t.Fatalf("gemini must not produce a limit hit in v1")
	}

	// A config override on the detector is honored.
	overridden := NewLimitDetector(map[string]string{tmux.ProgramClaude: `PLAN LIMIT TRIPPED`})
	if hit, _, _, _ := overridden.Check("PLAN LIMIT TRIPPED at 3pm", tmux.ProgramClaude, now); !hit {
		t.Fatalf("override detector should detect the reworded banner")
	}
}

// TestCheckReturnsResetPhraseEvidence pins the evidence contract (#6765): the
// detector reports the reset phrase it consumed so the poll's caller can tell
// a re-read of an unchanged banner from a new reset observation. The phrase is
// stable across parse instants even when the resolved reset time is not —
// exactly the drift the session dedups on.
func TestCheckReturnsResetPhraseEvidence(t *testing.T) {
	loc := nyLoc(t)
	now := time.Date(2026, 7, 4, 10, 0, 0, 0, loc)
	det := NewLimitDetector(nil)

	cases := []struct {
		name         string
		pane         string
		agent        string
		wantEvidence string
	}{
		{"claude time-only", "⎿  Claude usage limit reached. Your limit will reset at 2pm (America/New_York)", tmux.ProgramClaude, "reset at 2pm (America/New_York)"},
		{"claude weekday", "⎿  Claude usage limit reached. Your limit will reset Monday at 9am (America/New_York)", tmux.ProgramClaude, "reset Monday at 9am (America/New_York)"},
		// Wrapped banners: the evidence is the normalized (whitespace-collapsed)
		// reset clause, stable across re-reads (#6765). The newline from the wrap
		// is collapsed to a space so the evidence is a clean single-line phrase.
		{"claude time-only wrapped", "⎿  Claude usage limit reached. Your limit will reset at 2pm\n(America/New_York).", tmux.ProgramClaude, "reset at 2pm (America/New_York)."},
		{"claude weekday wrapped", "⎿  Claude usage limit reached. Your limit will reset Monday at 9am\n(America/New_York).", tmux.ProgramClaude, "reset Monday at 9am (America/New_York)."},
		{"codex dated", "■ You’ve hit your usage limit. Please try again at Sep 20th, 2026 9:16 AM.", tmux.ProgramCodex, "try again at Sep 20th, 2026 9:16 AM"},
		{"codex time-only", "■ You've hit your usage limit. Please try again at 6:34 AM.", tmux.ProgramCodex, "try again at 6:34 AM"},
		{"codex relative", "■ You've hit your usage limit, try again in 4 days 2 hours 46 minutes.", tmux.ProgramCodex, "try again in 4 days 2 hours 46 minutes."},
	}
	for _, tc := range cases {
		hit, _, hasReset, evidence := det.Check(tc.pane, tc.agent, now)
		if !hit || !hasReset {
			t.Fatalf("%s: expected hit with parsed reset, got hit=%v hasReset=%v", tc.name, hit, hasReset)
		}
		if evidence != tc.wantEvidence {
			t.Fatalf("%s: evidence = %q, want %q", tc.name, evidence, tc.wantEvidence)
		}
	}

	// The sharp case: a relative phrase re-resolves to a later instant every
	// parse, but the phrase itself — the re-read's identity — is unchanged.
	pane := "■ You've hit your usage limit, try again in 4 days 2 hours 46 minutes."
	_, reset1, _, ev1 := det.Check(pane, tmux.ProgramCodex, now)
	_, reset2, _, ev2 := det.Check(pane, tmux.ProgramCodex, now.Add(time.Hour))
	if reset1.Equal(reset2) {
		t.Fatalf("a relative countdown must re-resolve against the later clock: %v == %v", reset1, reset2)
	}
	if ev1 != ev2 {
		t.Fatalf("an unchanged banner must reproduce identical evidence: %q != %q", ev1, ev2)
	}

	// A banner with no parseable reset phrase carries no evidence.
	if hit, _, hasReset, evidence := det.Check("Claude usage limit reached. Your limit will reset at some point soon.", tmux.ProgramClaude, now); !hit || hasReset || evidence != "" {
		t.Fatalf("an unparseable reset clause must detect with no evidence: hit=%v hasReset=%v evidence=%q", hit, hasReset, evidence)
	}
}

// TestSight pins the banner anchor (#6823): Sight reports Check's claims plus
// the bytes before the LAST banner match and the anchored match count — the
// pair a parked session compares against its recorded sighting to tell a
// clipped re-read of that banner from a banner printed after it, which the
// (resetAt, evidence) claim alone cannot do once both degrade to (zero, "").
func TestSight(t *testing.T) {
	loc := nyLoc(t)
	now := time.Date(2026, 7, 4, 10, 0, 0, 0, loc)
	det := NewLimitDetector(nil)

	// Two genuine banners (a retry tripped the wall again before the first
	// scrolled off): the anchor must name the SECOND one's position, matching
	// the banner whose reset the claim carries — exactly like the parse.
	two := strings.Join([]string{
		"output before the wall",
		"⎿  Claude usage limit reached. Your limit will reset at 2pm (America/New_York)",
		"user pressed retry",
		"⎿  Claude usage limit reached. Your limit will reset at 7pm (America/New_York)",
		"trailing composer line",
	}, "\n")
	s := det.Sight(two, tmux.ProgramClaude, now)
	if !s.Hit || !s.HasResetTime {
		t.Fatalf("two-banner sighting: got hit=%v hasReset=%v", s.Hit, s.HasResetTime)
	}
	lastIdx := strings.LastIndex(two, "Claude usage limit reached.")
	if s.BannerPrefix != two[:lastIdx] {
		t.Fatalf("BannerPrefix must be the bytes before the LAST banner match")
	}
	if s.BannerCount != 2 {
		t.Fatalf("BannerCount = %d, want 2 anchored banners", s.BannerCount)
	}
	if !strings.HasPrefix(two[lastIdx:], "Claude usage limit reached.") {
		t.Fatalf("test fixture lost its last match")
	}

	// A clipped re-read — the same banner with its reset clause scrolled out —
	// still anchors: Hit stands with no reset, and the prefix/count still name
	// the banner the session recorded.
	clipped := "output before the wall\n⎿  Claude usage limit reached."
	s = det.Sight(clipped, tmux.ProgramClaude, now)
	if !s.Hit || s.HasResetTime || s.Evidence != "" || !s.ResetAt.IsZero() {
		t.Fatalf("clipped sighting: got hit=%v hasReset=%v evidence=%q", s.Hit, s.HasResetTime, s.Evidence)
	}
	if s.BannerPrefix != "output before the wall\n⎿  " || s.BannerCount != 1 {
		t.Fatalf("clipped sighting must still anchor: prefix=%q count=%d", s.BannerPrefix, s.BannerCount)
	}

	// A mid-line mention of the banner's words is not an anchored match
	// (#4721), so it contributes neither the hit nor the count.
	s = det.Sight("the agent said: Claude usage limit reached. — quoting docs", tmux.ProgramClaude, now)
	if s.Hit || s.BannerCount != 0 {
		t.Fatalf("an unanchored mention must not sight: %+v", s)
	}

	// A miss reports the zero sighting.
	if s = det.Sight("an ordinary idle pane", tmux.ProgramClaude, now); s != (LimitSighting{}) {
		t.Fatalf("a miss must be the zero sighting, got %+v", s)
	}
	// Check and Sight agree on the claim fields.
	hit, resetAt, hasReset, evidence := det.Check(two, tmux.ProgramClaude, now)
	s = det.Sight(two, tmux.ProgramClaude, now)
	if s.Hit != hit || !s.ResetAt.Equal(resetAt) || s.HasResetTime != hasReset || s.Evidence != evidence {
		t.Fatalf("Check and Sight disagree: (%v,%v,%v,%q) vs %+v", hit, resetAt, hasReset, evidence, s)
	}
}

// TestClaudeResetWrappedBannerTimezone pins the fix for the wrapped-banner
// timezone truncation bug: when Claude Code word-wraps its limit banner and the
// wrap pushes the parenthesized timezone onto the next row, the claudeResetAnchor
// regex must cross the newline so the paren stays in the capture and the clock
// time resolves in the account's zone — not the daemon host's. The bug report's
// reproduction clocks the daemon in UTC (the host-zone fallback) and shows the
// wrapped 2pm (America/New_York) banner parsing as 14:00Z (2pm read in UTC) instead
// of 19:00Z (2pm EST) — a 5-hour-too-early reset. This test reproduces that
// exactly: a UTC now, the wrapped and unwrapped forms of the same banner, and
// the assertion that both resolve to the same EST instant.
func TestClaudeResetWrappedBannerTimezone(t *testing.T) {
	// UTC-clocked reproduction: the daemon host's zone is UTC, so the bug's
	// fallback (now.Location()) reads 2pm in UTC. America/New_York is EST
	// (UTC-5) in January, so 2pm EST = 19:00Z — the correct parse the wrapped
	// form must also produce.
	now := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC) // 05:00 America/New_York
	est, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	want2pm := time.Date(2026, 1, 2, 14, 0, 0, 0, est).UTC() // 19:00Z

	cases := []struct {
		name    string
		content string
		want    time.Time
	}{
		{
			name:    "5h unwrapped (control)",
			content: "Claude usage limit reached. Your limit will reset at 2pm (America/New_York).",
			want:    want2pm,
		},
		{
			name:    "5h wrapped: tz paren on next row",
			content: "Claude usage limit reached. Your limit will reset at 2pm\n(America/New_York).",
			want:    want2pm,
		},
		{
			name:    "5h wrapped: break between reset and at (\\s crosses newline)",
			content: "Claude usage limit reached. Your limit will reset\nat 2pm (America/New_York).",
			want:    want2pm,
		},
		{
			name:    "weekly weekday unwrapped (control)",
			content: "Claude usage limit reached. Your limit will reset Monday at 9am (America/New_York).",
			want:    time.Date(2026, 1, 5, 9, 0, 0, 0, est).UTC(), // Mon 9am EST = 14:00Z
		},
		{
			name:    "weekly weekday wrapped: tz paren on next row",
			content: "Claude usage limit reached. Your limit will reset Monday at 9am\n(America/New_York).",
			want:    time.Date(2026, 1, 5, 9, 0, 0, 0, est).UTC(), // same as unwrapped
		},
		{
			name:    "weekly date unwrapped (control)",
			content: "Claude usage limit reached. Your limit will reset at Nov 6, 2026 9am (America/New_York).",
			want:    time.Date(2026, 11, 6, 9, 0, 0, 0, est).UTC(), // EST -> 14:00Z
		},
		{
			name:    "weekly date wrapped: tz paren on next row",
			content: "Claude usage limit reached. Your limit will reset at Nov 6, 2026 9am\n(America/New_York).",
			want:    time.Date(2026, 11, 6, 9, 0, 0, 0, est).UTC(), // same as unwrapped
		},
		{
			// Wrap INSIDE the parenthesized timezone token, not just before
			// it: the newline must rejoin the zone name (no space inserted) so
			// time.LoadLocation still accepts "America/New_York". A naive
			// newline-to-space normalization would yield "America/ New_York"
			// and silently fall back to the host zone — the bug this change
			// fixes.
			name:    "5h wrapped: tz split inside the paren",
			content: "Claude usage limit reached. Your limit will reset at 2pm (America/\nNew_York).",
			want:    want2pm,
		},
		{
			// Abbreviated month that keeps its period ("Nov. 6"). The anchor
			// must not treat that period as the sentence-ending terminator and
			// truncate the capture to "Nov." — the clock time still has to be
			// found further on. The unwrapped and wrapped forms both parse.
			name:    "weekly date: abbreviated month keeps its period",
			content: "Claude usage limit reached. Your limit will reset at Nov. 6, 2026 9am (America/New_York).",
			want:    time.Date(2026, 11, 6, 9, 0, 0, 0, est).UTC(), // EST -> 14:00Z
		},
		{
			// A narrow pane may wrap an abbreviated date immediately after
			// the period ("Nov.\n6, 2026 9am"). The period-plus-newline must
			// not terminate the clause here: the abbreviated-month period is
			// not a sentence terminator, so the capture crosses the wrap and
			// the clock still resolves — same instant as the unwrapped form.
			name:    "weekly date: abbreviated month wraps after its period",
			content: "Claude usage limit reached. Your limit will reset at Nov.\n6, 2026 9am (America/New_York).",
			want:    time.Date(2026, 11, 6, 9, 0, 0, 0, est).UTC(), // EST -> 14:00Z
		},
		{
			// The production shape (tmux `capture-pane -e`): the whole reset
			// clause is one ANSI-colored span — "\x1b[38;5;211m…\x1b[39m" — and
			// a narrow pane wraps the abbreviated date after its period. The
			// wrap newline carries no escape (the color persists across the
			// wrap), and the SGR reset sits at the end of the styled clause
			// right after the final period, before the pane newline. This must
			// still parse to the same EST instant as the unwrapped form, and
			// the evidence must be whitespace-normalized (no raw newline).
			name:    "weekly date: styled capture wraps abbreviated month",
			content: "\x1b[38;5;211mClaude usage limit reached. Your limit will reset at Nov.\n6, 2026 9am (America/New_York).\x1b[39m",
			want:    time.Date(2026, 11, 6, 9, 0, 0, 0, est).UTC(), // EST -> 14:00Z
		},
		{
			// A narrow pane may wrap INSIDE a multi-digit date token — the
			// day "12" splits as "1\n2" — and the wrap newline must rejoin
			// the digits (drop, not space) so monthDay sees "Nov 12" rather
			// than "Nov 1 2" (which would parse the day as 1). Same EST
			// instant as the unwrapped "Nov 12, 2026 9am" form.
			name:    "weekly date: day wraps inside its digits",
			content: "Claude usage limit reached. Your limit will reset at Nov 1\n2, 2026 9am (America/New_York).",
			want:    time.Date(2026, 11, 12, 9, 0, 0, 0, est).UTC(), // EST -> 14:00Z
		},
		{
			// A narrow pane may wrap INSIDE a multi-digit clock-hour token
			// — "11am" splits as "1\n1am" — and the wrap newline must rejoin
			// so parseClockTime sees "11am" (hour=11) rather than "1 1am"
			// (hour=1). Same EST instant as the unwrapped "11am" form.
			name:    "5h: hour wraps inside its digits",
			content: "Claude usage limit reached. Your limit will reset at 1\n1am (America/New_York).",
			want:    time.Date(2026, 1, 2, 11, 0, 0, 0, est).UTC(), // 11am EST -> 16:00Z
		},
		{
			// A narrow pane may wrap INSIDE an alphabetic month token —
			// "Nov" splits as "No\nv" — and the wrap newline must rejoin the
			// letters (drop, not space) so monthDay sees "Nov 6" rather than
			// "No v 6" (which misses the explicit date). Same EST instant as
			// the unwrapped "Nov 6, 2026 9am" form.
			name:    "weekly date: month wraps inside its letters",
			content: "Claude usage limit reached. Your limit will reset at No\nv 6, 2026 9am (America/New_York).",
			want:    time.Date(2026, 11, 6, 9, 0, 0, 0, est).UTC(), // EST -> 14:00Z
		},
		{
			// A narrow pane may wrap BETWEEN separate numeric tokens — the
			// day "6" followed by the year "2026" with no comma ("Nov 6\n2026
			// 9am"). The wrap must NOT rejoin the digits (it is not a mid-token
			// split): "Nov 62026" would let monthDay consume "62" as the day and
			// time.Date would normalize it months later. The wrap stays a
			// space so monthDay sees "Nov 6 2026" (day 6, year 2026) — the same
			// EST instant as the unwrapped "Nov 6, 2026 9am" form.
			name:    "weekly date: year wraps after the day without a comma",
			content: "Claude usage limit reached. Your limit will reset at Nov 6\n2026 9am (America/New_York).",
			want:    time.Date(2026, 11, 6, 9, 0, 0, 0, est).UTC(), // EST -> 14:00Z
		},
		{
			// A narrow pane may wrap INSIDE a four-digit year token —
			// "2027" splits as "20\n27" ("Nov 6, 20\n27 9am") — and the
			// wrap newline must rejoin (drop, not space) so monthDay sees
			// the explicit year "2027" rather than "20 27", which would
			// miss the year and diverge the evidence from the unwrapped
			// form (breaking the #6765 re-read identity across pane
			// resizes). Same EST instant as the unwrapped "Nov 6, 2027
			// 9am" form.
			name:    "weekly date: year wraps inside its digits",
			content: "Claude usage limit reached. Your limit will reset at Nov 6, 20\n27 9am (America/New_York).",
			want:    time.Date(2027, 11, 6, 9, 0, 0, 0, est).UTC(), // EST -> 14:00Z
		},
		{
			// A narrow pane may wrap INSIDE a clock at the colon — the
			// minutes "30" land on the next row ("2:\n30pm"). The wrap
			// newline must rejoin (drop, not space) so parseClockTime sees
			// "2:30pm" (hour 2, minute 30) rather than "2: 30pm", which it
			// rejects (the disjoint "30pm" reads hour 30 > 12). Same EST
			// instant as the unwrapped "2:30pm" form.
			name:    "5h: minutes wrap after the colon",
			content: "Claude usage limit reached. Your limit will reset at 2:\n30pm (America/New_York).",
			want:    time.Date(2026, 1, 2, 14, 30, 0, 0, est).UTC(), // 2:30pm EST -> 19:30Z
		},
		{
			// The colon itself may wrap onto the next row ("2\n:30pm"). The
			// wrap newline must rejoin (drop, not space) so parseClockTime
			// sees "2:30pm" rather than "2 :30pm". Same EST instant as the
			// unwrapped "2:30pm" form.
			name:    "5h: colon wraps before the minutes",
			content: "Claude usage limit reached. Your limit will reset at 2\n:30pm (America/New_York).",
			want:    time.Date(2026, 1, 2, 14, 30, 0, 0, est).UTC(), // 2:30pm EST -> 19:30Z
		},
		{
			// A wrapped-to-next-row parenthesized timezone must carry a
			// trailing period (the shipped wrapped-banner shape): without one,
			// the paren on the next row is treated as unrelated transcript text
			// rather than the banner's zone, so the clock resolves in the
			// injected clock's zone (UTC here) — NOT in the paren's location.
			// "2pm\n(America/Los_Angeles)" with no period must NOT adopt LA.
			name:    "5h wrapped: next-row paren without period falls back to now's zone",
			content: "Claude usage limit reached. Your limit will reset at 2pm\n(America/Los_Angeles)",
			want:    time.Date(2026, 1, 2, 14, 0, 0, 0, time.UTC).UTC(), // 2pm UTC, NOT 2pm LA
		},
		{
			// A narrow pane may wrap BETWEEN the day and the clock hour of a
			// comma-less date — "Nov 6\n9am" — and the wrap must NOT rejoin
			// the digits: "Nov 69am" makes parseClockTime reject hour 69 and
			// the banner loses its reset time. The wrap stays a space so
			// monthDay sees "Nov 6" (day 6) and parseClockTime sees "9am"
			// (hour 9). Same EST instant as the unwrapped "Nov 6 9am" form.
			// (Run length alone cannot identify this split: day+clock is
			// 1+1 = 2 digits, byte-identical to a mid-token day "Nov 1\n2";
			// the [ap]m following the post-run is what marks it a clock
			// boundary, and the joined "69" > 12 is what keeps it a space.)
			name:    "weekly date: day wraps before the clock (no comma)",
			content: "Claude usage limit reached. Your limit will reset at Nov 6\n9am (America/New_York).",
			want:    time.Date(2026, 11, 6, 9, 0, 0, 0, est).UTC(), // EST -> 14:00Z
		},
		{
			// A narrow pane may wrap INSIDE the two-letter [ap]m meridiem
			// suffix — "2pm" splits as "2p\nm" — and the wrap must rejoin
			// (drop, not space) so the clockTime regex sees "2pm" (the [ap]
			// and m must be contiguous) rather than "2p m", which it rejects.
			// Same EST instant as the unwrapped "2pm" form.
			name:    "5h: meridiem wraps inside its suffix",
			content: "Claude usage limit reached. Your limit will reset at 2p\nm (America/New_York).",
			want:    want2pm,
		},
		{
			// A narrow pane may push the trailing period onto its own row —
			// "2pm (America/New_York)\n." — and the wrap must rejoin (drop,
			// not space) so the evidence carries the period as the unwrapped
			// "(…)." form rather than the divergent "(…) ." (which would
			// break the #6765 re-read identity across pane resizes). Same
			// EST instant as the unwrapped "2pm (…)." form.
			name:    "5h wrapped: trailing period wraps to its own row",
			content: "Claude usage limit reached. Your limit will reset at 2pm (America/New_York)\n.",
			want:    want2pm,
		},
		{
			// A narrow pane may wrap BETWEEN the day and its comma — "Nov 6\n,
			// 2027 9am" — and the wrap must rejoin (drop, not space) so
			// monthDay sees "Nov 6, 2027" (the comma immediately follows the
			// day, so the explicit year matches) rather than "Nov 6 , 2027",
			// which matches only "Nov 6" and discards the year. Same EST
			// instant as the unwrapped "Nov 6, 2027 9am" form.
			name:    "weekly date: comma wraps after the day",
			content: "Claude usage limit reached. Your limit will reset at Nov 6\n, 2027 9am (America/New_York).",
			want:    time.Date(2027, 11, 6, 9, 0, 0, 0, est).UTC(), // EST -> 14:00Z
		},
		{
			// A narrow pane may wrap INSIDE the two-digit clock minutes —
			// "2:30pm" splits as "2:3\n0pm" — and the wrap must rejoin (drop,
			// not space) so parseClockTime sees "2:30pm" rather than "2:3
			// 0pm", which it rejects (minutes need 2 contiguous digits and
			// the disjoint "0pm" reads hour 0 < 1). The rejoin is gated on
			// the pre-run being preceded by a colon (the minutes context),
			// so a day+clock boundary ("Nov 6\n9am", no colon) still stays a
			// space even though the post-run is followed by [ap]m. Same EST
			// instant as the unwrapped "2:30pm" form.
			name:    "5h: minutes wrap inside their digits",
			content: "Claude usage limit reached. Your limit will reset at 2:3\n0pm (America/New_York).",
			want:    time.Date(2026, 1, 2, 14, 30, 0, 0, est).UTC(), // 2:30pm EST -> 19:30Z
		},
		{
			// A narrow pane may wrap BETWEEN a one-digit day and a
			// one-digit clock hour whose concatenation is a valid hour
			// — "Nov 1\n2pm" (joined "12" ≤ 12) — and the wrap must NOT
			// rejoin the digits: "Nov 12pm" would let monthDay consume
			// "12" as the day and parseClockTime read "12pm" as the
			// clock, scheduling the reset on Nov 12 at noon instead of
			// Nov 1 at 2pm. The wrap stays a space so monthDay sees
			// "Nov 1" (day 1) and parseClockTime sees "2pm" (hour 2).
			// Run length alone cannot identify this split (day+clock is
			// 1+1 = 2 digits, byte-identical to a mid-token clock hour
			// "1\n1am"); the month name preceding the day is what marks
			// the day+clock boundary. Same EST instant as the unwrapped
			// "Nov 1 2pm" form.
			name:    "weekly date: day wraps before a low clock hour (no comma)",
			content: "Claude usage limit reached. Your limit will reset at Nov 1\n2pm (America/New_York).",
			want:    time.Date(2026, 11, 1, 14, 0, 0, 0, est).UTC(), // Nov 1 2pm EST -> 19:00Z
		},
		{
			// A narrow pane may wrap INSIDE a weekday name — "Thursday"
			// splits as "Th\nursday" — and the wrap must rejoin the
			// letters (drop, not space) so the weekday regex matches
			// "Thursday" rather than "Th ursday" (which misses the
			// weekday and falls back to time-only, scheduling the reset
			// days early). Same EST instant as the unwrapped "Thursday
			// at 9am" form.
			name:    "weekly weekday: weekday wraps inside its letters",
			content: "Claude usage limit reached. Your limit will reset Th\nursday at 9am (America/New_York).",
			want:    time.Date(2026, 1, 8, 9, 0, 0, 0, est).UTC(), // Thu Jan 8 2026 9am EST -> 14:00Z
		},
		{
			// A sufficiently narrow pane may wrap BOTH before the
			// timezone and before the trailing period — "2pm
			// (America/New_York)\n." with the paren itself on the next
			// row — and the anchor must still capture the paren (the
			// account zone) and the period (so the evidence matches the
			// unwrapped render). The next-row paren alternative accepts a
			// wrapped trailing period (")\n.") in addition to a same-row
			// period (")."). Same EST instant as the unwrapped "2pm
			// (America/New_York)." form.
			name:    "5h wrapped: tz paren and trailing period each on their own row",
			content: "Claude usage limit reached. Your limit will reset at 2pm\n(America/New_York)\n.",
			want:    want2pm,
		},
		{
			// A sufficiently narrow pane may hard-wrap inside the "reset"
			// keyword itself — "re\nset at 2pm (America/New_York)." —
			// and the anchor must still locate the clause (tolerating a
			// wrap inside the leading keyword) and the normalizer must
			// rejoin it so the evidence matches the unwrapped "reset at
			// 2pm (…)." form. Same EST instant as the unwrapped form.
			name:    "5h wrapped: keyword wraps inside its letters",
			content: "Claude usage limit reached. Your limit will re\nset at 2pm (America/New_York).",
			want:    want2pm,
		},
		{
			// A narrow pane may wrap BETWEEN the clock's final digit and
			// its [ap]m meridiem suffix — "2:30pm" splits as "2:30\npm" —
			// and the wrap must rejoin (drop, not space) so the clockTime
			// regex sees "2:30pm" (the [ap] and m must be contiguous)
			// rather than "2:30 pm", which diverges the evidence from the
			// unwrapped form. Same EST instant as the unwrapped "2:30pm"
			// form.
			name:    "5h: meridiem wraps after the clock digits",
			content: "Claude usage limit reached. Your limit will reset at 2:30\npm (America/New_York).",
			want:    time.Date(2026, 1, 2, 14, 30, 0, 0, est).UTC(), // 2:30pm EST -> 19:30Z
		},
		{
			// A narrow pane may wrap INSIDE an ordinal date suffix —
			// "12th" splits as "12t\nh" — and the wrap must rejoin the
			// letters (drop, not space) so monthDay sees "Nov 12th, 2027"
			// (the suffix immediately follows the day, so the explicit
			// year matches) rather than "Nov 12t h, 2027", which matches
			// only "Nov 12" and discards the year. Same EST instant as the
			// unwrapped "Nov 12th, 2027 9am" form.
			name:    "weekly date: ordinal suffix wraps inside its letters",
			content: "Claude usage limit reached. Your limit will reset at Nov 12t\nh, 2027 9am (America/New_York).",
			want:    time.Date(2027, 11, 12, 9, 0, 0, 0, est).UTC(), // EST -> 14:00Z
		},
		{
			// A narrow pane may wrap BETWEEN a complete weekday name and
			// the following word — "Monday at" splits as "Monday\nat" —
			// and the wrap must NOT rejoin to "Mondayat": the joined token
			// is a weekday-shaped prefix but not a complete weekday name,
			// so the wrap stays a space and the evidence matches the
			// unwrapped "Monday at" render. The reset still parses to the
			// same EST instant as the unwrapped "Monday at 9am" form.
			name:    "weekly weekday: weekday wraps before the following word",
			content: "Claude usage limit reached. Your limit will reset Monday\nat 9am (America/New_York).",
			want:    time.Date(2026, 1, 5, 9, 0, 0, 0, est).UTC(), // Mon 9am EST -> 14:00Z
		},
		{
			// The production layout (task/testdata/claude_working_tool.ansi):
			// the reset clause sits under tmux's "  ⎿  " gutter and a wrapped
			// continuation row carries a 5-space indent. When the pane wraps
			// the timezone INSIDE the paren — "(America/\n     New_York)" —
			// the indent must be stripped on rejoin so time.LoadLocation still
			// accepts "America/New_York"; without the strip the zone rejects
			// and the clock falls back to the daemon host zone (UTC here),
			// landing the reset at 14:00Z instead of 19:00Z (the bug report's
			// exact symptom). Same EST instant as the unwrapped form.
			name:    "5h wrapped: tz split inside the paren with gutter indent",
			content: "⎿  Claude usage limit reached. Your limit will reset at 2pm (America/\n     New_York).",
			want:    want2pm,
		},
		{
			// Same production gutter+indent, but the paren opens on the next
			// row — "2pm\n     (America/New_York)." — the shipped wrapped-
			// banner shape with the continuation indent. The indent before
			// the paren is consumed by the anchor's next-row-paren alternative
			// ([ \t]* after \r?\n); the indent INSIDE the paren is stripped on
			// rejoin. Same EST instant as the unwrapped form.
			name:    "5h wrapped: tz paren on next row with gutter indent",
			content: "⎿  Claude usage limit reached. Your limit will reset at 2pm\n     (America/New_York).",
			want:    want2pm,
		},
		{
			// A narrow pane may wrap inside the "at" preposition — "at" splits
			// as "a\nt" — and the wrap must rejoin (drop, not space) so the
			// evidence matches the unwrapped "reset at 2pm (…)." form rather
			// than the divergent "reset a t 2pm (…)." (which would bypass the
			// same-wall dedupe after a pane resize). The anchor's optional
			// "at" branch does not cross a newline, so the wrapped "at" lands
			// inside the capture and the normalizer rejoins it. Same EST
			// instant as the unwrapped form.
			name:    "5h wrapped: at preposition wraps inside its letters",
			content: "Claude usage limit reached. Your limit will reset a\nt 2pm (America/New_York).",
			want:    want2pm,
		},
		{
			// A narrow pane may wrap BETWEEN a day digit and its ordinal
			// suffix — "Nov 12\nth, 2027 9am" — and the wrap must rejoin
			// (drop, not space) so monthDay sees "Nov 12th, 2027" (the
			// suffix immediately follows the day, so the explicit year
			// matches) rather than "Nov 12 th, 2027", which matches only
			// "Nov 12" and silently discards the year (scheduling the reset
			// a year early). Same EST instant as the unwrapped "Nov 12th,
			// 2027 9am" form.
			name:    "weekly date: ordinal suffix wraps after the day digits",
			content: "Claude usage limit reached. Your limit will reset at Nov 12\nth, 2027 9am (America/New_York).",
			want:    time.Date(2027, 11, 12, 9, 0, 0, 0, est).UTC(), // EST -> 14:00Z
		},
		{
			// A narrow pane may wrap AFTER a complete ordinal suffix and
			// before the date's comma — "Nov 12th\n, 2027 9am" — and the
			// wrap must rejoin (drop, not space) so monthDay sees "Nov 12th,
			// 2027" (the comma immediately follows the suffix, so the
			// explicit year matches) rather than "Nov 12th , 2027", which
			// matches only "Nov 12th" and silently discards the year. Same
			// EST instant as the unwrapped "Nov 12th, 2027 9am" form.
			name:    "weekly date: comma wraps after a complete ordinal suffix",
			content: "Claude usage limit reached. Your limit will reset at Nov 12th\n, 2027 9am (America/New_York).",
			want:    time.Date(2027, 11, 12, 9, 0, 0, 0, est).UTC(), // EST -> 14:00Z
		},
		{
			// The production gutter+indent layout applied to a wrapped
			// weekday reset — "reset Mon\n     day at 9am" — confirms the
			// outside-paren wrap rejoins (the weekday letters rejoin to
			// "Monday") even with the 5-space continuation indent. Same EST
			// instant as the unwrapped "Monday at 9am" form.
			name:    "weekly weekday: weekday wraps inside its letters with gutter indent",
			content: "⎿  Claude usage limit reached. Your limit will reset Mon\n     day at 9am (America/New_York).",
			want:    time.Date(2026, 1, 5, 9, 0, 0, 0, est).UTC(), // Mon 9am EST -> 14:00Z
		},
	}
	det := NewLimitDetector(nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hit, resetAt, hasReset, _ := det.Check(tc.content, tmux.ProgramClaude, now)
			if !hit || !hasReset {
				t.Fatalf("hit=%v hasReset=%v; the wrapped banner must still detect and parse", hit, hasReset)
			}
			if !resetAt.Equal(tc.want) {
				t.Fatalf("resetAt = %v, want %v (the wrapped form must resolve in the account's zone, not the daemon host's)", resetAt.UTC(), tc.want)
			}
			if !resetAt.After(now) {
				t.Fatalf("resetAt = %v must be after now = %v", resetAt, now)
			}
		})
	}
}

// TestClaudeResetWrappedBannerEvidenceStability pins the #6765 re-read identity
// for wrapped banners: re-reading an unchanged wrapped banner must reproduce the
// same evidence phrase, so the poll's same-phrase dedupe keeps the first (nearest
// to the wall) reset rather than re-parsing against a later clock. The evidence
// is the whitespace-normalized reset clause — deterministic for the same banner.
func TestClaudeResetWrappedBannerEvidenceStability(t *testing.T) {
	loc := nyLoc(t)
	now := time.Date(2026, 7, 4, 10, 0, 0, 0, loc)
	det := NewLimitDetector(nil)

	wrapped := "⎿  Claude usage limit reached. Your limit will reset at 2pm\n(America/New_York)."
	// Two reads of the same banner at different clocks must produce the same
	// evidence (the phrase is the re-read's identity) but may produce different
	// reset times (a time-only phrase re-resolves against the later clock).
	_, reset1, _, ev1 := det.Check(wrapped, tmux.ProgramClaude, now)
	_, reset2, _, ev2 := det.Check(wrapped, tmux.ProgramClaude, now.Add(time.Hour))
	if ev1 != ev2 {
		t.Fatalf("an unchanged wrapped banner must reproduce identical evidence: %q != %q", ev1, ev2)
	}
	if ev1 == "" {
		t.Fatalf("a wrapped banner with a parseable reset must carry evidence, got empty")
	}
	if !strings.Contains(ev1, "America/New_York") {
		t.Fatalf("evidence must contain the parenthesized timezone (the fix's whole point): %q", ev1)
	}
	if strings.Contains(ev1, "\n") {
		t.Fatalf("evidence must be whitespace-normalized (no raw newlines): %q", ev1)
	}
	// The reset time for a time-only phrase re-resolves against the later clock,
	// so the two reads differ — but the evidence (identity) is unchanged.
	if reset1.Equal(reset2) {
		// 2pm is after both 10:00 and 11:00, so both resolve to today 2pm —
		// they CAN be equal. Assert only that the evidence is stable, which
		// is the #6765 contract. (A phrase past the later clock would differ.)
	}

	// Trailing transcript/prompt rows after a clause that carries no trailing
	// period must NOT be swallowed into the capture: the evidence is the reset
	// clause alone, so it stays stable across re-reads even as the rows below
	// the banner change. The clause here has no terminating period (the shipped
	// banner form), and two trailing rows follow it in the same pane capture.
	trailing := "⎿  Claude usage limit reached. Your limit will reset at 2pm (America/New_York)\n" +
		"some transcript row\nanother changing row"
	_, _, _, evT := det.Check(trailing, tmux.ProgramClaude, now)
	if want := "reset at 2pm (America/New_York)"; evT != want {
		t.Fatalf("trailing rows must not be swallowed into the evidence: got %q, want %q", evT, want)
	}

	// A wrap between complete words ("reset\nat") is byte-identical to a
	// mid-token wrap at the newline, so the normalizer must not collapse it to
	// "resetat": the evidence must match the unwrapped render ("reset at …")
	// so a pane resize between reads does not change the identity phrase and
	// bypass the same-wall dedupe in SetLimitReachedAtEpoch.
	unwrappedAt := "Claude usage limit reached. Your limit will reset at 2pm (America/New_York)."
	wrappedAt := "Claude usage limit reached. Your limit will reset\nat 2pm (America/New_York)."
	_, _, _, evUnwrappedAt := det.Check(unwrappedAt, tmux.ProgramClaude, now)
	_, _, _, evWrappedAt := det.Check(wrappedAt, tmux.ProgramClaude, now)
	if evUnwrappedAt != evWrappedAt {
		t.Fatalf("a word-boundary wrap must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedAt, evWrappedAt)
	}
	if want := "reset at 2pm (America/New_York)."; evWrappedAt != want {
		t.Fatalf("wrapped word-boundary evidence = %q, want %q", evWrappedAt, want)
	}

	// A wrap that pushes the trailing period onto its own row — "2pm
	// (America/New_York)\n." — must produce the SAME evidence as the
	// unwrapped "2pm (America/New_York)." form: the period is sentence
	// punctuation, not a new word, so the wrap newline is dropped (not
	// spaced to "(…) ."). A divergent evidence would bypass the same-wall
	// dedupe in SetLimitReachedAtEpoch after a pane resize.
	unwrappedPeriod := "Claude usage limit reached. Your limit will reset at 2pm (America/New_York)."
	wrappedPeriod := "Claude usage limit reached. Your limit will reset at 2pm (America/New_York)\n."
	_, _, _, evUnwrappedPeriod := det.Check(unwrappedPeriod, tmux.ProgramClaude, now)
	_, _, _, evWrappedPeriod := det.Check(wrappedPeriod, tmux.ProgramClaude, now)
	if evUnwrappedPeriod != evWrappedPeriod {
		t.Fatalf("a wrapped trailing period must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedPeriod, evWrappedPeriod)
	}
	if want := "reset at 2pm (America/New_York)."; evWrappedPeriod != want {
		t.Fatalf("wrapped trailing-period evidence = %q, want %q", evWrappedPeriod, want)
	}

	// A wrap inside the [ap]m meridiem suffix — "2p\nm" — must produce the
	// SAME evidence as the unwrapped "2pm" form: the suffix letters rejoin
	// (not space to "2p m"), keeping the identity stable across resizes.
	wrappedMeridiem := "Claude usage limit reached. Your limit will reset at 2p\nm (America/New_York)."
	_, _, _, evWrappedMeridiem := det.Check(wrappedMeridiem, tmux.ProgramClaude, now)
	if evWrappedMeridiem != evUnwrappedPeriod {
		t.Fatalf("a wrapped meridiem suffix must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedPeriod, evWrappedMeridiem)
	}

	// A wrap before the date comma — "Nov 6\n, 2027 9am" — must produce the
	// SAME evidence as the unwrapped "Nov 6, 2027 9am" form: the comma
	// immediately follows the day (not "Nov 6 ,"), keeping the identity
	// stable and the explicit year matched.
	unwrappedComma := "Claude usage limit reached. Your limit will reset at Nov 6, 2027 9am (America/New_York)."
	wrappedComma := "Claude usage limit reached. Your limit will reset at Nov 6\n, 2027 9am (America/New_York)."
	_, _, _, evUnwrappedComma := det.Check(unwrappedComma, tmux.ProgramClaude, now)
	_, _, _, evWrappedComma := det.Check(wrappedComma, tmux.ProgramClaude, now)
	if evUnwrappedComma != evWrappedComma {
		t.Fatalf("a wrap before the date comma must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedComma, evWrappedComma)
	}

	// A wrap that pushes BOTH the timezone and the trailing period onto
	// their own rows — "2pm\n(America/New_York)\n." — must produce the
	// SAME evidence as the unwrapped "2pm (America/New_York)." form: the
	// paren and the period are both sentence punctuation, not new words,
	// so the wrap newlines are dropped (not spaced to "(…) ."). A
	// divergent evidence would bypass the same-wall dedupe in
	// SetLimitReachedAtEpoch after a pane resize.
	unwrappedTzPeriod := "Claude usage limit reached. Your limit will reset at 2pm (America/New_York)."
	wrappedTzPeriod := "Claude usage limit reached. Your limit will reset at 2pm\n(America/New_York)\n."
	_, _, _, evUnwrappedTzPeriod := det.Check(unwrappedTzPeriod, tmux.ProgramClaude, now)
	_, _, _, evWrappedTzPeriod := det.Check(wrappedTzPeriod, tmux.ProgramClaude, now)
	if evUnwrappedTzPeriod != evWrappedTzPeriod {
		t.Fatalf("a wrapped tz+period must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedTzPeriod, evWrappedTzPeriod)
	}
	if want := "reset at 2pm (America/New_York)."; evWrappedTzPeriod != want {
		t.Fatalf("wrapped tz+period evidence = %q, want %q", evWrappedTzPeriod, want)
	}

	// A wrap inside the "reset" keyword itself — "re\nset at 2pm (…)." —
	// must produce the SAME evidence as the unwrapped "reset at 2pm (…)."
	// form: the keyword letters rejoin (not space to "re set"), keeping
	// the identity stable across pane resizes.
	wrappedKeyword := "Claude usage limit reached. Your limit will re\nset at 2pm (America/New_York)."
	_, _, _, evWrappedKeyword := det.Check(wrappedKeyword, tmux.ProgramClaude, now)
	if evWrappedKeyword != evUnwrappedTzPeriod {
		t.Fatalf("a wrapped keyword must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedTzPeriod, evWrappedKeyword)
	}
	if want := "reset at 2pm (America/New_York)."; evWrappedKeyword != want {
		t.Fatalf("wrapped keyword evidence = %q, want %q", evWrappedKeyword, want)
	}

	// A wrap inside a weekday name — "Th\nursday at 9am (…)." — must
	// produce the SAME evidence as the unwrapped "Thursday at 9am (…)."
	// form: the weekday letters rejoin (not space to "Th ursday"),
	// keeping the identity stable across pane resizes.
	unwrappedWeekday := "Claude usage limit reached. Your limit will reset Thursday at 9am (America/New_York)."
	wrappedWeekday := "Claude usage limit reached. Your limit will reset Th\nursday at 9am (America/New_York)."
	_, _, _, evUnwrappedWeekday := det.Check(unwrappedWeekday, tmux.ProgramClaude, now)
	_, _, _, evWrappedWeekday := det.Check(wrappedWeekday, tmux.ProgramClaude, now)
	if evUnwrappedWeekday != evWrappedWeekday {
		t.Fatalf("a wrapped weekday must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedWeekday, evWrappedWeekday)
	}
	if want := "reset Thursday at 9am (America/New_York)."; evWrappedWeekday != want {
		t.Fatalf("wrapped weekday evidence = %q, want %q", evWrappedWeekday, want)
	}

	// A wrap between the clock's final digit and its [ap]m meridiem suffix —
	// "2:30\npm" — must produce the SAME evidence as the unwrapped "2:30pm"
	// form: the suffix rejoining the digits (not spacing to "2:30 pm")
	// keeps the identity stable across pane resizes.
	unwrappedMeridiemDigits := "Claude usage limit reached. Your limit will reset at 2:30pm (America/New_York)."
	wrappedMeridiemDigits := "Claude usage limit reached. Your limit will reset at 2:30\npm (America/New_York)."
	_, _, _, evUnwrappedMeridiemDigits := det.Check(unwrappedMeridiemDigits, tmux.ProgramClaude, now)
	_, _, _, evWrappedMeridiemDigits := det.Check(wrappedMeridiemDigits, tmux.ProgramClaude, now)
	if evUnwrappedMeridiemDigits != evWrappedMeridiemDigits {
		t.Fatalf("a wrapped meridiem-after-digits must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedMeridiemDigits, evWrappedMeridiemDigits)
	}
	if want := "reset at 2:30pm (America/New_York)."; evWrappedMeridiemDigits != want {
		t.Fatalf("wrapped meridiem-after-digits evidence = %q, want %q", evWrappedMeridiemDigits, want)
	}

	// A wrap inside an ordinal date suffix — "Nov 12t\nh, 2027 9am" — must
	// produce the SAME evidence as the unwrapped "Nov 12th, 2027 9am" form:
	// the suffix letters rejoin (not space to "12t h") so monthDay keeps the
	// explicit year and the identity stays stable across pane resizes.
	unwrappedOrdinal := "Claude usage limit reached. Your limit will reset at Nov 12th, 2027 9am (America/New_York)."
	wrappedOrdinal := "Claude usage limit reached. Your limit will reset at Nov 12t\nh, 2027 9am (America/New_York)."
	_, _, _, evUnwrappedOrdinal := det.Check(unwrappedOrdinal, tmux.ProgramClaude, now)
	_, _, _, evWrappedOrdinal := det.Check(wrappedOrdinal, tmux.ProgramClaude, now)
	if evUnwrappedOrdinal != evWrappedOrdinal {
		t.Fatalf("a wrapped ordinal suffix must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedOrdinal, evWrappedOrdinal)
	}
	if want := "reset at Nov 12th, 2027 9am (America/New_York)."; evWrappedOrdinal != want {
		t.Fatalf("wrapped ordinal-suffix evidence = %q, want %q", evWrappedOrdinal, want)
	}

	// A wrap between a complete weekday name and the following word —
	// "Monday\nat" — must NOT rejoin to "Mondayat": the joined token is a
	// weekday-shaped prefix but not a complete weekday name, so the wrap
	// stays a space and the evidence matches the unwrapped "Monday at"
	// render, keeping the identity stable across pane resizes.
	unwrappedWeekdayAt := "Claude usage limit reached. Your limit will reset Monday at 9am (America/New_York)."
	wrappedWeekdayAt := "Claude usage limit reached. Your limit will reset Monday\nat 9am (America/New_York)."
	_, _, _, evUnwrappedWeekdayAt := det.Check(unwrappedWeekdayAt, tmux.ProgramClaude, now)
	_, _, _, evWrappedWeekdayAt := det.Check(wrappedWeekdayAt, tmux.ProgramClaude, now)
	if evUnwrappedWeekdayAt != evWrappedWeekdayAt {
		t.Fatalf("a wrap after a complete weekday must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedWeekdayAt, evWrappedWeekdayAt)
	}
	if want := "reset Monday at 9am (America/New_York)."; evWrappedWeekdayAt != want {
		t.Fatalf("wrapped weekday+word evidence = %q, want %q", evWrappedWeekdayAt, want)
	}

	// The production gutter+indent layout: a wrap inside the tz paren that
	// carries the 5-space continuation indent — "(America/\n     New_York)"
	// — must produce the SAME evidence as the unwrapped "(America/New_York)"
	// form: the indent is stripped on rejoin (not spaced to
	// "(America/      New_York)"), keeping the identity stable and the zone
	// acceptable to time.LoadLocation across pane resizes.
	unwrappedIndent := "Claude usage limit reached. Your limit will reset at 2pm (America/New_York)."
	wrappedIndent := "⎿  Claude usage limit reached. Your limit will reset at 2pm (America/\n     New_York)."
	_, _, _, evUnwrappedIndent := det.Check(unwrappedIndent, tmux.ProgramClaude, now)
	_, _, _, evWrappedIndent := det.Check(wrappedIndent, tmux.ProgramClaude, now)
	if evUnwrappedIndent != evWrappedIndent {
		t.Fatalf("a wrapped tz paren with gutter indent must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedIndent, evWrappedIndent)
	}
	if want := "reset at 2pm (America/New_York)."; evWrappedIndent != want {
		t.Fatalf("wrapped tz-paren-indent evidence = %q, want %q", evWrappedIndent, want)
	}

	// A wrap inside the "at" preposition — "a\nt" — must produce the SAME
	// evidence as the unwrapped "reset at 2pm (…)." form: the preposition
	// letters rejoin (not space to "reset a t 2pm"), keeping the identity
	// stable across pane resizes.
	wrappedAtPrep := "Claude usage limit reached. Your limit will reset a\nt 2pm (America/New_York)."
	_, _, _, evWrappedAtPrep := det.Check(wrappedAtPrep, tmux.ProgramClaude, now)
	if evWrappedAtPrep != evUnwrappedIndent {
		t.Fatalf("a wrapped at preposition must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedIndent, evWrappedAtPrep)
	}
	if want := "reset at 2pm (America/New_York)."; evWrappedAtPrep != want {
		t.Fatalf("wrapped at-preposition evidence = %q, want %q", evWrappedAtPrep, want)
	}

	// A wrap between a day digit and its ordinal suffix — "Nov 12\nth" —
	// must produce the SAME evidence as the unwrapped "Nov 12th, 2027 9am"
	// form: the suffix rejoins the day (not space to "Nov 12 th") so
	// monthDay keeps the explicit year and the identity stays stable.
	wrappedDigitOrdinal := "Claude usage limit reached. Your limit will reset at Nov 12\nth, 2027 9am (America/New_York)."
	_, _, _, evWrappedDigitOrdinal := det.Check(wrappedDigitOrdinal, tmux.ProgramClaude, now)
	if evWrappedDigitOrdinal != evUnwrappedOrdinal {
		t.Fatalf("a wrapped digit+ordinal suffix must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedOrdinal, evWrappedDigitOrdinal)
	}
	if want := "reset at Nov 12th, 2027 9am (America/New_York)."; evWrappedDigitOrdinal != want {
		t.Fatalf("wrapped digit-ordinal evidence = %q, want %q", evWrappedDigitOrdinal, want)
	}

	// A wrap after a complete ordinal suffix and before the comma —
	// "Nov 12th\n, 2027" — must produce the SAME evidence as the unwrapped
	// "Nov 12th, 2027 9am" form: the comma rejoins the suffix (not space to
	// "Nov 12th , 2027") so monthDay keeps the explicit year.
	wrappedOrdinalComma := "Claude usage limit reached. Your limit will reset at Nov 12th\n, 2027 9am (America/New_York)."
	_, _, _, evWrappedOrdinalComma := det.Check(wrappedOrdinalComma, tmux.ProgramClaude, now)
	if evWrappedOrdinalComma != evUnwrappedOrdinal {
		t.Fatalf("a wrapped ordinal-suffix+comma must not change the evidence: unwrapped=%q wrapped=%q", evUnwrappedOrdinal, evWrappedOrdinalComma)
	}
	if want := "reset at Nov 12th, 2027 9am (America/New_York)."; evWrappedOrdinalComma != want {
		t.Fatalf("wrapped ordinal-suffix-comma evidence = %q, want %q", evWrappedOrdinalComma, want)
	}
}

// TestClaudeResetWrappedBannerNoTimezoneFallback confirms that a wrapped banner
// WITHOUT a parenthesized timezone still falls back to now's location — the
// documented behavior for a banner that genuinely carries no zone. The fix must
// not change this fallback; it only ensures the paren is not truncated out of
// the capture when it IS present.
func TestClaudeResetWrappedBannerNoTimezoneFallback(t *testing.T) {
	loc := nyLoc(t)
	now := time.Date(2026, 7, 4, 10, 0, 0, 0, loc)
	det := NewLimitDetector(nil)

	// A wrapped banner with no paren tz: the clock resolves in now's zone.
	// The wrap is between the clock time and the period — the same wrap point
	// that would truncate the paren in the buggy regex, but here there is no
	// paren to truncate.
	content := "Claude usage limit reached. Your limit will reset at 2:30pm\n."
	hit, resetAt, hasReset, _ := det.Check(content, tmux.ProgramClaude, now)
	if !hit || !hasReset {
		t.Fatalf("hit=%v hasReset=%v", hit, hasReset)
	}
	want := time.Date(2026, 7, 4, 14, 30, 0, 0, loc).UTC() // 18:30 UTC
	if !resetAt.Equal(want) {
		t.Fatalf("resetAt = %v, want %v (no-paren fallback to now's zone)", resetAt.UTC(), want)
	}
}

// TestClaudeResetUnrelatedClockRow pins the clause boundary of claudeResetAnchor:
// when an unparseable reset clause ends with a period-newline and a later pane
// row carries its own clock — e.g. "reset at some point soon.\nuser draft: meet
// Monday at 2pm" — the dot-all pre-clock span must not cross the completed
// clause and adopt the unrelated clock as the reset time. The detector still
// hits (the banner is present) but degrades to the documented hasResetTime=false
// so the daemon does not schedule auto-resume against transcript/prompt text.
func TestClaudeResetUnrelatedClockRow(t *testing.T) {
	loc := nyLoc(t)
	now := time.Date(2026, 7, 4, 10, 0, 0, 0, loc)
	det := NewLimitDetector(nil)

	cases := []struct {
		name    string
		content string
	}{
		{
			name:    "unrelated clock row after a period-terminated clause",
			content: "Claude usage limit reached. Your limit will reset at some point soon.\nuser draft: meet Monday at 2pm",
		},
		{
			// tmux `capture-pane -e` keeps ANSI SGR escapes, and the real
			// fixture shows "York).<ESC>[39m\n" at a styled line ending: an
			// unparseable colored clause ends with "soon.<ESC>[39m\n" and a
			// later row carries its own clock. A period immediately followed
			// by an ANSI escape byte is a styled line ending — i.e. a clause
			// boundary — so the span must stop at that period and NOT cross
			// into the later row's clock. (Without recognizing the escape the
			// inline-period branch would consume ".<ESC>" and then "[^.]" would
			// carry the capture across the newline into "2pm".)
			name:    "period-terminated styled clause then unrelated clock",
			content: "Claude usage limit reached. Your limit will reset at some point soon.\x1b[39m\nuser draft: meet Monday at 2pm",
		},
		{
			// An unparseable clause that merely ends with an abbreviated
			// month must NOT cross the row boundary into a later row's clock:
			// the month-abbreviation branch only consumes "Nov.\n" when the
			// next row begins with a digit (the day of a date continuation).
			// Here "Nov.\n" is followed by "user draft: … at 2pm", so the
			// branch fails, the period ends the clause, and the transcript's
			// "2pm" is NOT adopted as the reset time — degrades to the
			// documented hit=true, hasResetTime=false.
			name:    "period-terminated month-abbrev clause then unrelated clock",
			content: "Claude usage limit reached. Your limit will reset sometime in Nov.\nuser draft: meet at 2pm",
		},
		{
			// The boundary is the period+newline, not just any newline: a
			// wrap inside the reset clause (no period before the newline)
			// still crosses so the clock on the next row is captured.
			name:    "wrap before the clock is not a clause boundary",
			content: "Claude usage limit reached. Your limit will reset\nat 2pm (America/New_York).",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hit, _, hasReset, evidence := det.Check(tc.content, tmux.ProgramClaude, now)
			if !hit {
				t.Fatalf("must detect the banner: hit=%v", hit)
			}
			if strings.Contains(tc.name, "period-terminated") {
				if hasReset {
					t.Fatalf("an unrelated clock row must not yield a reset time: hasReset=%v evidence=%q", hasReset, evidence)
				}
				if evidence != "" {
					t.Fatalf("an unparseable clause must carry no evidence: %q", evidence)
				}
			} else {
				if !hasReset {
					t.Fatalf("a wrap inside the clause must still parse the clock: hasReset=%v", hasReset)
				}
			}
		})
	}
}
