package bugreport

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Whole-token matching: what counts as one complete occurrence of a secret in
// otherwise-kept text, and how it is replaced.
//
// Split out of redact.go under the file-length limit (#1145). It is a unit rather
// than a leftover: three passes share it — session titles, usernames, and
// registered account labels — and they differ only in the ALPHABET that decides
// where a token ends (replaceToken's isTokenRune). Keeping the scan, the
// longest-first ordering and the marker guard in one place is what stops those
// three from drifting into three slightly different ideas of "a whole match".

// sortLongestFirst keeps redaction order in one place: replace longer secrets
// before their prefixes, with a lexical tie-break for deterministic output.
func sortLongestFirst(values []string) {
	sort.Slice(values, func(i, j int) bool {
		if len(values[i]) != len(values[j]) {
			return len(values[i]) > len(values[j])
		}
		return values[i] < values[j]
	})
}

// replaceBareTitle removes a title only when it occupies a complete text token.
// The legacy logger's raw %s form is delimited by surrounding prose/newlines, so
// this covers that representation without compiling single-line punctuation-only
// titles such as "." or "/" into an unbounded matcher that erases every period
// or path separator in the bundle. A multiline title is different: its exact,
// byte-identical cross-line sequence must be removed before a legacy line matcher
// can consume line one and strand the rest. Exact %q forms are handled above.
//
// A token boundary means start/end of text or a neighboring rune that is not a
// letter, number, combining mark, or underscore. Checking both edges regardless
// of the title's own first/last character handles titles such as "client[prod]"
// while refusing to match "." inside "1.2" or "/" inside "repo/path".
func replaceBareTitle(s, title string) string {
	return replaceBareToken(s, title, redactedMarker)
}

// replaceBareToken replaces token with marker only where token occupies a
// COMPLETE text token — start/end of text or a neighboring rune that is not a
// letter, number, mark, or underscore. It is the manual-boundary replacement both
// the title scrub and the username scrub use, because a `\b<token>\b` regex only
// anchors at word↔non-word transitions: a token that itself ENDS (or starts) in a
// non-word rune — a username like "test-", or a title like "client[prod]" — has no
// `\b` after its trailing "-", so `\b` never matches it and it leaks (#2533). This
// checks the actual neighboring runes instead, so "test-" is redacted in
// "test-/fix-login-bug" (the "/" is a non-word boundary) but not inside a larger
// word.
func replaceBareToken(s, token, marker string) string {
	return replaceToken(s, token, marker, isWordRune)
}

// replaceToken is replaceBareToken with the token's own ALPHABET as a parameter:
// isTokenRune reports whether a neighboring rune CONTINUES a token of this kind,
// and a match is taken only when neither neighbor does.
//
// The alphabet is a parameter because it is what makes a match a whole value
// rather than a fragment of a longer one, and the right alphabet differs by what
// is being replaced. Titles and usernames are free text with no alphabet of their
// own, so they use the general word-rune rule; an account label's alphabet is
// pinned by agentaccount.ValidateName, which is strictly larger, and using the
// word-rune rule for one would rewrite "work" inside a branch_prefix of
// "work-stuff" (#3871). Same scan, same marker guard, different edges.
func replaceToken(s, token, marker string, isTokenRune func(rune) bool) string {
	if strings.TrimSpace(token) == "" || (!containsWordRune(token) && !strings.ContainsAny(token, "\r\n")) {
		return s
	}
	var out strings.Builder
	scan, copied := 0, 0
	changed := false
	for scan <= len(s)-len(token) {
		rel := strings.Index(s[scan:], token)
		if rel < 0 {
			break
		}
		start := scan + rel
		end := start + len(token)
		if tokenBoundary(s, start, end, isTokenRune) && !insideRedactionMarker(s, start, end) {
			out.WriteString(s[copied:start])
			out.WriteString(marker)
			copied = end
			scan = end
			changed = true
			continue
		}
		// Advance one byte past this rejected occurrence. strings.Index remains
		// byte-based too, so this cannot skip a later exact byte sequence.
		scan = start + 1
	}
	if !changed {
		return s
	}
	out.WriteString(s[copied:])
	return out.String()
}

func tokenBoundary(s string, start, end int, isTokenRune func(rune) bool) bool {
	if start > 0 {
		r, _ := utf8.DecodeLastRuneInString(s[:start])
		if isTokenRune(r) {
			return false
		}
	}
	if end < len(s) {
		r, _ := utf8.DecodeRuneInString(s[end:])
		if isTokenRune(r) {
			return false
		}
	}
	return true
}

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r)
}

func containsWordRune(s string) bool {
	for _, r := range s {
		if isWordRune(r) {
			return true
		}
	}
	return false
}

// insideRedactionMarker keeps the title sanitizer idempotent when a legal title
// is itself "redacted", "secret", or another substring of a marker emitted by
// an earlier title. Such a match is already inside public replacement text; it
// must not grow the marker or destroy its recognizable shape.
func insideRedactionMarker(s string, start, end int) bool {
	for _, marker := range []string{redactedMarker, secretMarker, userMarker} {
		first := start - len(marker) + 1
		if first < 0 {
			first = 0
		}
		for candidate := first; candidate <= start; candidate++ {
			markerEnd := candidate + len(marker)
			if markerEnd >= end && markerEnd <= len(s) && s[candidate:markerEnd] == marker {
				return true
			}
		}
	}
	return false
}
