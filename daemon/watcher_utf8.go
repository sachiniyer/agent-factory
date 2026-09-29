package daemon

import "strings"

// sanitizeUTF8 strips invalid UTF-8 byte sequences from s so the result is
// valid UTF-8. bufio.Reader.ReadSlice returns bufio.ErrBufferFull with a chunk
// of exactly maxWatchLineBytes bytes; when the byte cap splits a multi-byte
// rune — or the watched command emits latin-1/binary noise — the chunk can hold
// invalid UTF-8 anywhere, not only a trailing partial rune. The durable event
// queue persists the chunk via json.Marshal(queuedEvent{Line: line}), whose
// encoding/json rewrites invalid UTF-8 as U+FFFD in the replay .jsonl record
// (#863 class, exposed by #1129 when the raw 64KB chunk was wired through the
// durable queue, #4655). truncateRunes(line, maxWatchLineBytes) does not cover
// this: its len<=maxBytes fast path leaves a trailing split rune in place and
// it never touches invalid bytes earlier in the chunk, so the line must be made
// valid UTF-8 directly. strings.ToValidUTF8 with an empty replacement drops
// every invalid byte sequence — including a trailing partial rune — so the
// retained chunk is always valid UTF-8 and no U+FFFD reaches the durable
// record; dropping (not replacing) the bytes is intentional, since a U+FFFD
// replacement would survive json.Marshal and re-introduce the corruption.
// Already-valid UTF-8 (all ASCII content) is returned unchanged, so the full
// byte cap is preserved for ASCII — TestWatcherTruncatesLongLines pins that
// case at exactly maxWatchLineBytes.
func sanitizeUTF8(s string) string {
	return strings.ToValidUTF8(s, "")
}
