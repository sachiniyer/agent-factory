package sessionenv

import (
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// This file models #7603's named launchers — the reset-class programs,
// command-string runners and environment writers the gate knows by name.
// Anything still unmodelled is judged exactly as master and named in the
// gate's stated limit (docs/usage-limits.md) — there is no generic scan
// over arbitrary commands.
//
// The rule (the design decision after six Codex rounds — the per-launcher
// option grammars and every-reading payloads are deleted) is closure by
// construction: the verdict never asks WHERE a launcher's command starts,
// so no finding of the form "option X's position, arity or value hides a
// command" can exist. For the tail of every launcher this file models,
// refuse when ANY of these holds:
//
//   - a literal piece names the account's agent program or a protected
//     variable at identifier boundaries — a piece is every argv word
//     and every piece of a word split on whitespace, `=`, `;`, `&`,
//     `|`, quotes and parentheses, and an agent name also matches as a
//     path basename (`'/opt/agent tools/codex'`, `co""dex`/`co\dex`
//     once the shell's quoting and escapes are normalized);
//   - a word cannot be read (no literal — a substitution or expansion)
//     or a literal carries command-substitution or parameter-expansion
//     text ($ or `) — a name the text cannot see could be constructed
//     at run time;
//   - a word spells input the gate cannot see — the per-launcher
//     hidden-input word table: a descriptor, stdin, or a language it
//     does not parse — the same refusal class `sh -s` gets. Commands
//     and config read from a FILE are the stated limit in
//     docs/usage-limits.md (`ssh -F`, `tmux -f`, a pwsh script path
//     admit the way `bash ./build.sh` does) — the gate judges the
//     visible command line.
//
// Shells keep #7610's exact `-c` judge — this rule only runs on the
// launchers named in accountNamedLauncherNames. Work is a single linear
// pass: each word literalizes once, each piece checks once, and a buried
// launcher reuses the shared reach budget's per-word verdicts.
//
// The accepted cost: a tail that merely MENTIONS a denied name refuses —
// `ssh host 'cat notes/codex.md'` or `tmux new-window 'vim codex.md'`
// name codex at a boundary inside an account tab, and a `$HOME` in a
// watch command is expansion text the gate cannot resolve. That
// over-refusal is measured against the corpus and stated in
// docs/usage-limits.md rather than predicted away.

// namedLauncherPieceBreaks are the delimiters a tail word is split on —
// the boundaries a launcher's own parsing can place a command or name
// at: whitespace, `=`, `;`, `&`, `|`, quotes and parentheses.
func namedLauncherPieceBreak(c rune) bool {
	return c < 128 && namedPieceBreakByte(byte(c))
}

// namedPieceBreakByte is the same delimiter set at byte level, for the
// offset scan in namedLauncherTextRefuses.
func namedPieceBreakByte(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '=', ';', '&', '|', '\'', '"', '`', '(', ')':
		return true
	}
	return false
}

// namedLauncherPieceRefuses applies the boundary rule to one piece: a
// protected variable named at identifier boundaries, or the account's
// own agent program — boundary text or path basename — with an agent
// whose config pin survives through `preserved` skipped (a still-pinned
// agent is not a re-root). A piece carrying POSIX pathname-expansion
// syntax (XCU 2.13 — `*`, `?`, `[…]`) is matched as a PATTERN too:
// `co[d]ex` resolves to the codex on disk at run time, so the text
// alone does not show the name. The whole piece and its path basename
// are both matched.
func namedLauncherPieceRefuses(piece string, names, preserved map[string]struct{}, b *reachBudget) bool {
	if piece == "" {
		return false
	}
	variants, finished := namedPieceVariants(piece, b)
	for name := range names {
		if pieceVariantsCouldName(variants, finished, name, b) {
			return true
		}
	}
	for agent, configVar := range accountConfigVars {
		if _, pinned := names[configVar]; !pinned {
			continue
		}
		if _, kept := preserved[configVar]; kept {
			continue
		}
		if pieceVariantsCouldName(variants, finished, agent, b) {
			return true
		}
	}
	return false
}

