package ui

import (
	"strconv"
	"strings"
)

// ResolveTabJump answers "which tab did the user mean?" for the unbounded
// jump-to-tab prompt (#3021).
//
// It lives here, in a package whose tests run on a developer box, rather than in
// app/ where the wiring is: app's tests drive real tmux and a real daemon, so a
// resolver buried there could only be exercised in CI. The interesting behaviour is
// all in the matching rules, and rules deserve tests that can be run while writing
// them.
//
// names is the tab bar in display order, so the returned index is one-based to match
// what the digits do and what the bar shows. 0 means "no match", which the caller
// reports rather than guessing at — jumping to the wrong tab is worse than saying no.
//
// The rules, in order, and each earns its place:
//
//  1. A bare number is an ordinal. That is what the digits already mean, so typing
//     12 in the prompt has to mean the twelfth tab and nothing else — even if a tab
//     happens to be NAMED "12".
//  2. Exact name, case-insensitive. Unambiguous by construction when it hits.
//  3. Unique prefix, then unique substring. Prefix first because "s" should reach
//     "server" rather than refusing over "dns-server"; substring is what makes a long
//     generated name reachable by the memorable part in the middle.
//
// AMBIGUITY IS NOT A MATCH. Two tabs matching a prefix returns 0 rather than the
// first one: silently picking one teaches the user the prompt is unreliable, and the
// cost of saying no is one more keystroke.
func ResolveTabJump(query string, names []string) int {
	q := strings.TrimSpace(query)
	if q == "" {
		return 0
	}
	if n, err := strconv.Atoi(q); err == nil {
		if n >= 1 && n <= len(names) {
			return n
		}
		return 0 // an out-of-range ordinal is a typo, not a name to search for
	}
	lower := strings.ToLower(q)

	if hit := uniqueMatch(names, func(name string) bool {
		return strings.EqualFold(name, q)
	}); hit != 0 {
		return hit
	}
	if hit := uniqueMatch(names, func(name string) bool {
		return strings.HasPrefix(strings.ToLower(name), lower)
	}); hit != 0 {
		return hit
	}
	return uniqueMatch(names, func(name string) bool {
		return strings.Contains(strings.ToLower(name), lower)
	})
}

// uniqueMatch returns the one-based index of the ONLY name satisfying match, or 0
// when none or several do. The "or several" half is the point: see ResolveTabJump.
func uniqueMatch(names []string, match func(string) bool) int {
	found := 0
	for i, name := range names {
		if !match(name) {
			continue
		}
		if found != 0 {
			return 0 // ambiguous
		}
		found = i + 1
	}
	return found
}

// ResolveTabJumpCandidates counts how many tabs a query could mean, so a caller can
// tell "no such tab" from "be more specific" — two misses that want different next
// keystrokes from the user.
//
// Counts at the most permissive tier only when the stricter ones found nothing, so it
// agrees with ResolveTabJump about what a match is rather than inventing a second
// rule that could disagree with it.
func ResolveTabJumpCandidates(query string, names []string) int {
	q := strings.TrimSpace(query)
	if q == "" {
		return 0
	}
	if _, err := strconv.Atoi(q); err == nil {
		return 0 // an ordinal either resolves or is out of range; it is never ambiguous
	}
	lower := strings.ToLower(q)
	for _, match := range []func(string) bool{
		func(name string) bool { return strings.EqualFold(name, q) },
		func(name string) bool { return strings.HasPrefix(strings.ToLower(name), lower) },
		func(name string) bool { return strings.Contains(strings.ToLower(name), lower) },
	} {
		count := 0
		for _, name := range names {
			if match(name) {
				count++
			}
		}
		if count > 0 {
			return count
		}
	}
	return 0
}
