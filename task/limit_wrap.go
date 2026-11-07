package task

import "strings"

// This file holds the wrap-classification helpers that normalizeResetWhitespace
// (in limit.go) consults to decide whether a letter-newline-letter or
// digit-newline-digit wrap rejoins (a mid-token split) or takes a space (a
// word/token boundary). They are split out of limit.go to keep that file under
// the structural-health line limit (#1145); they are pure functions over the
// already-built normalizer buffer and the unconsumed input, with no I/O.

// wrapSplitsWeekday reports whether a wrap newline between two ASCII letters
// splits a weekday-name token (a mid-token wrap) rather than falling between
// two complete words (a word-boundary wrap), mirroring wrapSplitsMonth. A
// narrow pane may wrap before the first three letters of a weekday are
// complete — "Th\nursday" (the wrapped "Thursday") — and the normalizer must
// rejoin it so the weekday regex matches "Thursday" rather than the spaced
// "Th ursday", which misses the weekday and falls back to time-only,
// scheduling the reset days early. The trailing letters already emitted (the
// run at the end of pre) and the leading letters still ahead (the run at the
// start of post) are rejoined and checked against the weekday abbreviations
// the weekday regex accepts. A weekday-shaped join rejoins; anything else
// takes a space, preserving the unwrapped evidence for the #6765 re-read
// identity.
func wrapSplitsWeekday(pre, post string) bool {
	j := len(pre)
	for j > 0 {
		c := pre[j-1]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			break
		}
		j--
	}
	head := pre[j:]
	if head == "" {
		return false
	}
	k := 0
	for k < len(post) {
		c := post[k]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			break
		}
		k++
	}
	tail := post[:k]
	if tail == "" {
		return false
	}
	joined := strings.ToLower(head + tail)
	for w := range weekdayIndex {
		if strings.HasPrefix(joined, w) {
			return true
		}
	}
	return false
}

// wrapSplitsResetKeyword reports whether a wrap newline between two ASCII
// letters splits the leading "reset" keyword of a Claude reset clause — e.g.
// "re\nset" (the wrapped "reset") — so the normalizer rejoins it rather than
// spacing to "re set", which the literal `reset` anchor cannot locate and the
// banner degrades to hit=true, hasResetTime=false. The shape is
// letter-newline-letter, byte-identical to a month/weekday mid-token wrap and
// to a word-boundary wrap, so the distinction is semantic: the trailing
// letters already emitted (the run at the end of pre) and the leading letters
// still ahead (the run at the start of post) rejoin to EXACTLY "reset" (the
// five-letter keyword). A word-boundary wrap ("reset\nat" → "resetat") does
// not equal "reset", so it takes a space and preserves the unwrapped evidence
// ("reset at") for the #6765 re-read identity; a mid-token wrap of a longer
// word ("re\nsetting" → "resetting") likewise does not equal "reset".
func wrapSplitsResetKeyword(pre, post string) bool {
	j := len(pre)
	for j > 0 {
		c := pre[j-1]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			break
		}
		j--
	}
	head := pre[j:]
	if head == "" {
		return false
	}
	k := 0
	for k < len(post) {
		c := post[k]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			break
		}
		k++
	}
	tail := post[:k]
	if tail == "" {
		return false
	}
	return strings.ToLower(head+tail) == "reset"
}

// preRunIsCalendarDay reports whether the digit run ending at a wrap newline
// (pre[preStart:]) is a calendar day preceded by a month name — the
// weekly-date banner grammar "Nov 6", including the abbreviated "Nov. 6"
// form. When the pre-run is a day, a wrap between it and the clock hour
// ("Nov 1\n2pm") is a day+clock boundary, not a mid-token clock-hour split,
// and must stay a space: rejoining to "Nov 12pm" would let monthDay consume
// "12" as the day and parseClockTime read "12pm" as the clock, scheduling the
// reset on the wrong day at the wrong hour. Run length alone cannot identify
// this split (day+clock is 1+1 = 2 digits, byte-identical to a mid-token
// clock hour "1\n1am"); the month name preceding the day is what marks it.
func preRunIsCalendarDay(pre string, preStart int) bool {
	j := preStart
	for j > 0 && (pre[j-1] == ' ' || pre[j-1] == '\t') {
		j--
	}
	// An abbreviated month keeps its period ("Nov. 6"); skip it.
	if j > 0 && pre[j-1] == '.' {
		j--
		for j > 0 && (pre[j-1] == ' ' || pre[j-1] == '\t') {
			j--
		}
	}
	k := j
	for k > 0 {
		c := pre[k-1]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			break
		}
		k--
	}
	word := strings.ToLower(pre[k:j])
	if len(word) < 3 {
		return false
	}
	_, ok := monthIndex[word[:3]]
	return ok
}
