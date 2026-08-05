package session

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
)

// Token redaction for hook output that lands in a persisted error.
//
// WHAT THIS IS, AND WHAT IT IS NOT. This is defense in depth against the
// ACCIDENT of a launch_cmd echoing its endpoint into a diagnostic — the common
// case by far, and the one worth catching. It is NOT a security boundary, and it
// cannot become one: a hook script is the user's own code, the token is its own
// secret, and `echo "token is $TOK"` defeats any redactor ever written. Chasing
// completeness here buys a guarantee that does not exist.
//
// So the contract is deliberately modest and, in exchange, sound:
//
//   - It may MISS a token in sufficiently mangled output.
//   - It must never CORRUPT the diagnostic, panic, or run super-linearly. The
//     output it is redacting is the only window a user has onto a failed remote
//     provision; losing that window to a redactor bug is a worse outcome than the
//     leak it was trying to prevent.
//
// Two passes, in order of how much they can trust their input:
//
//  1. Complete JSON documents go through encoding/json, which is exact. Strings
//     inside them are re-sanitized recursively, so a structured log that
//     serializes the endpoint into a field is covered.
//  2. Everything else is matched as raw bytes: a token key, a colon, a quoted
//     value. This catches truncated output, where a killed launch_cmd wrote the
//     credential and then died before closing its JSON.

// maxHookTokenKeyEncodedLen bounds the token-key scan. A five-character key is
// 30 bytes when every character uses a six-byte \u escape, and each further
// serialization level doubles its backslashes: 35 bytes at depth 2, 45 at depth
// 3. 96 covers those with room to spare while still keeping a malformed escape
// flood linear — the earlier 32-byte bound silently gave up on a serialized
// Unicode-escaped key and left its token in the error (Codex P2 on #2718).
const maxHookTokenKeyEncodedLen = 96

// maxHookTokenQuoteEscapeDepth is how many backslashes may precede a quote that
// still counts as a delimiter. Depth 0 is a raw JSON object, 1 is one serialized
// into a string field, 2 is that logged again. Beyond this the output is noise,
// and over-matching a delimiter only ever redacts more.
const maxHookTokenQuoteEscapeDepth = 6

// maxHookTokenKeyDecodeDepth bounds how many escaping levels a key spelling is
// peeled through before it is judged not to be `token`.
const maxHookTokenKeyDecodeDepth = 3

// redactHookOutputTokens preserves the hook's diagnostic text while replacing
// every quoted JSON token value it can recognize.
func redactHookOutputTokens(output string) string {
	output = redactCompleteHookJSON(output)

	ranges := hookTokenRedactionRanges(output)
	if len(ranges) == 0 {
		return output
	}
	var redacted strings.Builder
	written := 0
	for _, redaction := range ranges {
		if redaction.start < written {
			continue
		}
		redacted.WriteString(output[written:redaction.start])
		redacted.WriteString("[REDACTED]")
		written = redaction.end
	}
	redacted.WriteString(output[written:])
	return redacted.String()
}

// redactCompleteHookJSON handles structured log records, including ones that
// serialize another JSON document into a string field. The byte matcher below
// cannot see through arbitrary escaping, so decode complete values first — that
// path is exact — and recursively sanitize JSON-bearing strings. UseNumber keeps
// unrelated resource IDs byte-exact when a sanitized value is re-encoded.
func redactCompleteHookJSON(output string) string {
	var redacted strings.Builder
	written := 0
	for cursor := 0; cursor < len(output); {
		candidate, next := extractJSONAt(output, cursor)
		if candidate == "" {
			break
		}
		start := next - len(candidate)
		if start < written {
			cursor = next
			continue
		}
		replacement, changed := redactHookJSONDocument(candidate)
		if changed {
			redacted.WriteString(output[written:start])
			redacted.WriteString(replacement)
			written = next
		}
		cursor = next
	}
	if written == 0 {
		return output
	}
	redacted.WriteString(output[written:])
	return redacted.String()
}

func redactHookJSONDocument(document string) (string, bool) {
	decoder := json.NewDecoder(strings.NewReader(document))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return document, false
	}

	value, changed := redactHookJSONValue(value)
	if !changed {
		return document, false
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return document, false
	}
	return string(encoded), true
}

