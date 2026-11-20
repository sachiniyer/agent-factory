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
	set := newNamedVariantSet(piece, b)
	for name := range names {
		if pieceVariantsCouldName(set, name, b) {
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
		if pieceVariantsCouldName(set, agent, b) {
			return true
		}
	}
	return false
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
	_, joiner := namedLauncherShellJoins[launcher]
	memoize := len(preserved) == 0 && b.namedWords != nil
	// A joiner's words land in the downstream shell as ONE string —
	// build it first so a shell name in any word's text judges the
	// text the shell actually gets, not the word in isolation.
	var joinedText string
	var literals []string
	if joiner {
		var joined strings.Builder
		literals = make([]string, len(words))
		for i, word := range words {
			literal, ok := literalWordBudgeted(word, b)
			if !ok {
				continue // the word verdict below refuses anyway
			}
			literals[i] = literal
			if joined.Len() > 0 {
				joined.WriteByte(' ')
			}
			joined.WriteString(literal)
		}
		joinedText = joined.String()
	}
	var tailFields []joinField
	last := len(words) - 1
	for i, word := range words {
		key := namedWordKey{word: word, launcher: launcher, last: i == last}
		var verdict namedWordVerdict
		seen := false
		if memoize {
			verdict, seen = b.namedWords[key]
		}
		if !seen {
			verdict = namedLauncherWordVerdict(word, i == last, decode, launcher, names, preserved, joinedText, b)
			if memoize {
				b.namedWords[key] = verdict
			}
		}
		if verdict.refuse {
			return true
		}
		literal := ""
		if joiner {
			literal = literals[i]
			// The downstream shell re-splits the joined tail, so
			// the re-split words are a reading of their own —
			// collected here and judged below.
			tailFields = append(tailFields, shellJoinFields(literal)...)
		} else {
			var ok bool
			literal, ok = literalWordBudgeted(word, b)
			if !ok {
				continue // the verdict above already refused
			}
		}
		for _, nested := range namedLauncherNamesOf(literal, b) {
			if namedNestedWordsRefuse(words[i+1:], nested, names, preserved, joiner, b) {
				return true
			}
		}
	}
	if joiner {
		if len(tailFields) > 0 {
			// The tail's first field is the downstream command
			// word — `watch '$x'` runs `sh -c '$x'`.
			tailFields[0].cmd = true
		}
		if namedNestedFieldsRefuse(tailFields, launcher, names, preserved, joinedText, b) {
			return true
		}
	}
	return false
}

// joinField is one re-split word of a shell-carried string plus the
// command-position flag its delimiter earned: cmd marks the field a
// downstream shell reads as a command word — the first field, or one
// whose preceding delimiter run holds a command operator (`;`, `&`,
// `|`, `(`, `)`, a backtick, a newline — a line break ends a command
// exactly as `;` does). Redirect operators (`<`, `>`) and plain
// whitespace do not start a command, so `2>$f` reads as operand
// fields. carried marks the field shell-carried — master's own shell
// rule applies to names inside it (`a | sh` feeds the shell the pipe
// produces); a preserved argv word is not carried.
type joinField struct {
	text    string
	cmd     bool
	carried bool
}

// shellJoinFields re-splits a shell-carried joined string the way the
// downstream shell splits it: whitespace and every operator character
// — < > | ; & ( ) and the newline — delimits words, so an option and
// its redirect (`-EncodedCommand</dev/null`) classify as the two words
// sh runs. Redirect targets and digits-before-redirect (`2>`, `&>`,
// `>>`) need no grammar — splitting on the characters alone
// over-approximates safely. Quoting and escapes are not re-parsed in
// this reading: each field's own verdict judges its raw AND normalized
// forms, so `-enc\<x` still lands `-enc` through the normalized form.
func shellJoinFields(s string) []joinField {
	var fields []joinField
	for i := 0; i < len(s); {
		d0 := i
		for i < len(s) && joinBreakByte(s[i]) {
			i++
		}
		delim := s[d0:i]
		start := i
		for i < len(s) && !joinBreakByte(s[i]) {
			i++
		}
		if start == i {
			continue
		}
		fields = append(fields, joinField{
			text:    s[start:i],
			cmd:     strings.ContainsAny(delim, ";&|()`\n"),
			carried: true,
		})
	}
	return fields
}

// joinBreakByte is the shellJoinFields delimiter set — whitespace plus
// the shell operator characters.
func joinBreakByte(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '<', '>', '|', ';', '&', '(', ')', '`':
		return true
	}
	return false
}

