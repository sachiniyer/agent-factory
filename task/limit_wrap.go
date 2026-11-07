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
// start of post) are rejoined and checked against the complete weekday names
// (full names plus the standard abbreviations the weekday regex accepts). A
// COMPLETE weekday name rejoins; a weekday-shaped prefix that is not a
// complete name ("Monday\nat" → "mondayat", which starts with "mon" but is
// not a weekday) takes a space, preserving the unwrapped evidence ("Monday
// at") for the #6765 re-read identity.
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
	return weekdayNameSet[strings.ToLower(head+tail)]
}

// weekdayNameSet is the set of complete weekday tokens (full names plus the
// standard abbreviations the weekday regex accepts) that wrapSplitsWeekday
// rejoins a mid-token wrap to. Membership is exact so a weekday-shaped prefix
// that is not itself a weekday ("mondayat", the word-boundary wrap
// "Monday\nat") does not rejoin and the unwrapped evidence is preserved.
var weekdayNameSet = map[string]bool{
	"sun": true, "sunday": true,
	"mon": true, "monday": true,
	"tue": true, "tues": true, "tuesday": true,
	"wed": true, "wednesday": true,
	"thu": true, "thur": true, "thurs": true, "thursday": true,
	"fri": true, "friday": true,
	"sat": true, "saturday": true,
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

// wrapSplitsClockDigitMeridiem reports whether a wrap newline sits between a
// clock's final digit and its [ap]m meridiem suffix — "2:30\npm" or "2\npm" —
// so the normalizer rejoins it (drop, not space) and the clockTime regex sees
// "2:30pm"/"2pm" (the [ap] and m must be contiguous) rather than "2:30 pm"/
// "2 pm", which diverges the evidence from the unwrapped form and breaks the
// #6765 re-read identity across pane resizes. The pre run must end in a clock
// digit (lastDigit at the newline) and post must begin with a complete [ap]m
// token whose 'm' ends the suffix — a following letter means a longer word
// such as "amazing", a word-boundary wrap that stays a space. A wrap INSIDE
// the suffix ("2p\nm", letter-newline-letter) is handled separately by
// wrapSplitsClockSuffix.
func wrapSplitsClockDigitMeridiem(pre, post string) bool {
	if len(pre) == 0 {
		return false
	}
	last := pre[len(pre)-1]
	if last < '0' || last > '9' {
		return false
	}
	if len(post) < 2 {
		return false
	}
	switch post[0] {
	case 'a', 'A', 'p', 'P':
	default:
		return false
	}
	if post[1] != 'm' && post[1] != 'M' {
		return false
	}
	if len(post) >= 3 && (post[2] >= 'a' && post[2] <= 'z' || post[2] >= 'A' && post[2] <= 'Z') {
		return false
	}
	return true
}

// wrapSplitsOrdinalSuffix reports whether a wrap newline between two ASCII
// letters splits the two-letter ordinal suffix of a calendar day — "st",
// "nd", "rd", or "th" — e.g. "Nov 12t\nh" (the wrapped "Nov 12th") — so the
// normalizer rejoins it rather than spacing to "12t h", which monthDay
// matches only as the day "12" and silently drops the explicit year (the
// optional suffix must immediately follow the day digits). The shape is
// letter-newline-letter, byte-identical to a month/weekday mid-token wrap and
// to a word-boundary wrap, so the distinction is semantic: the trailing
// letter run of pre is exactly the first letter of an ordinal suffix
// ({s,n,r,t}) that immediately follows a day digit, and the leading letter
// run of post completes the two-letter suffix. The digit-before-head marks
// an ordinal context uniquely inside a reset clause (a clock-suffix wrap
// "2p\nm" is letter-newline-letter too, but its head is [ap] not in
// {s,n,r,t}); anything else takes a space, preserving the unwrapped evidence
// for the #6765 re-read identity.
func wrapSplitsOrdinalSuffix(pre, post string) bool {
	j := len(pre)
	for j > 0 {
		c := pre[j-1]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			break
		}
		j--
	}
	head := pre[j:]
	if len(head) != 1 {
		return false
	}
	switch head[0] {
	case 's', 'S', 'n', 'N', 'r', 'R', 't', 'T':
	default:
		return false
	}
	// The suffix's first letter must immediately follow a day digit.
	if j == 0 {
		return false
	}
	prev := pre[j-1]
	if prev < '0' || prev > '9' {
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
	if len(tail) != 1 {
		return false
	}
	switch strings.ToLower(head + tail) {
	case "st", "nd", "rd", "th":
		return true
	}
	return false
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

// wrapSplitsMonth reports whether a wrap newline between two ASCII letters
// splits a month-name token (a mid-token wrap) rather than falling between two
// complete words (a word-boundary wrap). The two shapes are byte-identical at
// the newline — "No\nv" (mid-token, the wrapped month "Nov") and "reset\nat"
// (word boundary) both present as letter-newline-letter with no surrounding
// spaces — so the distinction is semantic: the trailing letters already
// emitted (the run at the end of pre) and the leading letters still ahead (the
// run at the start of post) are rejoined and checked against the month
// abbreviations monthDay accepts. A month-shaped join rejoins so monthDay
// still sees the date; anything else takes a space so the unwrapped evidence
// ("reset at") is preserved for the #6765 re-read identity.
func wrapSplitsMonth(pre, post string) bool {
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
	for m := range monthIndex {
		if strings.HasPrefix(joined, m) {
			return true
		}
		// A multi-row split of a month token — a very narrow pane breaking
		// "November" one letter per row ("N\no\nvember") — presents a
		// 2-letter fragment ("No") at the first wrap that is not yet a
		// complete 3-letter month abbreviation, so the prefix check above
		// misses it and the wrap would space to "N o vember", which monthDay
		// cannot match (degrading the reset to time-only). Rejoin when the
		// fragment is a strict prefix of a month abbreviation so the next
		// wrap sees "November" and rejoins in turn. The fragment is at
		// least 2 letters (head and tail are both non-empty), and within a
		// reset clause the only letter-newline-letter wraps that yield a
		// 2-letter month-prefix are month tokens being split, so this does
		// not rejoin a word-boundary wrap.
		if len(joined) < len(m) && strings.HasPrefix(m, joined) {
			return true
		}
	}
	return false
}

// wrapSplitsClockSuffix reports whether a wrap newline between two ASCII
// letters splits the two-letter [ap]m meridiem suffix of a clock token —
// e.g. "2p\nm" (the wrapped "2pm") or "11a\nm" (the wrapped "11am") — so the
// normalizer rejoins it rather than spacing to "2p m", which the clockTime
// regex rejects (the [ap] and m must be contiguous). The shape is
// letter-newline-letter, byte-identical to a month mid-token wrap and to a
// word-boundary wrap, so the distinction is semantic: the trailing letter
// run of pre is exactly one [ap] letter that immediately follows a clock-hour
// digit, or — for the explicitly supported spaced meridiem form "2:30 PM" /
// "2 PM" — a space that itself follows a clock digit, and the leading letter
// run of post is exactly one 'm'. Anything wider (a word-boundary wrap like
// "tap\nm", or a multi-letter head/tail) is not a clock-suffix split and
// takes a space, preserving the unwrapped evidence for the #6765 re-read
// identity.
func wrapSplitsClockSuffix(pre, post string) bool {
	j := len(pre)
	for j > 0 {
		c := pre[j-1]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			break
		}
		j--
	}
	head := pre[j:]
	if len(head) != 1 {
		return false
	}
	switch head[0] {
	case 'a', 'A', 'p', 'P':
	default:
		return false
	}
	// The [ap] must immediately follow a clock-hour digit, or — for the
	// explicitly supported spaced meridiem form "2:30 PM" / "2 PM" — a
	// space that itself follows a clock digit; otherwise a word-boundary
	// split like "tap M" would rejoin.
	if j == 0 {
		return false
	}
	prev := pre[j-1]
	if prev == ' ' || prev == '\t' {
		if j < 2 {
			return false
		}
		pp := pre[j-2]
		if pp < '0' || pp > '9' {
			return false
		}
	} else if prev < '0' || prev > '9' {
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
	if len(tail) != 1 {
		return false
	}
	return tail[0] == 'm' || tail[0] == 'M'
}

// wrapSplitsAtPreposition reports whether a wrap newline between two ASCII
// letters splits the leading "at" preposition of a reset clause —
// e.g. "reset a\nt 2pm" (the wrapped "reset at 2pm") — so the normalizer
// rejoins it rather than spacing to "reset a t 2pm". The anchor's optional
// "at" branch does not cross a newline, so a wrapped "at" lands inside the
// capture; the rejoin keeps the evidence byte-identical to the unwrapped
// "reset at …" render (the #6765 re-read identity) across pane resizes. The
// shape is letter-newline-letter, byte-identical to a month/weekday mid-token
// wrap and to a word-boundary wrap, so the distinction is semantic: the
// trailing letter run of pre is exactly the single-letter word "a" (not a
// longer word ending in "a") and the leading letter run of post is exactly
// "t" (not a longer word starting with "t"). Only the preposition "at"
// matches; a longer head ("data") or tail ("thing") takes a space.
func wrapSplitsAtPreposition(pre, post string) bool {
	j := len(pre)
	for j > 0 {
		c := pre[j-1]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			break
		}
		j--
	}
	head := pre[j:]
	if len(head) != 1 {
		return false
	}
	if head[0] != 'a' && head[0] != 'A' {
		return false
	}
	// "a" must be a standalone single-letter word, not the tail of a longer
	// word ("data"). At the very start of the capture (j == 0) it is the
	// preposition following "reset ".
	if j > 0 {
		prev := pre[j-1]
		if (prev >= 'a' && prev <= 'z') || (prev >= 'A' && prev <= 'Z') {
			return false
		}
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
	if len(tail) != 1 {
		return false
	}
	return tail[0] == 't' || tail[0] == 'T'
}

// wrapSplitsDigitOrdinalSuffix reports whether a wrap newline between a digit
// and an ASCII letter splits a calendar day from its ordinal suffix —
// e.g. "Nov 12\nth, 2027" (the wrapped "Nov 12th") — so the normalizer
// rejoins it rather than spacing to "Nov 12 th", which monthDay matches only
// as the day "12" and silently discards the explicit year. This is the
// digit-newline-letter counterpart to wrapSplitsOrdinalSuffix (which handles
// the letter-newline-letter shape "12t\nh"). The pre digit run must be a
// calendar day (1-2 digits), and post must begin with a complete two-letter
// ordinal suffix ("st"/"nd"/"rd"/"th") immediately followed by a non-letter
// (the comma or space of the date), so a longer word starting with those
// letters ("the") does not rejoin.
func wrapSplitsDigitOrdinalSuffix(pre, post string) bool {
	if len(pre) == 0 {
		return false
	}
	last := pre[len(pre)-1]
	if last < '0' || last > '9' {
		return false
	}
	j := len(pre)
	for j > 0 {
		c := pre[j-1]
		if c < '0' || c > '9' {
			break
		}
		j--
	}
	run := len(pre) - j
	if run < 1 || run > 2 {
		return false
	}
	if len(post) < 2 {
		return false
	}
	switch strings.ToLower(post[:2]) {
	case "st", "nd", "rd", "th":
	default:
		return false
	}
	if len(post) >= 3 {
		c := post[2]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

// preEndsOrdinalSuffix reports whether the buffer already emitted ends with a
// complete two-letter ordinal suffix ("st"/"nd"/"rd"/"th") immediately
// following a day digit — e.g. "Nov 12th" — so a wrap newline before the
// date's comma ("Nov 12th\n, 2027") rejoins the same way the digit-before-
// comma branch ("Nov 6\n, 2027") does. monthDay's optional year requires the
// comma to immediately follow the day (or its ordinal suffix); the spaced
// form "Nov 12th , 2027" matches only "Nov 12th" and silently discards the
// explicit year, scheduling the reset a year early.
func preEndsOrdinalSuffix(pre string) bool {
	j := len(pre)
	for j > 0 {
		c := pre[j-1]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			break
		}
		j--
	}
	head := pre[j:]
	if len(head) != 2 {
		return false
	}
	switch strings.ToLower(head) {
	case "st", "nd", "rd", "th":
	default:
		return false
	}
	if j == 0 {
		return false
	}
	prev := pre[j-1]
	return prev >= '0' && prev <= '9'
}
