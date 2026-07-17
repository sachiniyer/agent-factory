package parity

// Enum parity: the VALUES a surface offers for a field.
//
// A third drift dimension, under the other two. The verb check asks "can this
// surface do X?"; the field check asks "with which options?"; neither asks
// "and does it offer the same VALUES?".
//
// That gap is live. web/src/modals.ts:173 hardcodes
// ["claude","codex","aider","gemini","amp"] while session/tmux/session.go:22
// owns the canonical list — and the TUI (app/handle_input.go:121) and CLI
// (commands/root.go:246) both read the canonical one. Every existing check
// passes: the web DOES send `program`, so field coverage calls it covered. Add a
// sixth agent server-side and the web silently never offers it, with the whole
// suite green.
//
// It is the #1933 shape one level down — a surface quietly serving a stale copy
// of something the daemon owns — so it gets the same treatment: derive both
// sides and compare, rather than trusting a copy to stay in step.
//
// The fix for the underlying hazard is to serve the enum instead of copying it
// (the ListBackends pattern from #1968, tracked separately). Until that lands,
// this check makes the drift loud instead of silent.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// webProgramListRe matches the hardcoded agent list in the new-session modal:
//
//	for (const prog of ["claude", "codex", …]) {
//
// Anchored on `const prog of [` rather than a bare array so an unrelated string
// array cannot be mistaken for it. If the modal is rewritten (or, better, starts
// rendering a served list), this stops matching and the test says so instead of
// passing on an empty set.
var webProgramListRe = regexp.MustCompile(`const\s+prog\s+of\s+\[([^\]]*)\]`)

var quotedRe = regexp.MustCompile(`"([^"]+)"`)

// webHardcodedPrograms reads the program values the web modal offers.
func webHardcodedPrograms(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "web/src/modals.ts"))
	if err != nil {
		t.Fatalf("read modals.ts: %v", err)
	}
	m := webProgramListRe.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatalf("web/src/modals.ts no longer matches the hardcoded program-list pattern.\n\n" +
			"If the modal now renders a list SERVED by the daemon, that is the fix — delete " +
			"this check and flip enum.agent-programs to parity in parity/inventory.json. If it " +
			"was merely restructured, fix webProgramListRe: leaving it unmatched would make the " +
			"enum drift silent again.")
	}
	var out []string
	for _, q := range quotedRe.FindAllStringSubmatch(m[1], -1) {
		out = append(out, q[1])
	}
	if len(out) == 0 {
		t.Fatal("parsed zero programs from web/src/modals.ts — the parser is blind")
	}
	sort.Strings(out)
	return out
}

// TestWebProgramEnumMatchesWire fails when the web's hardcoded agent list drifts
// from the canonical one.
//
// This is deliberately NOT a "the web should serve this" assertion — that is
// enum.agent-programs' verdict to carry. It is the narrower, mechanical claim
// the copy makes implicitly and cannot keep on its own: that the copy is
// current.
func TestWebProgramEnumMatchesWire(t *testing.T) {
	canonical := append([]string(nil), tmux.SupportedPrograms...)
	sort.Strings(canonical)
	if len(canonical) == 0 {
		t.Fatal("tmux.SupportedPrograms is empty — the canonical source is unreadable")
	}

	web := webHardcodedPrograms(t)

	if strings.Join(web, ",") != strings.Join(canonical, ",") {
		missing := diff(canonical, web)
		extra := diff(web, canonical)
		t.Errorf("the web's agent list has drifted from tmux.SupportedPrograms.\n"+
			"  canonical (session/tmux/session.go:22): %v\n"+
			"  web       (web/src/modals.ts:173):      %v\n"+
			"  never offered by the web: %v\n"+
			"  offered by the web but unknown to af: %v\n\n"+
			"The web hardcodes a COPY of an enum the daemon owns, so adding an agent "+
			"server-side silently leaves the web unable to offer it. Sync the list to unblock, "+
			"but the real fix is to serve it (the ListBackends pattern) — see "+
			"enum.agent-programs in parity/inventory.json.",
			canonical, web, missing, extra)
	}
}