// pieceCouldName reports whether a piece could resolve to name — the
// identifier-boundary check, the path basename, and (when the piece
// carries glob syntax) the over-approximated pattern match on both —
// under every `{a,b}` brace alternative: brace expansion runs before
// globbing on downstreams that do it, so `co{de,}x` can land `codex`.
// Agent names and launcher names share this helper so the two lookups
// cannot diverge.
func pieceCouldName(piece, name string, b *reachBudget) bool {
	variants, finished := namedPieceVariants(piece, b)
	return pieceVariantsCouldName(variants, finished, name, b)
}

// pieceVariantsCouldName is the per-name half of pieceCouldName with
// the piece's brace alternatives computed once and shared — a denied
// set or a launcher table checks every name against the same variants
// rather than re-expanding per lookup. An unfinished expansion is
// unprovable and treated as able to spell the name.
func pieceVariantsCouldName(variants []string, finished bool, name string, b *reachBudget) bool {
	if !finished {
		return true
	}
	for _, variant := range variants {
		if textNamesIdentifierBoundary(variant, name) ||
			filepath.Base(variant) == name {
			return true
		}
		// The variant carries the glob flag — a `{x..y}` range
		// over-approximates to `*`, which the original piece never
		// spelled.
		if strings.ContainsAny(variant, "*?[") &&
			(globOverMatch(variant, name, b) ||
				globOverMatch(filepath.Base(variant), name, b)) {
			return true
		}
	}
	return false
}

// namedVariantCap bounds one piece's brace alternatives — expansion
// that cannot finish inside it leaves the piece unprovable.
const namedVariantCap = 64

// namedPieceVariants expands `{a,b}` brace alternatives — downstream
// shells that do brace expansion run it before globbing. A `{x..y}`
// range over-approximates to `*` (every element is one run); a brace
// group with no top-level comma or range is literal in the real
// expansion and stays one variant. Every variant is expanded to
// completion — consecutive groups like `co{d,x}{e,y}x` revisit the
// replaced slot until no alternative remains — and the second return
// is false when the variant cap or the shared budget cuts expansion
// short: every character the scans examine is charged to b, so nested
// groups cannot run quadratic outside the bound.
func namedPieceVariants(piece string, b *reachBudget) ([]string, bool) {
	if b == nil {
		b = newReachBudget()
	}
	variants := []string{piece}
	for i := 0; i < len(variants); i++ {
		v := variants[i]
		at, expanded := 0, false
		for at < len(v) {
			// Every character-level pass — the `{` search, the
			// matching-`}` scan, the alternatives split — is O(remaining
			// text) and is charged to the shared budget, so nested
			// groups cannot run quadratic outside the bound.
			if !b.spend(len(v) - at) {
				return variants, false
			}
			open := strings.IndexByte(v[at:], '{')
			if open < 0 {
				break
			}
			open += at
			close := namedBraceEnd(v, open)
			if close < 0 {
				// Unterminated `{` is literal, but a group
				// nested inside it can still expand.
				at = open + 1
				continue
			}
			content := v[open+1 : close]
			var alts []string
			if strings.Contains(content, "..") {
				alts = []string{"*"}
			} else if top := splitNamedBraceAlts(content); len(top) > 1 {
				alts = top
			}
			if alts == nil {
				// A comma-less group is literal in the real
				// expansion, but groups nested inside it are
				// not — descend past the `{` only.
				at = open + 1
				continue
			}
			variants[i] = v[:open] + alts[0] + v[close+1:]
			for _, alt := range alts[1:] {
				if len(variants) >= namedVariantCap {
					return variants, false
				}
				variants = append(variants, v[:open]+alt+v[close+1:])
			}
			expanded = true
			break
		}
		if !expanded {
			continue
		}
		// The replaced variant may still carry a group — re-examine
		// it before advancing so the first alternative expands too.
		i--
	}
	return variants, true
}