// namedLauncherNamesOf collects EVERY modelled launcher name text could
// spell — pieceCouldName on the literal AND its normalized form, the
// same lookup the agent name gets: `/usr/bin/pwsh`, `./pwsh`,
// `~/.local/bin/watch` match on the basename, `pw”sh` normalizes to
// pwsh, and `/usr/bin/pw[s]h` glob-matches it. A piece can spell several
// names — `nix-shell` is nix at a `-` boundary and nix-shell exactly,
// `pw[s]h` and `pw{,s}h` are pwsh alike — and EACH match applies its own
// rule to the text after the piece, so a benign first name never masks
// a later one's hidden input.
func namedLauncherNamesOf(text string, b *reachBudget) []string {
	var found []string
	for _, form := range [2]string{text, normalizeNamedLiteral(text)} {
		set := newNamedVariantSet(form, b)
		for name := range accountNamedLauncherNames {
			if slices.Contains(found, name) {
				continue
			}
			if pieceVariantsCouldName(set, name, b) {
				found = append(found, name)
			}
		}
	}
	return found
}

// namedNestedWordsRefuse literalizes each remaining word once (the
// shared memo — literalizing is the counted work) and applies the
// nested launcher's rule to them as pseudo-words. resplit is the
// OUTER launcher's join property: when the outer launcher hands its
// argv to a shell as one joined string, each literal re-splits into
// fields — one argv word carrying an option and its operand
// (`pwsh '-enc x'` under watch) is two words to the downstream shell.
func namedNestedWordsRefuse(words []*syntax.Word, launcher string, names, preserved map[string]struct{}, resplit bool, b *reachBudget) bool {
	if b == nil {
		b = newReachBudget()
	}
	if !b.spend(len(words)) {
		return true
	}
	fields := make([]joinField, 0, len(words))
	var joined strings.Builder
	for _, word := range words {
		literal, ok := literalWordBudgeted(word, b)
		if !ok {
			return true // an unreadable tail word is hidden input
		}
		if joined.Len() > 0 {
			joined.WriteByte(' ')
		}
		joined.WriteString(literal)
		if resplit {
			fields = append(fields, shellJoinFields(literal)...)
		} else {
			fields = append(fields, joinField{text: literal})
		}
	}
	return namedNestedFieldsRefuse(fields, launcher, names, preserved, joined.String(), b)
}

// namedNestedFieldsRefuse applies launcher's own rule to pseudo-words —
// text fields, not argv: a nested launcher inside a word's text (or a
// quoted command string) has no argv to preserve. Each field gets the
// same literal verdict a word gets, a field that is another modelled
// launcher's name recurses on the fields after it, and the shared
// budget bounds the nesting. A field's own cmd flag (a command
// operator in its delimiter run), an operator inside the previous
// field, and an assignment the previous field opened all carry the
// downstream command position into the per-field verdict. A field
// that is a shell's name at a command position applies master's own
// rule for that shell to the carried text — a pipeline ending `| sh`
// reads exactly as master's `sh` does. joined is that text: the one
// string the downstream shell parses, preserved so master's parse
// sees the delimiters the fields lost.
func namedNestedFieldsRefuse(fields []joinField, launcher string, names, preserved map[string]struct{}, joined string, b *reachBudget) bool {
	if b == nil {
		b = newReachBudget()
	}
	if !b.spend(len(fields)) {
		return true
	}
	_, decode := namedLauncherDecodesOperands[launcher]
	memoize := len(preserved) == 0 && b.namedLiterals != nil
	last := len(fields) - 1
	prevOp, prevAssign := false, false
	for i, field := range fields {
		atCmd := field.cmd || prevOp || prevAssign
		prevOp, prevAssign = false, false
		var verdict namedWordVerdict
		seen := false
		key := namedLiteralKey{literal: field.text, launcher: launcher, last: i == last, decode: decode, atCmd: atCmd, carried: field.carried, carriedText: joined}
		if memoize {
			verdict, seen = b.namedLiterals[key]
		}
		if !seen {
			verdict = namedLiteralVerdict(field.text, i == last, decode, launcher, names, preserved, atCmd, field.carried, joined, b)
			if memoize {
				b.namedLiterals[key] = verdict
			}
		}
		if verdict.refuse {
			return true
		}
		if atCmd && field.carried && pieceIsNamedShell(field.text) && namedShellTextRefuses(joined, names, b) {
			return true
		}
		if atCmd && isAssignField(field.text) {
			// `NAME=value` at a command slot is an assignment, not a
			// command — the next field still carries the position.
			prevAssign = true
		}
		if fieldEndsOperator(field.text) {
			prevOp = true
		}
		for _, nested := range namedLauncherNamesOf(field.text, b) {
			if namedNestedFieldsRefuse(fields[i+1:], nested, names, preserved, joined, b) {
				return true
			}
		}
	}
	return false
}

