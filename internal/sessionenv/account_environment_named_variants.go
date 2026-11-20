// Account-environment named launchers: the piece variant and POSIX
// glob machinery — how one tail piece is read under `{a,b}` brace
// alternatives and matched against a name when it carries bracket
// atoms. Kept beside account_environment_named_launchers.go: the
// text walk lives there, the pattern algebra lives here.
//
// The over-approximation: a `[…]` atom produces exactly one character,
// `*`/`?` are never read as name spellings (master admits `ls /tmp/*`),
// and `{x..y}` ranges become `[x-y]` atoms. An unfinished brace
// expansion is unprovable and counts as able to spell the name.

package sessionenv

import (
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// namedVariantSet is one piece's brace-alternative forms plus the data
// every per-name check reuses: each variant's produced length (one
// rune per literal, one per `[…]` atom) and its basename. Computing
// them once keeps a glob-carrying word's cost at O(piece) instead of
// O(names × piece).
type namedVariantSet struct {
	variants []string
	lens     []int
	baseLens []int
	bases    []string
	litPre   []string
	basePre  []string
	finished bool
}

// globLitPrefix returns the literal run before the first `[` atom —
// with `*`/`?` absent every atom produces exactly one character, so a
// produced word keeps that prefix byte-for-byte and a name without it
// cannot match.
func globLitPrefix(pattern string) string {
	if i := strings.IndexByte(pattern, '['); i >= 0 {
		return pattern[:i]
	}
	return pattern
}

func newNamedVariantSet(piece string, b *reachBudget) namedVariantSet {
	// Only `{` can produce alternatives — a piece without one is its own
	// variant and costs no expansion walk. This is the common case, so
	// literal text never touches the budget at all.
	variants, finished := []string{piece}, true
	if strings.ContainsRune(piece, '{') {
		variants, finished = namedPieceVariants(piece, b)
	}
	set := namedVariantSet{
		variants: variants,
		lens:     make([]int, len(variants)),
		baseLens: make([]int, len(variants)),
		bases:    make([]string, len(variants)),
		litPre:   make([]string, len(variants)),
		basePre:  make([]string, len(variants)),
		finished: finished,
	}
	for i, v := range variants {
		set.bases[i] = filepath.Base(v)
		if strings.ContainsRune(v, '[') && !strings.ContainsAny(v, "*?") {
			set.lens[i] = variantGlobLength(v, b)
			set.baseLens[i] = variantGlobLength(set.bases[i], b)
			set.litPre[i] = globLitPrefix(v)
			set.basePre[i] = globLitPrefix(set.bases[i])
		}
	}
	return set
}

// pieceCouldName reports whether a piece could resolve to name — the
// identifier-boundary check, the path basename, and (when the piece
// carries glob syntax) the over-approximated pattern match on both —
// under every `{a,b}` brace alternative: brace expansion runs before
// globbing on downstreams that do it, so `co{de,}x` can land `codex`.
// Agent names and launcher names share this helper so the two lookups
// cannot diverge.
func pieceCouldName(piece, name string, b *reachBudget) bool {
	return pieceVariantsCouldName(newNamedVariantSet(piece, b), name, b)
}

// pieceVariantsCouldName is the per-name half of pieceCouldName with
// the piece's brace alternatives computed once and shared — a denied
// set or a launcher table checks every name against the same variants
// rather than re-expanding per lookup. An unfinished expansion is
// unprovable and treated as able to spell the name.
func pieceVariantsCouldName(set namedVariantSet, name string, b *reachBudget) bool {
	if !set.finished {
		return true
	}
	for i, variant := range set.variants {
		if textNamesIdentifierBoundary(variant, name) ||
			set.bases[i] == name {
			return true
		}
		// Only a `[…]` bracket atom can still spell a name — the
		// literal letters around it decide the produced length, so a
		// `co[d]ex` spells codex while `f[0-9]0` and any bare `*`/`?`
		// wildcard spell nothing a name could be (master admits
		// `ls /tmp/*` too — a bare wildcard is not a name reading).
		if !strings.ContainsRune(variant, '[') {
			continue
		}
		if strings.ContainsAny(variant, "*?") {
			// `*`/`?` make the produced length unbounded — run the
			// over-approximating match without the length check.
			if globOverMatch(variant, name, b) ||
				globOverMatch(set.bases[i], name, b) {
				return true
			}
			continue
		}
		// Every atom produces exactly one character, so the produced
		// word's length is fixed: a variant that cannot reach the
		// name's length cannot spell it and costs no match. The
		// basename reading lengths its own form. The literal run
		// before the first atom survives verbatim — a name missing
		// that prefix fails without a matcher call.
		if (set.lens[i] == len(name) && strings.HasPrefix(name, set.litPre[i]) &&
			globOverMatch(variant, name, b)) ||
			(set.baseLens[i] == len(name) && strings.HasPrefix(name, set.basePre[i]) &&
				globOverMatch(set.bases[i], name, b)) {
			return true
		}
	}
	return false
}

// variantGlobLength counts the rune length every expansion of variant
// produces when it holds no `*`/`?` — one rune per literal rune, one
// per `[…]` atom.
func variantGlobLength(variant string, b *reachBudget) int {
	n := 0
	for i := 0; i < len(variant); {
		if variant[i] == '[' {
			if end := globBracketEnd(variant, i, b); end >= 0 {
				n++
				i = end + 1
				continue
			}
		}
		_, w := utf8.DecodeRuneInString(variant[i:])
		i += w
		n++
	}
	return n
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
				// A `{x..y}` range produces one character per
				// element — a `[x-y]` atom is the same single
				// unknown character to the over-approximating
				// match, and keeps `co{d..z}ex` refused without
				// the bare-wildcard reading.
				lo, hi, _ := strings.Cut(content, "..")
				alts = []string{"[" + lo + "-" + hi + "]"}
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
	return globMatchCore(pattern, name, false, b)
}

// globCouldCarryPrefix reports whether pattern could produce a string
// that STARTS WITH prefix — the word glob expansion produces then
// carries its operand attached, the way a single-dash hidden option
// takes `-Eexpr`. Any leftover pattern atom produces some (possibly
// empty) suffix, so exhausting the name mid-pattern is a match.
func globCouldCarryPrefix(pattern, prefix string, b *reachBudget) bool {
	return globMatchCore(pattern, prefix, true, b)
}

// globMatchCore is the shared over-approximating matcher behind
// globOverMatch and globCouldCarryPrefix; prefixMode accepts the match
// when the name runs out with pattern atoms still to consume.
func globMatchCore(pattern, name string, prefixMode bool, b *reachBudget) bool {
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
			res = prefixMode
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