func redactHookJSONValue(value any) (any, bool) {
	switch value := value.(type) {
	case map[string]any:
		changed := false
		for key, child := range value {
			if strings.EqualFold(key, "token") {
				value[key] = "[REDACTED]"
				changed = true
				continue
			}
			redacted, childChanged := redactHookJSONValue(child)
			if childChanged {
				value[key] = redacted
				changed = true
			}
		}
		return value, changed
	case []any:
		changed := false
		for index, child := range value {
			redacted, childChanged := redactHookJSONValue(child)
			if childChanged {
				value[index] = redacted
				changed = true
			}
		}
		return value, changed
	case string:
		// A complete outer record can carry an incomplete serialized endpoint, so
		// run the full redaction over the decoded string before re-encoding.
		redacted := redactHookOutputTokens(value)
		return redacted, redacted != value
	default:
		return value, false
	}
}

type hookOutputRange struct {
	start int
	end   int
	// complete records that the value's closing quote was found, as opposed to
	// the scan stopping at a line break or at the end of truncated output.
	complete bool
}

// hookTokenRedactionRanges finds `token`-keyed string values in raw bytes,
// without needing the surrounding document to parse or even to be complete.
//
// The escape depth of the enclosing context is deliberately IGNORED. A serialized
// endpoint reaches a log line as `\"token\":\"…\"`, and once an outer string is
// truncated or split across a raw newline its delimiters no longer nest
// consistently — that ambiguity is what a stateful scanner has to guess at, and
// guessing is what makes it phase-shift. Matching the field wherever it appears,
// at whatever depth, needs no guess: a false positive only redacts more.
// A raw line break can land anywhere in the field — inside the key, before the
// colon, inside the value — because the writer was interrupted or the logger
// wrapped. Rather than teach the matcher to carry state across those breaks (the
// thing that makes such scanners phase-shift), run it a second time over a view
// with the breaks removed and map the hits back to real offsets. One mechanism
// covers every split position; over-matching across a line break only redacts
// more.
func hookTokenRedactionRanges(output string) []hookOutputRange {
	ranges := scanHookTokenRanges(output)
	for index := range ranges {
		// Only a value the scan could not finish continues onto another line. A
		// complete one already has its closing quote, and extending past it would
		// eat the diagnostic that follows.
		if ranges[index].complete {
			continue
		}
		ranges[index].end = extendHookTokenValue(output, ranges[index])
	}
	// The joined pass exists only to find fields the raw pass could not see —
	// ones whose KEY or colon a line break split. Where the raw pass already
	// matched, its result is authoritative: the joined view has no line breaks, so
	// every match in it looks continuable and would run through the diagnostic
	// behind a complete value.
	rawStarts := make(map[int]struct{}, len(ranges))
	for _, raw := range ranges {
		rawStarts[raw.start] = struct{}{}
	}
	if stripped, offsets := stripRawLineBreaks(output); len(offsets) > 0 {
		for _, joined := range scanHookTokenRanges(stripped) {
			if joined.start >= len(offsets) || joined.end <= joined.start {
				continue
			}
			start := offsets[joined.start]
			if _, seen := rawStarts[start]; seen {
				continue
			}
			ranges = append(ranges, hookOutputRange{
				start: start,
				end:   extendHookTokenValue(output, hookOutputRange{start: start, end: start}),
			})
		}
	}
	return mergeHookOutputRanges(ranges)
}

// extendHookTokenValue follows a token value that a raw line break interrupted.
//
// A logger may hard-wrap a long credential, once or several times, and a killed
// launch_cmd may leave it wrapped AND unterminated — in every case the tail is
// still the secret. The continuation is bounded by WHITESPACE rather than by a
// line count: a bearer token contains none, and prose does. That is what keeps a
// truncated token from swallowing the diagnostic behind it, which is the only
// account a user gets of a failed provision. The cost is at most the first word
// of a following line, which is the safe direction to err.
func extendHookTokenValue(output string, value hookOutputRange) int {
	end := value.end
	if end < value.start {
		end = value.start
	}
	for end < len(output) {
		switch output[end] {
		case '\n', '\r':
			// Cross the break, and the INDENT behind it: a logger that hard-wraps a
			// long value commonly indents the continuation, and that tail is still
			// the secret. Whitespace inside a line still terminates the value, so a
			// truncated token cannot run on through prose.
			next := end
			for next < len(output) && (output[next] == '\n' || output[next] == '\r' ||
				output[next] == ' ' || output[next] == '\t') {
				next++
			}
			if next >= len(output) || output[next] == '"' {
				return end
			}
			end = next
		case ' ', '\t', '"':
			return end
		case '\\':
			// An ESCAPED closing quote ends a serialized value just as a bare one
			// ends a raw value. Stepping over it would run the redaction through
			// the diagnostic behind the token.
			if end+1 < len(output) && output[end+1] == '"' {
				return end
			}
			end += 2
		default:
			end++
		}
	}
	return len(output)
}

