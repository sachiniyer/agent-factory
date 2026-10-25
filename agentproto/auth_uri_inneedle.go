package agentproto

import "strings"

// scanRawAccessTokenValues is redactRawAccessTokenValue's implementation. It
// also returns a count of the work it did — loop iterations plus value bytes
// searched for a terminator — so a test can pin that the count grows linearly
// with the input, whatever the input's shape.
//
// Matching an escaped needle character at every cursor position would rescan
// a run of '%' bytes once per position, which is quadratic in the run (#4702).
// The escapes are resolved once up front instead, by resolvePercentEscapes,
// and each cursor position then costs at most one step per needle character.
// The output is built once too, rather than by rewriting raw after every
// match, so the escape table keeps indexing the input.
func scanRawAccessTokenValues(raw, terminators string) (string, bool, int) {
	needle := AccessTokenQueryParam + "="
	escapes, steps := resolvePercentEscapes(raw)
	var redacted strings.Builder
	copied := 0
	for cursor := 0; cursor < len(raw); {
		keyEnd, matchSteps, ok := matchAccessTokenNeedleRaw(raw, cursor, needle, escapes)
		steps += matchSteps
		if !ok {
			cursor++
			continue
		}
		valueEnd := len(raw)
		if terminators != "" {
			if pos := strings.IndexAny(raw[keyEnd:], terminators); pos >= 0 {
				valueEnd = keyEnd + pos
			}
		}
		steps += valueEnd - keyEnd
		if copied == 0 {
			redacted.Grow(len(raw))
		}
		redacted.WriteString(raw[copied:keyEnd])
		redacted.WriteString(accessTokenRedaction)
		copied = valueEnd
		cursor = valueEnd
	}
	if redacted.Len() == 0 {
		return raw, false, steps
	}
	redacted.WriteString(raw[copied:])
	return redacted.String(), true, steps
}

// matchAccessTokenNeedleRaw reports whether needle starts at raw[cursor],
// accepting at each needle position the literal byte case-insensitively or a
// resolved escape whose byte equals it case-insensitively. It returns the raw
// position after the needle, so the caller keeps the key's spelling verbatim
// and rewrites only the value (#4161). A '%' that does not resolve is data,
// not an escape, so the candidate at that position fails and the %ac overlap
// still matches from the literal tail that survives the collapse.
func matchAccessTokenNeedleRaw(raw string, cursor int, needle string, escapes []percentEscape) (int, int, bool) {
	pos := cursor
	for j := 0; j < len(needle); j++ {
		if pos >= len(raw) {
			return 0, j, false
		}
		if equalFoldASCII(raw[pos], needle[j]) {
			pos++
			continue
		}
		if escapes != nil && escapes[pos].end != 0 && equalFoldASCII(escapes[pos].value, needle[j]) {
			pos = escapes[pos].end
			continue
		}
		return 0, j + 1, false
	}
	return pos, len(needle), true
}

// percentEscape is the resolution of the escape that starts at a '%': the
// byte it decodes to and the raw position after it. end is zero when the '%'
// does not start an escape.
type percentEscape struct {
	end   int
	value byte
}

// resolvePercentEscapes resolves, for every '%' in raw, the escape starting
// there the way redactx.PercentDecode's reducing stack would if it began at
// that '%': a '%' followed by two hex digits, where either digit may itself be
// an escape that decodes to a hex digit (%25%35%46 is '_'), and where a result
// of '%' is itself an escape start that needs two more digits (%255F is '_').
// The escape ends at the first byte it decodes to other than '%'. It returns
// nil when raw has no '%'.
//
// Every dependency lies to the right, so one right-to-left pass fills the
// table. The total is linear: a digit an escape consumes is either a literal
// or a nested escape wholly inside it, so the nested escapes form a tree and
// each loop iteration consumes digits no other escape's loop consumes.
func resolvePercentEscapes(raw string) ([]percentEscape, int) {
	if strings.IndexByte(raw, '%') < 0 {
		return nil, 0
	}
	escapes := make([]percentEscape, len(raw))
	hexDigit := func(pos int) (byte, int, bool) {
		if pos >= len(raw) {
			return 0, 0, false
		}
		if digit, ok := hexValue(raw[pos]); ok {
			return digit, pos + 1, true
		}
		// A resolved escape never decodes to '%', so it is a digit only when
		// its value is one.
		if escapes[pos].end != 0 {
			if digit, ok := hexValue(escapes[pos].value); ok {
				return digit, escapes[pos].end, true
			}
		}
		return 0, 0, false
	}
	steps := 0
	for start := len(raw) - 1; start >= 0; start-- {
		if raw[start] != '%' {
			continue
		}
		for pos := start + 1; ; {
			steps++
			high, next, ok := hexDigit(pos)
			if !ok {
				break
			}
			low, end, ok := hexDigit(next)
			if !ok {
				break
			}
			if value := high<<4 | low; value != '%' {
				escapes[start] = percentEscape{end: end, value: value}
				break
			}
			pos = end
		}
	}
	return escapes, steps
}

// equalFoldASCII reports whether got equals the lowercase ASCII byte want
// under ASCII case folding, mirroring indexFoldASCII.
func equalFoldASCII(got, want byte) bool {
	if got >= 'A' && got <= 'Z' {
		got += 'a' - 'A'
	}
	return got == want
}

// hexValue returns the value of a single ASCII hex digit, mirroring
// redactx's unexported helper of the same name.
func hexValue(char byte) (byte, bool) {
	switch {
	case char >= '0' && char <= '9':
		return char - '0', true
	case char >= 'a' && char <= 'f':
		return char - 'a' + 10, true
	case char >= 'A' && char <= 'F':
		return char - 'A' + 10, true
	default:
		return 0, false
	}
}
