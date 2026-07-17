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

// programListSite is one hardcoded copy of the agent enum in the web client.
type programListSite struct {
	File     string
	Programs []string
}

// webHardcodedProgramSites finds EVERY hardcoded agent list in the web client,
// not just the new-session modal's.
//
// There are two today — web/src/modals.ts:173 (new session) and
// web/src/tasks.ts:270 (the task form's program picker) — and checking one would
// leave the other free to drift while the audit reported green. That is the
// audit under-covering, which is worse than not auditing: it asserts parity over
// a selector it never opened. Any future copy is picked up automatically.
func webHardcodedProgramSites(t *testing.T) []programListSite {
	t.Helper()
	var out []programListSite
	for _, path := range webSourceFiles(t) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range webProgramListRe.FindAllStringSubmatch(string(b), -1) {
			var progs []string
			for _, q := range quotedRe.FindAllStringSubmatch(m[1], -1) {
				progs = append(progs, q[1])
			}
			if len(progs) == 0 {
				t.Errorf("%s: matched a program-list site but parsed zero values — the parser "+
					"is blind on it", relSite(t, path))
				continue
			}
			sort.Strings(progs)
			out = append(out, programListSite{File: relSite(t, path), Programs: progs})
		}
	}
	if len(out) < minProgramListSites {
		t.Fatalf("found %d hardcoded program-list sites in web/src (expected >= %d).\n\n"+
			"If the web now renders a list SERVED by the daemon, that is the fix (#1970) — "+
			"delete this check and flip enum.agent-programs to parity in parity/inventory.json. "+
			"If the sites were merely restructured, fix webProgramListRe: leaving it unmatched "+
			"would make the enum drift silent again, which is the whole failure this guards.",
			len(out), minProgramListSites)
	}
	return out
}

// minProgramListSites guards against a vacuous pass. Two copies exist today
// (modals.ts, tasks.ts); if the regex stops matching them the check must fail
// loudly rather than conclude the web hardcodes nothing.
const minProgramListSites = 2

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

	for _, site := range webHardcodedProgramSites(t) {
		if strings.Join(site.Programs, ",") == strings.Join(canonical, ",") {
			continue
		}
		t.Errorf("the web's agent list has drifted from tmux.SupportedPrograms.\n"+
			"  canonical (session/tmux/session.go:22): %v\n"+
			"  web       (%s):                         %v\n"+
			"  never offered by the web: %v\n"+
			"  offered by the web but unknown to af: %v\n\n"+
			"The web hardcodes a COPY of an enum the daemon owns, so adding an agent "+
			"server-side silently leaves the web unable to offer it. Sync every site to "+
			"unblock, but the real fix is to serve it (the ListBackends pattern) — see "+
			"enum.agent-programs in parity/inventory.json (#1970).",
			canonical, site.File, site.Programs, diff(canonical, site.Programs), diff(site.Programs, canonical))
	}
}