// stripRawLineBreaks returns output without its raw line breaks, plus the
// original offset of every retained byte. It returns no offsets when there was
// nothing to strip, so the common case does not pay for a second scan.
func stripRawLineBreaks(output string) (string, []int) {
	if !strings.ContainsAny(output, "\n\r") {
		return "", nil
	}
	var stripped strings.Builder
	stripped.Grow(len(output))
	offsets := make([]int, 0, len(output))
	for cursor := 0; cursor < len(output); cursor++ {
		if output[cursor] == '\n' || output[cursor] == '\r' {
			continue
		}
		stripped.WriteByte(output[cursor])
		offsets = append(offsets, cursor)
	}
	return stripped.String(), offsets
}

// mergeHookOutputRanges sorts and coalesces so the writer can emit one pass.
func mergeHookOutputRanges(ranges []hookOutputRange) []hookOutputRange {
	if len(ranges) < 2 {
		return ranges
	}
	slices.SortFunc(ranges, func(a, b hookOutputRange) int { return a.start - b.start })
	merged := ranges[:1]
	for _, candidate := range ranges[1:] {
		last := &merged[len(merged)-1]
		if candidate.start <= last.end {
			if candidate.end > last.end {
				last.end = candidate.end
			}
			continue
		}
		merged = append(merged, candidate)
	}
	return merged
}

func scanHookTokenRanges(output string) []hookOutputRange {
	var ranges []hookOutputRange
	for cursor := 0; cursor < len(output); cursor++ {
		if output[cursor] != '"' {
			continue
		}
		keyEnd, ok := hookTokenKeyEnd(output, cursor)
		if !ok {
			continue
		}

		separator := skipHookTokenPadding(output, keyEnd)
		if separator >= len(output) || output[separator] != ':' {
			continue
		}
		valueStart := skipHookTokenPadding(output, separator+1)
		if valueStart >= len(output) || output[valueStart] != '"' {
			continue
		}

		valueEnd, complete := hookTokenValueEnd(output, valueStart)
		candidate := hookOutputRange{start: valueStart + 1, end: valueEnd, complete: complete}
		if len(ranges) > 0 && candidate.start <= ranges[len(ranges)-1].end {
			if candidate.end > ranges[len(ranges)-1].end {
				ranges[len(ranges)-1].end = candidate.end
			}
		} else {
			ranges = append(ranges, candidate)
		}
		if !complete {
			break
		}
		// Reconsider the closing quote as a possible overlapping key boundary.
		cursor = valueEnd - 1
	}
	return ranges
}

// hookTokenKeyEnd reports the end of a `token` key opening at start, accepting
// any JSON-equivalent spelling within the bounded length. The key's own quotes
// may themselves be escaped, so both delimiters are normalized before decoding.
func hookTokenKeyEnd(input string, start int) (int, bool) {
	limit := start + maxHookTokenKeyEncodedLen
	if limit > len(input) {
		limit = len(input)
	}
	// However `token` is spelled, its bytes contain a literal t/T or a \u escape.
	// Rejecting the whole window on that once — rather than attempting a decode at
	// each escaped quote inside it — is what keeps an escaped-quote flood cheap
	// now that the window is wide enough for deeply escaped keys.
	if window := input[start:limit]; !strings.ContainsAny(window, "tT") &&
		!strings.Contains(window, `\u`) {
		return 0, false
	}
	escaped := false
	for cursor := start + 1; cursor < limit; cursor++ {
		switch {
		case escaped:
			escaped = false
			if input[cursor] == '"' {
				// An escaped quote closes a key whose delimiters are themselves
				// escaped — `\"token\"` inside a serialized document. Drop the whole
				// run of backslashes before it, not one: at depth 2 and beyond the
				// delimiter carries several, and a leftover kept the key from matching.
				body := input[start+1 : cursor]
				for len(body) > 0 && body[len(body)-1] == '\\' {
					body = body[:len(body)-1]
				}
				if hookTokenKeyMatches(body) {
					return cursor + 1, true
				}
			}
		case input[cursor] == '\\':
			escaped = true
		case input[cursor] == '"':
			return cursor + 1, hookTokenKeyMatches(input[start+1 : cursor])
		}
	}
	return 0, false
}

