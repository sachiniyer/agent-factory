package parity

// Enum parity: the VALUES a surface offers for a field.
//
// A third drift dimension, under the other two. The verb check asks "can this
// surface do X?"; the field check asks "with which options?"; neither asks
// "and does it offer the same VALUES?".
//
// That gap WAS live. web/src/modals.ts and web/src/tasks.ts each hardcoded a copy
// of the agent list while session/tmux/session.go owns the canonical one, and the
// TUI (app/handle_input.go) and CLI (commands/root.go) both read the canonical one.
// Every other check passed: the web DOES send `program`, so field coverage called
// it covered. Add a sixth agent server-side and the web silently never offers it,
// with the whole suite green.
//
// #1970 closed it structurally: the daemon SERVES the enum (POST /v1/ListPrograms,
// daemon/programs.go) and the web renders the response (web/src/programs.ts). There
// is no copy left to drift.
//
// So this file's job INVERTED. It used to assert "the copy is current" — a
// mitigation that let the copy exist as long as someone remembered to sync it. It
// now asserts the stronger thing: THERE IS NO COPY. A re-introduced hardcoded agent
// list is the bug itself, not a list to check, and it fails here the moment it
// appears rather than the day someone adds a seventh agent.
//
// Keeping a check here at all is deliberate. "Serve the enum" is a property of the
// code that nothing in the type system enforces: a future picker can hand-type six
// strings and work perfectly, today, exactly as these two did.

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// quotedArrayRe matches a bracketed run of quoted strings — the shape any
// hardcoded enum copy takes, whatever it is assigned to:
//
//	["claude", "codex", …]
//
// Deliberately NOT anchored on the old `const prog of [` form. That anchor was the
// weakness of the previous check: it recognized one spelling of the mistake, so a
// copy stored in a const, a default parameter, or a Set would have slipped past
// while the audit reported green.
var quotedArrayRe = regexp.MustCompile(`\[((?:\s*"[^"\n]*"\s*,?)+)\s*\]`)

var quotedRe = regexp.MustCompile(`"([^"]*)"`)

// stripComments blanks out TS comments so a PROSE mention of the old hardcoded
// list — including the one at the top of web/src/programs.ts, which quotes it to
// explain what was removed — is not mistaken for the list itself.
//
// It tracks string and template literals rather than blindly cutting at the first
// `//`, because a URL inside a string ("https://…") would otherwise truncate the
// rest of the line and hide a real copy sitting after it. Under-covering while
// reporting green is the failure this whole file exists to prevent, so the scanner
// errs toward scanning too much rather than too little.
func stripComments(src string) string {
	var out strings.Builder
	out.Grow(len(src))

	const (
		code = iota
		lineComment
		blockComment
		str
	)
	state := code
	var quote byte
	var escaped bool

	for i := 0; i < len(src); i++ {
		c := src[i]
		switch state {
		case code:
			switch {
			case c == '/' && i+1 < len(src) && src[i+1] == '/':
				state = lineComment
				i++
			case c == '/' && i+1 < len(src) && src[i+1] == '*':
				state = blockComment
				i++
			case c == '"' || c == '\'' || c == '`':
				state, quote, escaped = str, c, false
				out.WriteByte(c)
			default:
				out.WriteByte(c)
			}
		case lineComment:
			if c == '\n' {
				state = code
				out.WriteByte(c)
			}
		case blockComment:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				state = code
				i++
			} else if c == '\n' {
				// Keep newlines so reported positions stay meaningful.
				out.WriteByte(c)
			}
		case str:
			out.WriteByte(c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == quote:
				state = code
			}
		}
	}
	return out.String()
}

// hardcodedAgentEnums returns every array literal in src that lists two or more
// canonical agent names — the signature of a copied enum.
//
// TWO is the threshold, not one. A single agent name in an array is ordinary and
// legitimate (a default, a one-off fixture, an agent-specific branch); two or more
// together is a list of agents, which is the thing the daemon now owns. Naming the
// threshold here rather than leaving it implicit matters, because it is exactly the
// line between "this check under-covers" and "this check false-fires".
func hardcodedAgentEnums(src string) []string {
	canonical := map[string]bool{}
	for _, p := range tmux.SupportedPrograms {
		canonical[p] = true
	}

	var found []string
	for _, m := range quotedArrayRe.FindAllStringSubmatch(stripComments(src), -1) {
		var hits []string
		for _, q := range quotedRe.FindAllStringSubmatch(m[1], -1) {
			if canonical[q[1]] {
				hits = append(hits, q[1])
			}
		}
		if len(hits) >= 2 {
			sort.Strings(hits)
			found = append(found, strings.Join(hits, ","))
		}
	}
	return found
}

