package bugreport

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Whole-token matching: what counts as one complete occurrence of a sensitive
// value in otherwise-kept text. Span producers share these boundary rules so
// the union planner receives consistent candidates.
//
// Split out of redact.go under the file-length limit (#1145). It is a unit rather
// than a leftover: titles, usernames, and registered account labels differ only
// in the alphabet that decides where a token ends. Keeping their boundary and
// marker guards together prevents three subtly different ideas of a whole match.

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

// insideRedactionMarker keeps repeated planning idempotent when a registered
// value is itself "redacted", "repo", or another substring of an emitted
// marker. Such a match is already inside public replacement text; it must not
// grow the marker or destroy its recognizable shape.
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
	open := strings.LastIndex(s[:start+1], "[")
	if open >= 0 {
		if relClose := strings.IndexByte(s[end:], ']'); relClose >= 0 {
			marker := s[open : end+relClose+1]
			if marker == afHomeToken || numberedRootMarker(marker, "repo") || numberedRootMarker(marker, "worktree") {
				return true
			}
		}
	}
	return false
}

func numberedRootMarker(marker, role string) bool {
	prefix := "[" + role + ":"
	if !strings.HasPrefix(marker, prefix) || !strings.HasSuffix(marker, "]") {
		return false
	}
	number := marker[len(prefix) : len(marker)-1]
	if number == "" {
		return false
	}
	for _, r := range number {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