// isAssignField reports whether text spells `NAME=value` — an
// environment-name run before its first `=`. At a command position
// such a field is an assignment prefix; it does not consume the
// command slot.
func isAssignField(text string) bool {
	eq := strings.IndexByte(text, '=')
	return eq > 0 && envNamePrefixSpelling(text[:eq])
}

// fieldEndsOperator reports whether text ends on a shell command
// operator — a field that IS a run of operators (`&&`, `;`) or one
// carrying a trailing operator (`x;`) — so the field after it reads
// as a new command. `<` and `>` are redirects, not separators.
func fieldEndsOperator(text string) bool {
	return len(text) > 0 && strings.ContainsRune(";&|()`", rune(text[len(text)-1]))
}

// envNamePrefixSpelling reports whether text is an environment name
// spelling — [A-Za-z_][A-Za-z0-9_]*.
func envNamePrefixSpelling(text string) bool {
	if text == "" || (text[0] >= '0' && text[0] <= '9') {
		return false
	}
	for i := 0; i < len(text); i++ {
		if !isEnvNameByte(text[i]) {
			return false
		}
	}
	return true
}

// pieceCouldBeAssignment reports whether a command-position piece
// could spell `NAME` for a `NAME=value` prefix — the exact envname,
// any `{a,b}` alternative that is one, or a glob piece whose
// atom-stripped skeleton spells one (`FOO[d]` can produce `FOOd`).
// An expansion the budget cuts short is unprovable — it counts.
func pieceCouldBeAssignment(piece string, b *reachBudget) bool {
	if envNamePrefixSpelling(piece) {
		return true
	}
	if !strings.ContainsAny(piece, "{[*?") {
		return false
	}
	variants, finished := []string{piece}, true
	if strings.ContainsRune(piece, '{') {
		variants, finished = namedPieceVariants(piece, b)
	}
	if !finished {
		return true
	}
	for _, v := range variants {
		if envNamePrefixSpelling(globEnvSkeleton(v, b)) {
			return true
		}
	}
	return false
}

// globEnvSkeleton rewrites a variant to the envname it could produce:
// every glob atom — a `[…]` group or a bare `*`/`?` — can produce an
// envname character, so each stands in as `_`; the literal runes keep
// their places.
func globEnvSkeleton(v string, b *reachBudget) string {
	var out []byte
	for i := 0; i < len(v); {
		if v[i] == '[' {
			if end := globBracketEnd(v, i, b); end >= 0 {
				out = append(out, '_')
				i = end + 1
				continue
			}
			out = append(out, '_')
			i++
			continue
		}
		if v[i] == '*' || v[i] == '?' {
			out = append(out, '_')
			i++
			continue
		}
		out = append(out, v[i])
		i++
	}
	return string(out)
}

// namedLiteralKey is one pseudo-word's memoized verdict slot — the
// same shape as namedWordKey, keyed on text because nested scans
// produce fields, not word nodes. atCmd keys the field's command
// position — a `$`-carrying field refuses only there.
type namedLiteralKey struct {
	literal     string
	launcher    string
	last        bool
	decode      bool
	atCmd       bool
	carried     bool
	carriedText string
}

// namedLauncherWordVerdict classifies ONE tail word: no literal →
// refuse; a hidden-input spelling → refuse; every piece — the literal
// and its shell-normalized form, each split on the delimiter set —
// checked for the agent and protected names, with a `$`-carrying piece
// refusing at a command position (a name the text cannot see could be
// constructed); and a payload flag at the tail's end reads stdin. A
// preserved argv word's downstream position is unknowable — any slot
// could be the command — so its text enters at command position.
func namedLauncherWordVerdict(word *syntax.Word, last bool, decode bool, launcher string, names, preserved map[string]struct{}, joinedText string, b *reachBudget) namedWordVerdict {
	literal, ok := literalWordBudgeted(word, b)
	if !ok {
		return namedWordVerdict{refuse: true}
	}
	// A joiner hands its tail argv to a downstream shell as one
	// string — the word's text is shell-carried, so a shell name in
	// its pieces applies master's rule for that shell to joinedText.
	_, carried := namedLauncherShellJoins[launcher]
	return namedLiteralVerdict(literal, last, decode, launcher, names, preserved, true, carried, joinedText, b)
}

