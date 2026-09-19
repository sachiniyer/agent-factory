package daemon

import "unicode/utf8"

// trimTrailingPartialRune drops a trailing partial UTF-8 rune from s so the
// result is valid UTF-8. bufio.Reader.ReadSlice returns bufio.ErrBufferFull
// with a chunk of exactly maxWatchLineBytes bytes; when the byte cap splits a
// multi-byte rune the chunk ends mid-rune, and the durable event queue
// persists the chunk via json.Marshal(queuedEvent{Line: line}), whose
// encoding/json rewrites invalid UTF-8 as U+FFFD in the replay .jsonl record
// (#863 class, exposed by #1129 when the raw 64KB chunk was wired through the
// durable queue). truncateRunes(line, maxWatchLineBytes) does not fix this:
// len(line) == maxWatchLineBytes hits that helper's len<=maxBytes fast-path and
// leaves the split rune untrimmed, so the trailing incomplete rune must be
// trimmed directly. Already-valid UTF-8 (all ASCII content) is returned
// unchanged so the full byte cap is preserved — TestWatcherTruncatesLongLines
// pins the ASCII case at exactly maxWatchLineBytes.
func trimTrailingPartialRune(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	// Walk back over the trailing continuation bytes to the lead byte of the
	// incomplete rune, then drop the lead byte too so s[:end] holds only whole
	// runes.
	end := len(s)
	for end > 0 && !utf8.RuneStart(s[end-1]) {
		end--
	}
	if end > 0 {
		end--
	}
	return s[:end]
}
