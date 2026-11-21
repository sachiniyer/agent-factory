package tmux

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// codexDirectoryTrustWording is one shape of Codex's directory-trust modal as
// upstream paints it (codex-rs/tui/src/onboarding/trust_directory.rs). Every
// string is whitespace-stripped, so no match depends on the width the pane
// wrapped the modal at: upstream writes fixed sentences and ratatui wraps
// them, and a row's pieces are contiguous once whitespace is gone — the same
// observation compactCodexUpdatePane relies on.
type codexDirectoryTrustWording struct {
	// question is the modal's question prose as upstream writes it. The
	// complete-modal match requires it on screen so Enter is never sent on
	// the options chrome alone.
	question string
	// affirm is the modal's always-selected first option row, marker
	// included — "› 1. …" upstream (selection_option_row, then
	// picker_option_row, keep "›" for the highlighted row at every tag).
	affirm string
	// declines is the modal's second option row in each form it takes.
	declines []string
	// footers is the modal's footer row in each form it takes.
	footers []string
	// affirmative is the option af's Enter names as its choice, empty for a
	// wording af may only ever hold: the restricted variants offer no row
	// that grants trust, so a settled modal in one is held rather than
	// answered.
	affirmative string
}

// The 0.156.0+ modal's second option and footer each take one form per
// TrustCancelAction — "Quit"/" quit" on the startup path (the one af's
// create-path launch sees), "Back to Agent Command Center"/" back" navigating
// tasks, "Keep current directory"/" cancel" from rust-v0.160.0 on — and the
// Windows sandbox hint extends the footer to "enter continue and create
// sandbox · esc …" (the old modal's hint keeps its article: "… create a
// sandbox...").
var codexDirectoryTrustModalDeclines = []string{
	"2.Quit",
	"2.BacktoAgentCommandCenter",
	"2.Keepcurrentdirectory",
}

var codexDirectoryTrustModalFooters = []string{
	"entercontinue·escquit",
	"entercontinue·escback",
	"entercontinue·esccancel",
	"entercontinueandcreatesandbox·escquit",
	"entercontinueandcreatesandbox·escback",
	"entercontinueandcreatesandbox·esccancel",
}

// codexDirectoryTrustWordings is every directory-trust wording a supported
// Codex release paints, checked in upstream
// codex-rs/tui/src/onboarding/trust_directory.rs at every stable tag from
// rust-v0.144.6 through rust-v0.171.0: the first wording covers
// rust-v0.144.6–rust-v0.155.x, the rest rust-v0.156.0 on.
var codexDirectoryTrustWordings = []codexDirectoryTrustWording{
	{
		// rust-v0.144.6 through rust-v0.155.x: "> You are in <cwd>" then
		// "Do you trust the contents of this directory? Working with
		// untrusted contents comes with higher risk of prompt injection.
		// Trusting the directory allows project-local config, hooks, and exec
		// policies to load." then "› 1. Yes, continue" / "  2. No, quit" then
		// "Press enter to continue" — "… and create a sandbox..." under the
		// Windows hint.
		question:    "Doyoutrustthecontentsofthisdirectory?",
		affirm:      "›1.Yes,continue",
		declines:    []string{"2.No,quit"},
		footers:     []string{"Pressentertocontinue", "Pressentertocontinueandcreateasandbox..."},
		affirmative: codexDirectoryTrustAffirmative,
	},
	{
		// rust-v0.156.0 on, including rust-v0.171.0: "  Folder access" and
		// the cwd, then "Trust this folder? Codex can read, edit, and run
		// files here, subject to your permission settings. Folder settings
		// can run code automatically, even without a model request. Continue
		// only if you trust these files. Your trust decision will be saved."
		// then "› 1. Trust and continue" / "  2. Quit" then
		// "enter continue · esc quit".
		question:    "Trustthisfolder?",
		affirm:      "›1.Trustandcontinue",
		declines:    codexDirectoryTrustModalDeclines,
		footers:     codexDirectoryTrustModalFooters,
		affirmative: codexDirectoryTrustNewAffirmative,
	},
	{
		// The same modal on a folder the user marked untrusted: "Config,
		// hooks, and exec policies from untrusted folders stay disabled. …"
		// with "› 1. Open restricted" selected, and on a resumed untrusted
		// task "This existing task may retain settings and history, …" with
		// "› 1. Open existing task". af only holds either one: no row grants
		// trust, so Enter is never sent.
		affirm:   "›1.Openrestricted",
		declines: codexDirectoryTrustModalDeclines,
		footers:  codexDirectoryTrustModalFooters,
	},
	{
		affirm:   "›1.Openexistingtask",
		declines: codexDirectoryTrustModalDeclines,
		footers:  codexDirectoryTrustModalFooters,
	},
}