// hookTokenKeyMatches reports whether a key body spells `token`, peeling one
// escaping level at a time. A serialized document reaches a log line already
// escaped once, so `token` and `\\u0074oken` are both this key — the depth
// depends on how many times the record was nested, which is not knowable here.
func hookTokenKeyMatches(body string) bool {
	// Reject on length and shape before decoding. A malformed diagnostic can put
	// a quote every other byte, and this runs at each one.
	if len(body) < len("token") || len(body) > maxHookTokenKeyEncodedLen {
		return false
	}
	if !strings.Contains(body, `\`) {
		return strings.EqualFold(body, "token")
	}
	// However it is spelled, this key contains a literal `t` or a `\u` escape.
	// Without one there is nothing to decode, and a quote flood hits this at
	// every boundary.
	if !strings.ContainsAny(body, "tT") && !strings.Contains(body, `\u`) {
		return false
	}
	candidate := body
	for depth := 0; depth <= maxHookTokenKeyDecodeDepth; depth++ {
		if strings.EqualFold(candidate, "token") {
			return true
		}
		decoded, ok := decodeJSONStringBody(candidate)
		if !ok || decoded == candidate {
			return false
		}
		candidate = decoded
	}
	return false
}

// decodeJSONStringBody interprets the escape sequences in one JSON string body —
// the bytes BETWEEN a pair of quotes, never the quotes themselves.
//
// It decodes in place rather than wrapping the body back in quotes and handing
// it to encoding/json. Rebuilding a document around bytes that came out of
// arbitrary hook output is the shape of an injection even where it happens to be
// safe: today `json.Unmarshal` rejects a body containing a bare quote only
// because it requires the WHOLE input to be one value, so a later switch to
// `json.Decoder.Decode`, which stops at the first value, would silently let
// `token","x` match as this key. Not reconstructing the document removes the
// question (CodeQL go/unsafe-quoting).
//
// Anything that cannot appear inside a JSON string is rejected outright, so a
// malformed fragment is refused rather than guessed at.
func decodeJSONStringBody(body string) (string, bool) {
	var decoded strings.Builder
	decoded.Grow(len(body))
	for cursor := 0; cursor < len(body); cursor++ {
		if body[cursor] == '"' || body[cursor] < 0x20 {
			return "", false
		}
		if body[cursor] != '\\' {
			decoded.WriteByte(body[cursor])
			continue
		}
		cursor++
		if cursor >= len(body) {
			return "", false
		}
		switch body[cursor] {
		case '"', '\\', '/':
			decoded.WriteByte(body[cursor])
		case 'b':
			decoded.WriteByte('\b')
		case 'f':
			decoded.WriteByte('\f')
		case 'n':
			decoded.WriteByte('\n')
		case 'r':
			decoded.WriteByte('\r')
		case 't':
			decoded.WriteByte('\t')
		case 'u':
			if cursor+4 >= len(body) {
				return "", false
			}
			// A JSON \uXXXX escape is exactly 16 bits, so parse it as 16 — the
			// conversion to rune is then bounded by the type rather than by the
			// happenstance that this slice is four digits long.
			point, err := strconv.ParseUint(body[cursor+1:cursor+5], 16, 16)
			if err != nil {
				return "", false
			}
			decoded.WriteRune(rune(point))
			cursor += 4
		default:
			return "", false
		}
	}
	return decoded.String(), true
}

// hookTokenValueEnd returns the offset of the value's closing quote. The earliest
// plausible terminator wins: a raw newline ends the line, and an escaped or bare
// quote ends the value. Stopping early can leave a fragment of a token that
// itself contains a quote; running long would swallow the rest of the
// diagnostic, which is the thing this output exists to show.
func hookTokenValueEnd(input string, start int) (int, bool) {
	for cursor := start + 1; cursor < len(input); cursor++ {
		switch input[cursor] {
		case '"':
			end := cursor
			for end > start+1 && input[end-1] == '\\' && end > cursor-maxHookTokenQuoteEscapeDepth {
				end--
			}
			return end, true
		case '\n', '\r':
			return cursor, false
		}
	}
	return len(input), false
}

// skipHookTokenPadding skips whitespace and the stray backslashes that separate
// a serialized key from its colon once the enclosing escaping is inconsistent.
func skipHookTokenPadding(input string, start int) int {
	for start < len(input) {
		switch input[start] {
		case ' ', '\t', '\r', '\n', '\\':
			start++
		default:
			return start
		}
	}
	return start
}