// namedBraceEnd returns the index of the `}` matching the `{` at i,
// nesting-aware, or -1.
func namedBraceEnd(s string, i int) int {
	depth := 0
	for j := i; j < len(s); j++ {
		switch s[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return -1
}

// splitNamedBraceAlts splits a brace group's content on top-level
// commas — commas inside nested groups are not alternatives here.
func splitNamedBraceAlts(s string) []string {
	var alts []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				alts = append(alts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(alts, s[start:])
}

// globOverMatch reports whether pattern — POSIX pathname expansion
// (XCU 2.13) OVER-APPROXIMATED, never path.Match semantics — could
// match name: `?` and every bracket expression `[…]` (classes, ranges
// and negation alike — the content is not read) each match ONE
// character, `*` matches any run, and `\<c>` matches its literal. An
// unterminated `[` matches everything: a pattern the matcher cannot
// read could be the name. `co[[:alpha:]]ex` therefore matches codex.
func globOverMatch(pattern, name string, b *reachBudget) bool {
	for i := 0; i < len(pattern); i++ {
		if pattern[i] != '[' {
			continue
		}
		end := globBracketEnd(pattern, i, b)
		if end < 0 {
			return true
		}
		i = end // a [ inside a closed expression is content, not a start
	}
	memo := map[[2]int]bool{}
	var match func(i, j int) bool
	match = func(i, j int) bool {
		key := [2]int{i, j}
		if res, ok := memo[key]; ok {
			return res
		}
		if !b.spend(1) {
			// The match could not finish inside the bound —
			// unprovable, so the pattern could be the name.
			memo[key] = true
			return true
		}
		var res bool
		switch {
		case i == len(pattern):
			res = j == len(name)
		case pattern[i] == '*':
			res = match(i+1, j) || (j < len(name) && match(i, j+1))
		case j == len(name):
			res = false
		case pattern[i] == '?' || pattern[i] == '[':
			next := i + 1
			if pattern[i] == '[' {
				end := globBracketEnd(pattern, i, b)
				if end < 0 {
					// Unreadable or unterminated — the atom
					// could be any character run.
					res = true
					break
				}
				next = end + 1
			}
			res = match(next, j+1)
		case pattern[i] == '\\' && i+1 < len(pattern):
			res = pattern[i+1] == name[j] && match(i+2, j+1)
		default:
			res = pattern[i] == name[j] && match(i+1, j+1)
		}
		memo[key] = res
		return res
	}
	return match(0, 0)
}

// globBracketEnd returns the index of the `]` closing the bracket
// expression that opens at i (pattern[i] == '['), or -1 when none
// does — or when the shared budget runs out mid-scan, which fails
// closed the same way. A leading `!` or `^` is negation, so a `]`
// right after it — or as the very first character — is literal. A
// POSIX `[[:class:]]`, `[[.coll.]]` or `[[=equiv=]]` inside is skipped
// whole so its `]` does not close the expression.
func globBracketEnd(pattern string, i int, b *reachBudget) int {
	j := i + 1
	if j < len(pattern) && (pattern[j] == '!' || pattern[j] == '^') {
		j++
	}
	if j < len(pattern) && pattern[j] == ']' {
		j++
	}
	for j < len(pattern) {
		if !b.spend(1) {
			return -1
		}
		if pattern[j] == '[' && j+1 < len(pattern) &&
			(pattern[j+1] == ':' || pattern[j+1] == '.' || pattern[j+1] == '=') {
			k := j + 2
			for k+1 < len(pattern) && !(pattern[k] == pattern[j+1] && pattern[k+1] == ']') {
				k++
			}
			if k+1 >= len(pattern) {
				return -1
			}
			j = k + 2
			continue
		}
		if pattern[j] == ']' {
			return j
		}
		j++
	}
	return -1
}

// namedWordKey is one word's memoized verdict slot — keyed on the word
// node, the launcher whose rule applies (hidden-input tables and the
// operand decode differ per name), and whether it ends the tail (a
// payload flag's stdin reading only exists at the end).
type namedWordKey struct {
	word     *syntax.Word
	launcher string
	last     bool
}

// namedWordVerdict is one word's classification under the closure rule:
// refuse is the word's own verdict; payload marks a command-string
// option spelling — one left at the tail's end reads its command from
// stdin.
type namedWordVerdict struct {
	refuse, payload bool
}

// namedLauncherTailRefuses applies the closure rule to a named
// launcher's tail words — one pass, each word judged once by
// namedLauncherWordVerdict, and a word that IS a modelled launcher's
// name re-enters that launcher's rule on the words after it (nesting
// by name — `watch pwsh -enc x` gets pwsh's hidden-input spellings).
// Verdicts are memoized per word on the shared reach budget when
// preserved is empty: a buried launcher's suffix judgements rescan
// the same tail words (ssh … ssh make), and without the memo that is
// a literalize per position per word — quadratic. Spending the shared
// budget down charges each call's scan size, so a pathological
// nesting still fails closed at the bound.
func namedLauncherTailRefuses(words []*syntax.Word, names, preserved map[string]struct{}, launcher string, b *reachBudget) bool {
	if b == nil {
		b = newReachBudget()
	}
	if !b.spend(len(words)) {
		return true
	}
	_, decode := namedLauncherDecodesOperands[launcher]
	memoize := len(preserved) == 0 && b.namedWords != nil
	last := len(words) - 1
	for i, word := range words {
		key := namedWordKey{word: word, launcher: launcher, last: i == last}
		var verdict namedWordVerdict
		seen := false
		if memoize {
			verdict, seen = b.namedWords[key]
		}
		if !seen {
			verdict = namedLauncherWordVerdict(word, i == last, decode, launcher, names, preserved, b)
			if memoize {
				b.namedWords[key] = verdict
			}
		}
		if verdict.refuse {
			return true
		}
		literal, ok := literalWordBudgeted(word, b)
		if !ok {
			continue // the verdict above already refused
		}
		for _, nested := range namedLauncherNamesOf(literal, b) {
			if namedNestedWordsRefuse(words[i+1:], nested, names, preserved, b) {
				return true
			}
		}
	}
	return false
}

// namedLauncherNamesOf collects EVERY modelled launcher name text could
// spell — pieceCouldName on the literal AND its normalized form, the
// same lookup the agent name gets: `/usr/bin/pwsh`, `./pwsh`,
// `~/.local/bin/watch` match on the basename, `pw”sh` normalizes to
// pwsh, and `/usr/bin/pw[s]h` glob-matches it. A piece can spell several
// names — `nix-shell` is nix at a `-` boundary and nix-shell exactly,
// `*sh` is pwsh and nix-shell alike — and EACH match applies its own
// rule to the text after the piece, so a benign first name never masks
// a later one's hidden input.
func namedLauncherNamesOf(text string, b *reachBudget) []string {
	var found []string
	for _, form := range [2]string{text, normalizeNamedLiteral(text)} {
		variants, finished := namedPieceVariants(form, b)
		for name := range accountNamedLauncherNames {
			if slices.Contains(found, name) {
				continue
			}
			if pieceVariantsCouldName(variants, finished, name, b) {
				found = append(found, name)
			}
		}
	}
	return found
}

// namedNestedWordsRefuse literalizes each remaining word once (the
// shared memo — literalizing is the counted work) and applies the
// nested launcher's rule to them as pseudo-words.
func namedNestedWordsRefuse(words []*syntax.Word, launcher string, names, preserved map[string]struct{}, b *reachBudget) bool {
	if b == nil {
		b = newReachBudget()
	}
	if !b.spend(len(words)) {
		return true
	}
	fields := make([]string, 0, len(words))
	for _, word := range words {
		literal, ok := literalWordBudgeted(word, b)
		if !ok {
			return true // an unreadable tail word is hidden input
		}
		fields = append(fields, literal)
	}
	return namedNestedFieldsRefuse(fields, launcher, names, preserved, b)
}

// namedNestedFieldsRefuse applies launcher's own rule to pseudo-words —
// text fields, not argv: a nested launcher inside a word's text (or a
// quoted command string) has no argv to preserve. Each field gets the
// same literal verdict a word gets, a field that is another modelled
// launcher's name recurses on the fields after it, and the shared
// budget bounds the nesting.
func namedNestedFieldsRefuse(fields []string, launcher string, names, preserved map[string]struct{}, b *reachBudget) bool {
	if b == nil {
		b = newReachBudget()
	}
	if !b.spend(len(fields)) {
		return true
	}
	_, decode := namedLauncherDecodesOperands[launcher]
	memoize := len(preserved) == 0 && b.namedLiterals != nil
	last := len(fields) - 1
	for i, field := range fields {
		var verdict namedWordVerdict
		seen := false
		key := namedLiteralKey{literal: field, launcher: launcher, last: i == last, decode: decode}
		if memoize {
			verdict, seen = b.namedLiterals[key]
		}
		if !seen {
			verdict = namedLiteralVerdict(field, i == last, decode, launcher, names, preserved, b)
			if memoize {
				b.namedLiterals[key] = verdict
			}
		}
		if verdict.refuse {
			return true
		}
		for _, nested := range namedLauncherNamesOf(field, b) {
			if namedNestedFieldsRefuse(fields[i+1:], nested, names, preserved, b) {
				return true
			}
		}
	}
	return false
}

// namedLiteralKey is one pseudo-word's memoized verdict slot — the
// same shape as namedWordKey, keyed on text because nested scans
// produce fields, not word nodes.
type namedLiteralKey struct {
	literal  string
	launcher string
	last     bool
	decode   bool
}

// namedLauncherWordVerdict classifies ONE tail word: no literal →
// refuse; a hidden-input spelling → refuse; `$` or backtick text →
// refuse (a name the text cannot see could be constructed); every piece
// — the literal and its shell-normalized form, each split on the
// delimiter set — checked for the agent and protected names; and a
// payload flag at the tail's end reads stdin.
func namedLauncherWordVerdict(word *syntax.Word, last bool, decode bool, launcher string, names, preserved map[string]struct{}, b *reachBudget) namedWordVerdict {
	literal, ok := literalWordBudgeted(word, b)
	if !ok {
		return namedWordVerdict{refuse: true}
	}
	return namedLiteralVerdict(literal, last, decode, launcher, names, preserved, b)
}

// namedLiteralVerdict is the word verdict on already-literal text —
// the shared core for argv words and the pseudo-words a nested
// launcher scan splits out of a word's text.
func namedLiteralVerdict(literal string, last, decode bool, launcher string, names, preserved map[string]struct{}, b *reachBudget) namedWordVerdict {
	raw := literal
	if decode {
		decoded, decOK := gvariantDecode(literal)
		if !decOK {
			return namedWordVerdict{refuse: true}
		}
		literal = decoded
	}
	verdict := namedWordVerdict{
		refuse:  literalNamesHiddenInput(literal, launcher),
		payload: literalSpellsTailPayload(literal, launcher),
	}
	if verdict.refuse {
		return verdict
	}
	if strings.ContainsAny(literal, "$`") {
		return namedWordVerdict{refuse: true}
	}
	// The literal and its normalized form are both judged: a
	// downstream shell removes quoting at every depth — `co""dex`
	// and `co\\\\dex` are both `codex` — so the piece rule sees
	// both the text and the flat-normalized over-approximation of
	// what the text becomes.
	if namedLauncherTextRefuses(literal, names, preserved, b) {
		return namedWordVerdict{refuse: true}
	}
	if normalized := normalizeNamedLiteral(literal); normalized != literal &&
		namedLauncherTextRefuses(normalized, names, preserved, b) {
		return namedWordVerdict{refuse: true}
	}
	if raw != literal && namedLauncherTextRefuses(raw, names, preserved, b) {
		// The decoded reading is added to the raw text's, never
		// substituted for it — a boundary the raw text shows is still
		// a boundary to refuse on.
		return namedWordVerdict{refuse: true}
	}
	if last && verdict.payload {
		// A payload flag with no operand reads its command from
		// stdin — the same hidden input.
		return namedWordVerdict{refuse: true}
	}
	return verdict
}

// normalizeNamedLiteral removes the quoting a downstream shell strips
// at ANY depth, in one flat pass: every run of backslashes goes, a line
// break (LF or CRLF) right after a run goes with it — sh's line
// continuation — and every quote character goes. The result is an
// over-approximation of POSIX escape resolution (XCU 2.2.1), closed
// under nesting: quoting quoted again reduces to the same text with no
// recursion and no fixpoint loop. Exact one-level resolution is
// depth-sensitive — inside double quotes `\\` is a literal backslash a
// deeper re-parse would still consume — so an escape can hide a name at
// a depth the exact rule misses; deleting the escapes themselves makes
// `co\\\\dex`, `co\\dex` and `co\dex` all read `codex`. Whitespace is
// kept, so `echo co dex` still admits.
func normalizeNamedLiteral(text string) string {
	if !strings.ContainsAny(text, "'\"`\\") {
		return text
	}
	var out strings.Builder
	out.Grow(len(text))
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\\':
			for i < len(text) && text[i] == '\\' {
				i++
			}
			switch {
			case i < len(text) && text[i] == '\n':
				i++
			case i+1 < len(text) && text[i] == '\r' && text[i+1] == '\n':
				i += 2
			}
			i-- // the loop's i++ lands past the run (and any line break)
		case '\'', '"', '`':
			// Quote characters vanish at every depth.
		default:
			out.WriteByte(text[i])
		}
	}
	return out.String()
}

// namedLauncherTextRefuses runs the piece rule over one text — the text
// itself plus every piece split on the delimiter set. A piece that IS
// a modelled launcher's name re-enters that launcher's rule on the
// text after it (nesting by name — `pwsh -EncodedCommand` inside a
// watch string gets pwsh's hidden-input spellings), recursively,
// inside the shared budget.
func namedLauncherTextRefuses(text string, names, preserved map[string]struct{}, b *reachBudget) bool {
	if b == nil {
		b = newReachBudget()
	}
	// The plain reading and every collapsed reading are judged as a
	// UNION — a reading never replaces the text it came from, so no
	// reading can hide a name another shows. A bracket expression is
	// one glob atom to the piece reading, but its interior is plain
	// text too: `[; pwsh -enc x ;]` carries the launcher the atom skip
	// would otherwise eat. Every closed atom's interior is queued as
	// its own span — iteratively, so pathological nesting burns the
	// shared budget rather than the stack.
	spans := []string{text}
	for len(spans) > 0 {
		s := spans[len(spans)-1]
		spans = spans[:len(spans)-1]
		if !b.spend(len(s) + 1) {
			return true
		}
		if namedLauncherPieceRefuses(s, names, preserved, b) {
			return true
		}
		for i := 0; i < len(s); {
			for i < len(s) && namedPieceBreakByte(s[i]) {
				i++
			}
			start := i
			for i < len(s) && !namedPieceBreakByte(s[i]) {
				if s[i] == '[' {
					if end := globBracketEnd(s, i, b); end >= 0 {
						// The glob reading keeps `[…]` as one
						// atom inside this piece — `['FOO=/o']`
						// is not split on `=` — and the plain
						// reading judges the interior on its
						// own span.
						spans = append(spans, s[i+1:end])
						i = end + 1
						continue
					} else if b.left < 0 {
						return true // the scan ran out of budget
					}
				}
				i++
			}
			piece := s[start:i]
			if piece == "" {
				break
			}
			if piece != s && namedLauncherPieceRefuses(piece, names, preserved, b) {
				return true
			}
			for _, nested := range namedLauncherNamesOf(piece, b) {
				if namedNestedFieldsRefuse(strings.Fields(s[i:]), nested, names, preserved, b) {
					return true
				}
			}
		}
	}
	return false
}

// unwrapNamedLauncher judges every #7603-named launcher under the one
// closure rule — the name selects the rule and its hidden-input table;
// option structure is never consulted.
func unwrapNamedLauncher(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool, launcher string) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand, memo) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	return nil, namedLauncherTailRefuses(words, names, nil, launcher, memo.reach)
}

// textNamesIdentifierBoundary reports whether name appears in text
// bounded on both sides by bytes that are not identifier bytes — the
// boundary `echo CODEX_HOME` satisfies and `CODEX_HOME_DIR` does not.
func textNamesIdentifierBoundary(text, name string) bool {
	for i := 0; i+len(name) <= len(text); i++ {
		if text[i:i+len(name)] != name {
			continue
		}
		if i > 0 && isEnvNameByte(text[i-1]) {
			continue
		}
		if i+len(name) < len(text) && isEnvNameByte(text[i+len(name)]) {
			continue
		}
		return true
	}
	return false
}