// codexTrustFooterInside reports whether rest contains a complete footer
// anywhere but at its end. A settled tail may end on the footer (the pane
// lost the question), and an error paragraph may carry any text before it —
// but a footer followed by more content means the modal finished painting
// and output continued: quoted transcript, not a live tail. Content after a
// footer match is output only when it cannot still be the remainder of a
// longer footer that shares the match as a prefix — "… and create a
// sandbox..." mid-paint, or already whole at the end.
func codexTrustFooterInside(rest string, footers []string) bool {
	for i := 0; i < len(rest); i++ {
		for _, f := range footers {
			if !strings.HasPrefix(rest[i:], f) {
				continue
			}
			rem := rest[i+len(f):]
			if rem == "" || codexTrustFooterCanContinue(rem, f, footers) {
				continue
			}
			return true
		}
	}
	return false
}

// codexTrustFooterCanContinue reports whether rem could still become the
// rest of a longer footer that has f as a prefix — the tail of the sandbox
// footer still painting, not output after a finished row.
func codexTrustFooterCanContinue(rem, f string, footers []string) bool {
	for _, g := range footers {
		if len(g) > len(f) && strings.HasPrefix(g, f) && strings.HasPrefix(g[len(f):], rem) {
			return true
		}
	}
	return false
}

// codexTrustLine is one non-blank capture line in the compact pane: where
// its stripped text sits in flat, which line of the normalized capture it
// came from, and how wide that line was painted. The painted width is what
// tells a real wrap — a row split because the pane ran out of room — from a
// split no pane width produces (#7735).
type codexTrustLine struct {
	start   int
	end     int
	src     int
	painted int
	budget  bool
}

// codexTrustPane is the pane compacted for matching plus the per-line facts
// the row matches need back: flat is each non-blank line's whitespace-stripped
// text concatenated, starts the flat offset at which each line's text begins,
// and width the wrap budget the matcher derives from the modal's own region —
// the widest painted line there that carries a breakable point, so its length
// is evidence of the width paragraphs wrapped at rather than of an
// unbreakable token's own length. A line that is one token — an overlong
// path, a config name, an option's continuation word — overflows where a
// wrap would split it, and counting it would read a wider pane than the
// wraps on screen (#7735's error variant carries "features.fast_mode=true"
// twenty-five cells wide on a twelve-column pane). And the region matters:
// the capture is taken with -J, which rejoins wrapped output into lines
// wider than the pane, so evidence above the modal would inflate the budget
// and unexplain the wraps a real narrow modal paints.
type codexTrustPane struct {
	flat   string
	starts []int
	lines  []codexTrustLine
	width  int
}

// compactCodexTrustPane is the pane with each non-blank line's whitespace
// stripped and the results concatenated, plus the offset at which each
// line's text begins in the concatenation. A row that wraps at one width and
// not another is the same contiguous run either way, and a run's start offset
// still proves it began on its own line — the two facts the line-exact match
// relied on, kept without the width (#7724).
func compactCodexTrustPane(content string) codexTrustPane {
	var p codexTrustPane
	clean := normalizeCodexUpdatePane(content)
	var b strings.Builder
	for i, line := range strings.Split(clean, "\n") {
		stripped := codexStripWhitespace(line)
		if stripped == "" {
			continue
		}
		p.lines = append(p.lines, codexTrustLine{
			start: b.Len(), end: b.Len() + len(stripped), src: i,
			painted: utf8.RuneCountInString(line),
			budget:  strings.IndexFunc(strings.TrimSpace(line), unicode.IsSpace) >= 0,
		})
		p.starts = append(p.starts, b.Len())
		b.WriteString(stripped)
	}
	p.flat = b.String()
	return p
}