// extglobOpeners are the two-byte sequences that open a bash extglob
// group (and zsh's kshglob forms) — `@(`, `?(`, `*(`, `+(`, `!(`.
// A word holding one could resolve to the name at run time the way a
// `*` does, so it counts as dynamic construction rather than waiting
// for a `bash -O extglob` reading.
var extglobOpeners = []string{"@(", "?(", "*(", "+(", "!("}

// literalCarriesDynamic reports whether literal holds an extglob group
// — `@(`, `?(`, `*(`, `+(` or `!(` — a downstream shell enabled could
// collapse to a denied name. `$` and backtick text is not judged here:
// in carried shell text it constructs a name only at a command
// position, which the piece scan tracks (`echo $HOME` and `sh -c`
// read it the same way — as an operand).
func literalCarriesDynamic(literal string) bool {
	for _, opener := range extglobOpeners {
		if strings.Contains(literal, opener) {
			return true
		}
	}
	return false
}

// namedLiteralVerdict is the word verdict on already-literal text —
// the shared core for argv words and the pseudo-words a nested
// launcher scan splits out of a word's text. atCmd is the text's
// command position downstream: a preserved argv word could sit in any
// slot, a re-split field carries the flag its delimiter run earned.
// carried marks the text shell-carried — a joiner's argv word, a
// re-split field, or nested text — where a shell name at a command
// position applies master's own rule for that shell to carriedText,
// the one string the downstream shell parses.
func namedLiteralVerdict(literal string, last, decode bool, launcher string, names, preserved map[string]struct{}, atCmd, carried bool, carriedText string, b *reachBudget) namedWordVerdict {
	raw := literal
	if decode {
		decoded, decOK := gvariantDecode(literal)
		if !decOK {
			return namedWordVerdict{refuse: true}
		}
		literal = decoded
	}
	normalized := normalizeNamedLiteral(literal)
	// Every check runs over every reading of the word — the union
	// property covers classification, not just the name scans. `-en""c`
	// reads `-enc` to the downstream shell, so the hidden-input and
	// payload spellings classify the normalized form alongside the
	// raw one.
	verdict := namedWordVerdict{
		refuse: literalNamesHiddenInput(literal, launcher, b) ||
			literalNamesHiddenInput(normalized, launcher, b) ||
			(raw != literal && literalNamesHiddenInput(raw, launcher, b)),
		payload: literalSpellsTailPayload(literal, launcher, b) ||
			literalSpellsTailPayload(normalized, launcher, b) ||
			(raw != literal && literalSpellsTailPayload(raw, launcher, b)),
	}
	if verdict.refuse {
		return verdict
	}
	if literalCarriesDynamic(literal) || literalCarriesDynamic(normalized) ||
		(raw != literal && literalCarriesDynamic(raw)) {
		return namedWordVerdict{refuse: true}
	}
	// The literal and its normalized form are both judged: a
	// downstream shell removes quoting at every depth — `co""dex`
	// and `co\\\\dex` are both `codex` — so the piece rule sees
	// both the text and the flat-normalized over-approximation of
	// what the text becomes.
	if namedLauncherTextRefuses(literal, atCmd, carried, carriedText, names, preserved, b) {
		return namedWordVerdict{refuse: true}
	}
	if normalized != literal && namedLauncherTextRefuses(normalized, atCmd, carried, carriedText, names, preserved, b) {
		return namedWordVerdict{refuse: true}
	}
	if raw != literal && namedLauncherTextRefuses(raw, atCmd, carried, carriedText, names, preserved, b) {
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

// namedSpan is one queued text span for the piece scan — the whole
// text, or a closed bracket atom's interior — with the command
// position its first piece enters at. Atom interiors are judged from
// command position: a `[FOO=x]` atom could produce text a downstream
// reading runs, so the conservative slot applies.
type namedSpan struct {
	text     string
	atCmd    bool
	interior bool
}

// namedLauncherTextRefuses runs the piece rule over one text — the text
// itself plus every piece split on the delimiter set. A piece that IS
// a modelled launcher's name re-enters that launcher's rule on the
// text after it (nesting by name — `pwsh -EncodedCommand` inside a
// watch string gets pwsh's hidden-input spellings), recursively,
// inside the shared budget. atCmd is the text's command position — a
// piece carrying `$` constructs a name only where a command word can
// stand (the position `cd ~/src && $EDITOR .` gives `$EDITOR` and
// `echo $HOME` denies it), so the scan tracks the slot: start and
// every command operator put the next piece at the command word, a
// `NAME=` prefix yields it one piece later, and anything else reads
// operands. carried marks the text shell-carried — a joiner's argv
// word or nested text — where a shell name at a command position
// applies master's own rule for that shell to carriedText, the whole
// string the downstream shell parses (never the piece's own text —
// `sh` inside `sh -c "echo hi"` is proven, while `sh` alone is not).
func namedLauncherTextRefuses(text string, atCmd, carried bool, carriedText string, names, preserved map[string]struct{}, b *reachBudget) bool {
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
	// shared budget rather than the stack. An interior span is a glob
	// atom's character class, not command text — `[sh]` matches `s` or
	// `h`, it does not run `sh` — so the shell rule reads only the
	// text the atom belongs to, not the interior.
	spans := []namedSpan{{text: text, atCmd: atCmd}}
	for len(spans) > 0 {
		cur := spans[len(spans)-1]
		spans = spans[:len(spans)-1]
		s := cur.text
		if !b.spend(len(s) + 1) {
			return true
		}
		atCmd, value := cur.atCmd, false
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
						spans = append(spans, namedSpan{text: s[i+1 : end], atCmd: true, interior: true})
						i = end + 1
						continue
					} else if b.left < 0 {
						return true // the scan ran out of budget
					}
				}
				if s[i] == '{' {
					if end := namedBraceEnd(s, i); end >= 0 {
						// `{a,b}` is one expansion atom to the
						// piece reading too — a `;` inside it is
						// alternative text, not a separator
						// (`co{d,;}ex` still spells codex) — and
						// its interior is plain text the union
						// reading judges on its own span.
						spans = append(spans, namedSpan{text: s[i+1 : end], atCmd: true, interior: true})
						i = end + 1
						continue
					}
				}
				i++
			}
			piece := s[start:i]
			if piece == "" {
				break
			}
			pos := atCmd && !value
			if pos && strings.Contains(piece, "$") {
				// `$x` or `$(…)` at a command word is a constructed
				// program name — the same refusal `sh -c '$x'` gets.
				return true
			}
			if namedLauncherPieceRefuses(piece, names, preserved, b) {
				return true
			}
			if pos && carried && !cur.interior && pieceIsNamedShell(piece) && namedShellTextRefuses(carriedText, names, b) {
				return true
			}
			for _, nested := range namedLauncherNamesOf(piece, b) {
				var tail []joinField
				for _, f := range strings.Fields(s[i:]) {
					tail = append(tail, joinField{text: f, carried: true})
				}
				if namedNestedFieldsRefuse(tail, nested, names, preserved, s[i:], b) {
					return true
				}
			}
			// The break run after the piece decides the next slot:
			// a command operator starts a new command word, `NAME=`
			// at a command slot yields the slot to the value piece
			// and takes it back after, and anything else reads
			// operands.
			j := i
			for j < len(s) && namedPieceBreakByte(s[j]) {
				j++
			}
			breakAfter := s[i:j]
			switch {
			case strings.ContainsAny(breakAfter, ";&|()`\n"):
				atCmd, value = true, false
			case pos && pieceCouldBeAssignment(piece, b) && breakAfter == "=":
				// `NAME=` — the next piece is the assigned value;
				// the slot returns to command position after it.
				value = true
			case pos && pieceCouldBeAssignment(piece, b) && strings.HasPrefix(breakAfter, "="):
				// `NAME= x` — an empty assignment, so the command
				// slot survives to the next piece.
				atCmd, value = true, false
			case value && strings.HasPrefix(breakAfter, "="):
				// `NAME=a=b` — the value continues; still an operand.
			case value:
				// The assigned value was just consumed — the next
				// piece is a command word again.
				atCmd, value = true, false
			default:
				atCmd, value = false, false
			}
		}
	}
	return false
}

// pieceIsNamedShell reports whether piece names one of master's
// modelled shells — `sh`, `bash`, `dash`, `zsh` and the versioned and
// restricted forms shellBaseName covers. `filepath`/`command` argv
// reads the basename the way the shell does; quoting is judged by the
// piece's own readings before this call.
func pieceIsNamedShell(piece string) bool {
	_, ok := shellBaseName(filepath.Base(piece))
	return ok
}

// namedShellTextRefuses runs master's own account-environment judges —
// payload verdict, environment mutation and shell-feed checks — over
// the carried text a shell name stands in. The text is judged whole
// because master's judges are position-blind: a `| sh` anywhere in it
// feeds the shell exactly as the direct form does. Budget exhaustion
// fails closed like every other scan here.
func namedShellTextRefuses(text string, names map[string]struct{}, b *reachBudget) bool {
	if !b.spend(len(text)) {
		return true
	}
	return !accountEnvironmentGateProven(text, names, accountEnvironmentMaxPayloadDepth)
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
