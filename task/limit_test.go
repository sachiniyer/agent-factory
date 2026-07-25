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
			hit, resetAt, hasReset := isLimitContent(tc.content, tc.agent, matchers, now)
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
	if hit, _, _ := isLimitContent(content, tmux.ProgramClaude, resolveLimitMatchers(nil), now); hit {
		t.Fatalf("built-in matcher unexpectedly detected reworded banner")
	}

	// With the override the reworded banner is detected, and the built-in
	// reset parser still resolves "reset at 3pm (America/New_York)".
	overrides := map[string]string{tmux.ProgramClaude: `PLAN LIMIT TRIPPED`}
	hit, resetAt, hasReset := isLimitContent(content, tmux.ProgramClaude, resolveLimitMatchers(overrides), now)
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
	if hit, _, _ := isLimitContent("the provider says: RATE CEILING HIT", tmux.ProgramDevin, withDevin, now); !hit {
		t.Fatalf("devin override pattern should detect the user-supplied banner")
	}
	// ...and the built-in default ("quota exhausted") no longer does.
	if hit, _, _ := isLimitContent("Quota exhausted: usage quota has been exhausted", tmux.ProgramDevin, withDevin, now); hit {
		t.Fatalf("devin override should REPLACE the built-in detect, not augment it")
	}

	// An uncompilable regex is dropped and the built-in default stands: the
	// stock claude banner must still detect.
	bad := resolveLimitMatchers(map[string]string{tmux.ProgramClaude: `(unclosed`})
	hit, _, _ := isLimitContent("Claude usage limit reached. Your limit will reset at 2pm (America/New_York)", tmux.ProgramClaude, bad, now)
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
	hit, resetAt, hasReset := det.Check("Claude usage limit reached. Your limit will reset at 2pm (America/New_York)", tmux.ProgramClaude, now)
	if !hit || !hasReset {
		t.Fatalf("Check should detect the stock claude banner with a reset time; got hit=%v hasReset=%v", hit, hasReset)
	}
	want := time.Date(2026, 7, 4, 14, 0, 0, 0, loc).UTC()
	if !resetAt.Equal(want) {
		t.Fatalf("resetAt = %v, want %v", resetAt.UTC(), want)
	}

	// gemini is API-key-metered — no matcher, so never a hit.
	if hit, _, _ := det.Check("429 RESOURCE_EXHAUSTED", tmux.ProgramGemini, now); hit {
		t.Fatalf("gemini must not produce a limit hit in v1")
	}

	// A config override on the detector is honored.
	overridden := NewLimitDetector(map[string]string{tmux.ProgramClaude: `PLAN LIMIT TRIPPED`})
	if hit, _, _ := overridden.Check("PLAN LIMIT TRIPPED at 3pm", tmux.ProgramClaude, now); !hit {
		t.Fatalf("override detector should detect the reworded banner")
	}
}