// TestWebHardcodesNoAgentEnum is the #1970 acceptance criterion as a test: adding
// an agent server-side must reach the web with NO edit under web/src.
//
// It proves that by proving the only thing that could break it is absent — a local
// list of agent names. The web's pickers build their options from ListPrograms
// (web/src/programs.ts), so the enum has exactly one owner.
//
// Test files are excluded (webSourceFiles already skips *.test.ts) and that
// exclusion is intentional, not an oversight: a test naming several agents is how
// you verify the serving path works — web/src/programs.test.ts hands programChoices
// a fake catalog full of agent names on purpose. Production code is where a name
// must not appear.
func TestWebHardcodesNoAgentEnum(t *testing.T) {
	if len(tmux.SupportedPrograms) < 2 {
		t.Fatalf("tmux.SupportedPrograms has %d entries — this check cannot detect a copy of a list that short", len(tmux.SupportedPrograms))
	}

	for _, path := range webSourceFiles(t) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, hit := range hardcodedAgentEnums(string(b)) {
			t.Errorf("%s hardcodes a copy of the agent enum: [%s]\n\n"+
				"The daemon owns this list (session/tmux/session.go) and serves it over "+
				"POST /v1/ListPrograms (daemon/programs.go). A copy here works perfectly "+
				"today and silently omits the NEXT agent someone adds, with every test "+
				"green — that is the #1970 bug, which this copy would reintroduce.\n\n"+
				"Build the options from the served catalog instead: programChoices() in "+
				"web/src/programs.ts, as web/src/modals.ts and web/src/tasks.ts do.",
				relSite(t, path), hit)
		}
	}
}

// TestAgentEnumDetectorIsNotVacuous is the guard on the guard.
//
// A negative check ("no copies exist") passes just as green when it has silently
// stopped being able to SEE a copy — a refactor, a new quoting style, a regex that
// no longer matches. That failure mode is worse than no check, because it reports
// coverage it does not have. So the detector is made to fire on a synthetic copy,
// in several shapes, every run.
func TestAgentEnumDetectorIsNotVacuous(t *testing.T) {
	first, second := tmux.SupportedPrograms[0], tmux.SupportedPrograms[1]

	for _, tc := range []struct {
		name string
		src  string
	}{
		{"the for-of loop this check originally caught", fmt.Sprintf(`for (const p of [%q, %q]) {}`, first, second)},
		{"a const, which the old anchored regex missed", fmt.Sprintf(`const AGENTS = [%q, %q];`, first, second)},
		{"a Set, likewise", fmt.Sprintf(`const AGENTS = new Set([%q, %q]);`, first, second)},
		{"a default parameter", fmt.Sprintf(`function f(agents = [%q, %q]) {}`, first, second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hardcodedAgentEnums(tc.src); len(got) == 0 {
				t.Errorf("the detector did not fire on a hardcoded copy: %s\n\n"+
					"TestWebHardcodesNoAgentEnum is therefore passing without being able to "+
					"see the thing it checks for. Fix quotedArrayRe/stripComments rather than "+
					"this test — a blind check is how the enum drift went silent the first time.",
					tc.src)
			}
		})
	}
}

// TestAgentEnumDetectorIgnoresProseAndSingletons pins the other half: the check
// must not false-fire, or the next person deletes it.
//
// The comment case is load-bearing and not hypothetical — web/src/programs.ts opens
// by quoting the exact array it replaced, to explain what was removed and why.
func TestAgentEnumDetectorIgnoresProseAndSingletons(t *testing.T) {
	first, second := tmux.SupportedPrograms[0], tmux.SupportedPrograms[1]

	for _, tc := range []struct {
		name string
		src  string
	}{
		{"a line comment describing the removed list", fmt.Sprintf(`// the web used to hardcode [%q, %q] here`, first, second)},
		{"a block comment doing the same", fmt.Sprintf("/* it hardcoded [%q, %q] */", first, second)},
		{"a single agent name, which is not a list", fmt.Sprintf(`const fallback = [%q];`, first)},
		{"an unrelated string array", `const modes = ["light", "dark"];`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hardcodedAgentEnums(tc.src); len(got) != 0 {
				t.Errorf("the detector false-fired on %s: %v\nsource: %s", tc.name, got, tc.src)
			}
		})
	}
}

// TestStripCommentsDoesNotTruncateAtAURL is the regression for the naive
// implementation of stripComments — cutting each line at the first "//".
//
// A URL inside a string literal contains "//", so a naive strip would discard the
// rest of that line, and a hardcoded enum sitting after it would become invisible.
// The check would report green while blind, which is precisely the failure
// TestAgentEnumDetectorIsNotVacuous exists to catch and this one localizes.
func TestStripCommentsDoesNotTruncateAtAURL(t *testing.T) {
	first, second := tmux.SupportedPrograms[0], tmux.SupportedPrograms[1]
	src := fmt.Sprintf(`const docs = "https://example.invalid/agents"; const AGENTS = [%q, %q];`, first, second)

	if got := hardcodedAgentEnums(src); len(got) == 0 {
		t.Errorf("a copy after a URL string was not seen — stripComments truncated the line at the URL's //\nsource: %s", src)
	}
}