// codexStripWhitespace removes every whitespace rune from s but keeps bytes
// that do not decode: a capture can end mid-rune — one byte of the two-byte
// "·" the new modal's footer paints, for example — and strings.Map would
// already have flattened the byte to U+FFFD. Kept raw, the byte still works
// as a prefix of the glyph the modal was painting (it matches or it breaks
// the match, both of which are right); flattened it could never match.
func codexStripWhitespace(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteByte(s[i])
			i++
			continue
		}
		if !unicode.IsSpace(r) {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// lastCodexTrustRow returns the last offset in flat[:before] at which one of
// rows occupies whole lines — beginning at a line boundary and ending at
// another — or -1. A mid-line occurrence is prose, not the modal's row, the
// same distinction the old per-line exact match made. A row that spans lines
// is accepted only where wrapExplained says a narrow pane is what split it:
// upstream never paints the split a wider pane leaves.
func lastCodexTrustRow(p codexTrustPane, rows []string, before int) int {
	best := -1
	for _, row := range rows {
		for idx := strings.LastIndex(p.flat[:before], row); idx >= 0; idx = strings.LastIndex(p.flat[:idx], row) {
			if !slices.Contains(p.starts, idx) || !slices.Contains(p.starts, idx+len(row)) ||
				!p.wrapExplained(idx, idx+len(row)) {
				continue
			}
			if idx > best {
				best = idx
			}
			break
		}
	}
	return best
}

// wrapExplained reports whether flat[from:to] — already known to sit on whole
// line boundaries — is a row broken the way a narrow pane breaks it. The
// pieces of one painted row are consecutive capture lines, and their total
// painted footprint — first piece, then a space and each continuation
// hanging indent included — is wider than the widest budget-evidencing line
// on the pane: rejoined, the row could not have fit on one line, so a wrap
// explains the split. Pieces that would share a line inside that width are
// a split no pane width produces — the shape upstream never paints (#7735).
// A row on a single line has no break to explain.
func (p codexTrustPane) wrapExplained(from, to int) bool {
	i, ok := slices.BinarySearch(p.starts, from)
	if !ok {
		return false
	}
	j := len(p.lines)
	if to < len(p.flat) {
		if j, ok = slices.BinarySearch(p.starts, to); !ok {
			return false
		}
	}
	if j-i < 2 {
		return j == i+1
	}
	joined := p.lines[i].painted
	for k := i + 1; k < j; k++ {
		if p.lines[k].src != p.lines[k-1].src+1 {
			return false
		}
		joined += 1 + p.lines[k].painted
	}
	return joined > p.width
}

// lastCodexTrustQuestionIndex returns the flat offset of the question
// anchor's LAST line-anchored occurrence, or -1 — the same occurrence the
// line-exact predicate used, whose per-line scan kept the last matching
// row. Any occurrence is enough where a real modal puts its question, but
// only the last one tells a question row the modal never paints — repeated
// between the second option and the footer, for instance — apart from the
// one the modal paints where it belongs (#7735). It also marks where the
// modal's own region begins for the wrap budget.
func lastCodexTrustQuestionIndex(p codexTrustPane, question string) int {
	for idx := strings.LastIndex(p.flat, question); idx >= 0; idx = strings.LastIndex(p.flat[:idx], question) {
		if slices.Contains(p.starts, idx) {
			return idx
		}
	}
	return -1
}

// regionWidth returns the widest budget-evidencing line in lines[from:] —
// the wrap budget the modal's own rows imply. A modal's question paragraph
// and error text wrap at the pane's width, so their longest breakable
// fragment measures it; lines above the question are earlier output, which
// -J rejoins into single lines wider than the pane, and letting them set
// the budget would make a real modal's wraps look unexplained and leave a
// complete modal unanswered.
func (p codexTrustPane) regionWidth(from int) int {
	width := 0
	for _, l := range p.lines[from:] {
		if l.budget && l.painted > width {
			width = l.painted
		}
	}
	return width
}

// codexDirectoryTrustCompleteModal reports whether content shows a COMPLETE
// directory-trust modal af may answer — every row of a trust-granting
// wording, in order, with the footer the last content — and if so which
// wording, so the caller can name the choice it confirms. Restricted
// wordings ("Open restricted" / "Open existing task") deliberately do not
// match: no row of those modals grants trust, so the paint-hold below keeps
// them held instead of answering them.
//
// The match is the ordered anchor chain the old line-exact predicate kept —
// question before selected option before second option before footer, the
// footer last — run on the compact pane so the answer does not depend on
// how a narrow pane wrapped the rows (#7724). Intervening rows are tolerated
// exactly as before: an error paragraph Codex paints between the options and
// the footer does not hide the modal.
func codexDirectoryTrustCompleteModal(content string) *codexDirectoryTrustWording {
	p := compactCodexTrustPane(content)
	for wi := range codexDirectoryTrustWordings {
		w := &codexDirectoryTrustWordings[wi]
		if w.affirmative == "" {
			continue
		}
		qi := lastCodexTrustQuestionIndex(p, w.question)
		if qi < 0 {
			continue
		}
		// The wrap budget is the modal's own region: every line at or below
		// the question is a row the modal painted at the pane's width,
		// while output above it can rejoin wider than the pane under -J.
		line, _ := slices.BinarySearch(p.starts, qi)
		p.width = p.regionWidth(line)
		sy := lastCodexTrustRow(p, []string{w.affirm}, len(p.flat))
		if sy < 0 {
			continue
		}
		no := lastCodexTrustRow(p, w.declines, len(p.flat))
		if no <= sy {
			continue
		}
		fi := -1
		for _, footer := range w.footers {
			if !strings.HasSuffix(p.flat, footer) {
				continue
			}
			if idx := len(p.flat) - len(footer); idx > no && slices.Contains(p.starts, idx) &&
				p.wrapExplained(idx, len(p.flat)) {
				fi = idx
			}
		}
		if fi < 0 {
			continue
		}
		if qi < sy {
			return w
		}
	}
	return nil
}

// CodexTrustPromptPresent reports whether content shows a COMPLETE Codex
// directory-trust modal af may answer — the trust-granting variant, whole
// and settled, in either wording upstream paints:
//
//	Do you trust the contents of this directory?           (0.144.6–0.155.x)
//	› 1. Yes, continue
//	  2. No, quit
//	Press enter to continue
//
//	Folder access                                          (0.156.0–0.171.0)
//	Trust this folder? Codex can read, edit, and run files here, …
//	› 1. Trust and continue
//	  2. Quit
//	enter continue · esc quit
//
// This modal is on the config-agent delivery path before the real composer.
// Its selected option uses the SAME `›` glyph as Codex's composer, so the old
// readiness check treated it as ready, pasted the briefing into the modal, and
// used the trailing Enter to select the affirmative. Every tmux command
// succeeded while Codex recorded no user turn, leaving an empty composer by
// attach time (#2220).
//
// The match is anchored on the question, both option labels and the
// affordance, and no longer depends on the width the pane wrapped them at.
// CheckAndHandleTrustPrompt also runs on live-session polls, so a prose
// mention of one phrase must never inject Enter into a working agent.
func CodexTrustPromptPresent(content string) bool {
	return codexDirectoryTrustCompleteModal(content) != nil
}

// codexDirectoryTrustTrailingRows reports whether the pane's tail is a cut
// inside a directory-trust modal's option-and-footer rows: the last
// line-leading "›" row on screen, then nothing but a prefix of what the
// modal paints from there — the selected row partial or whole, the second
// option partial or whole, the blank between, or a partial footer — in
// either wording, at any width.
//
// Codex paints the modal top-down, so a capture taken mid-paint, before the
// remaining rows land, leaves exactly such a tail; so does a settled pane
// too short to have kept the question. The complete-modal match above
// declines every such frame, and the selected row's leading "›" glyph is
// also Codex's composer glyph, so isReadyContent's hasCodexComposerLine arm
// (task/runner.go) accepts the same frame — the create-path wedge this
// file's #3302 contract says must not be reported clear.
//
// The trailing rows alone are NOT proof a modal owns the pane. The dialog
// text can sit verbatim in quoted agent output above an active composer,
// whose visible terminal cursor separately marks the pane as composer-owned.
// CheckAndHandleTrustPrompt pairs this predicate with that hidden-cursor
// oracle — the same one the complete-modal branch above and
// inspectCodexSafetyPrompt both already rely on — before holding. The helper
// answers only the trailing-rows question; the caller marks a held frame
// only when the cursor is hidden or unreadable, and sends no key either way.
func codexDirectoryTrustTrailingRows(content string) bool {
	p := compactCodexTrustPane(content)
	tailStart := -1
	for idx := len(p.starts) - 1; idx >= 0; idx-- {
		if strings.HasPrefix(p.flat[p.starts[idx]:], "›") {
			tailStart = p.starts[idx]
			break
		}
	}
	if tailStart < 0 {
		return false
	}
	tail := p.flat[tailStart:]
	for _, w := range codexDirectoryTrustWordings {
		for _, decline := range w.declines {
			stream := w.affirm + decline
			if strings.HasPrefix(stream, tail) {
				return true
			}
			if !strings.HasPrefix(tail, stream) {
				continue
			}
			// Past the decline row upstream may paint rows the stream cannot
			// enumerate — the "Failed to set trust for …" paragraph a failed
			// trust write leaves (trust_directory.rs renders self.error
			// between the option rows and the footer; onboarding_screen.rs
			// fills it), then a partial footer. Anything else after a
			// complete footer is continued output, not a modal tail.
			if codexTrustFooterInside(tail[len(stream):], w.footers) {
				continue
			}
			return true
		}
	}
	return false
}

// codexDirectoryTrustQuestionBlocks is every question paragraph a
// directory-trust modal paints, whitespace-stripped — the first Paragraph
// trust_directory.rs's render_ref lays out, in each prose the widget's flags
// select: the old wording's trust question (rust-v0.144.6–0.155.x), the new
// "Trust this folder?" (rust-v0.156.0 on), the restricted variant in its two
// upstream phrasings, and the resumed-untrusted-task variant.
var codexDirectoryTrustQuestionBlocks = []string{
	codexStripWhitespace("Do you trust the contents of this directory? Working with untrusted contents comes with higher risk of prompt injection. Trusting the directory allows project-local config, hooks, and exec policies to load."),
	codexStripWhitespace("Trust this folder? Codex can read, edit, and run files here, subject to your permission settings. Folder settings can run code automatically, even without a model request. Continue only if you trust these files. Your trust decision will be saved."),
	codexStripWhitespace("This folder is in your untrusted list. Config, hooks, and exec policies from untrusted folders stay disabled. Your trust decision will be saved."),
	codexStripWhitespace("Config, hooks, and exec policies from untrusted folders stay disabled. Trusted project folders can still contribute settings. Skills still load, and tools follow your permission settings. Opening will not change saved trust."),
	codexStripWhitespace("This existing task may retain settings and history, including project configuration or hooks loaded while it was trusted. To use restricted settings, start a new task. The folder's trust setting will not change."),
}

// codexDirectoryTrustQuestionHeads is each block's first sentence, through
// its closing "?". A tail that is only a prefix of the first sentence — the
// question line alone, or part of it — is the prose fragment
// CodexTrustProseAloneInjectsNothing clears, not a modal-owned tail: the
// block has not painted far enough to tell it from quoted text.
var codexDirectoryTrustQuestionHeads = func() []string {
	heads := make([]string, len(codexDirectoryTrustQuestionBlocks))
	for i, q := range codexDirectoryTrustQuestionBlocks {
		if cut := strings.IndexAny(q, "?."); cut >= 0 {
			heads[i] = q[:cut+1]
		} else {
			heads[i] = q
		}
	}
	return heads
}()

// codexTrustHeadTailRows bounds the rows a modal's head may still be
// painting when a capture lands: "Folder access" or "> You are in <cwd>",
// the path beneath it, the "Note: You're in a subdirectory of a Git
// project. Trusting will apply to the repository root:" warning and the
// root it names — the head plus as many following rows as the snapshot set
// shows those four taking at any width before the question paragraph starts.
const codexTrustHeadTailRows = 7

// codexDirectoryTrustQuestionTail reports whether the pane's last content is
// a slice of a directory-trust modal's question block: the whole block, the
// rows of it painted so far, or the piece a settled pane too short to have
// kept the block's top still shows — in any wording, at any width. The
// modal's selected "›" row comes AFTER the question, so a pane ending inside
// the block reads no trailing-rows signal; an earlier "›" left on screen by
// prior output makes hasCodexComposerLine accept it anyway — the #7735
// wedge. The block's head rows count the same way: "Folder access" and the
// path, or "> You are in <cwd>", are the modal's first paint.
func codexDirectoryTrustQuestionTail(content string) bool {
	p := compactCodexTrustPane(content)
	for _, line := range p.lines {
		rest := p.flat[line.start:]
		for i, q := range codexDirectoryTrustQuestionBlocks {
			if strings.Contains(q, rest) &&
				!strings.HasPrefix(codexDirectoryTrustQuestionHeads[i], rest) {
				return true
			}
		}
	}
	for i, line := range p.lines {
		run := p.flat[line.start:line.end]
		if run != "Folderaccess" && !strings.HasPrefix(run, ">Youarein") {
			continue
		}
		if len(p.lines)-i > codexTrustHeadTailRows {
			continue
		}
		holds, block := true, false
		for _, l := range p.lines[i:] {
			seg := p.flat[l.start:l.end]
			if strings.HasPrefix(seg, "›") {
				holds = false
				break
			}
			isBlock := false
			for _, q := range codexDirectoryTrustQuestionBlocks {
				if strings.Contains(q, seg) {
					isBlock = true
					break
				}
			}
			if isBlock {
				block = true
			} else if block {
				holds = false
				break
			}
		}
		if holds {
			return true
		}
	}
	return false
}
