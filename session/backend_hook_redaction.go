package session

import (
	"encoding/json"
	"strings"
)

const maxHookTokenKeyEncodedLen = 32

type serializedTokenPhase uint8

const (
	serializedTokenNone serializedTokenPhase = iota
	serializedTokenPartialKey
	serializedTokenColon
	serializedTokenValue
	serializedTokenValueTail
)

type serializedTokenContinuation struct {
	phase     serializedTokenPhase
	keyPrefix string
}

// redactSerializedHookJSONStrings handles a JSON string containing serialized
// endpoint JSON whether it is a top-level record, an object field, or surrounded
// by logger metadata. Every quote boundary is considered so JSON-equivalent
// encodings such as leading whitespace and \u007b are covered. The scanner
// examines every byte once and reconsiders each closing quote as the next
// possible opener, so malformed prefixes cannot phase-shift later values.
func redactSerializedHookJSONStrings(output string) string {
	var redacted strings.Builder
	written := 0
	candidateStart := -1
	syntheticStart := false
	candidateInvalid := false
	escapedOpenerStart := -1
	unicodeEscapeDigits := 0
	tokenContinuation := serializedTokenContinuation{}
	escaped := false
	sanitizeCandidate := func(start, end int, syntheticStart, syntheticEnd bool) {
		replacement, changed, nextContinuation := sanitizeSerializedHookJSONString(
			output[start:end], syntheticStart, syntheticEnd, tokenContinuation,
		)
		if changed {
			appendSerializedHookJSONReplacement(&redacted, output, &written,
				start, end, replacement)
		}
		tokenContinuation = nextContinuation
	}
	for cursor := 0; cursor < len(output); cursor++ {
		if candidateStart < 0 {
			if output[cursor] == '"' {
				candidateStart = cursor
				syntheticStart = false
				candidateInvalid = false
				escapedOpenerStart = -1
				unicodeEscapeDigits = 0
				escaped = false
			}
			continue
		}
		if output[cursor] == '\n' || output[cursor] == '\r' {
			sanitizeCandidate(candidateStart, cursor, syntheticStart, true)
			// The next line can continue the impossible outer string with escaped
			// endpoint JSON. Treat its first byte as following a synthetic opening
			// quote until a real unescaped quote supplies the closing boundary.
			candidateStart = cursor + 1
			syntheticStart = true
			candidateInvalid = false
			escapedOpenerStart = -1
			unicodeEscapeDigits = 0
			escaped = false
			continue
		}
		if unicodeEscapeDigits > 0 {
			if isJSONHexDigit(output[cursor]) {
				unicodeEscapeDigits--
				continue
			}
			candidateInvalid = true
			unicodeEscapeDigits = 0
		}
		if escaped {
			switch output[cursor] {
			case 'u':
				unicodeEscapeDigits = 4
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			default:
				candidateInvalid = true
			}
			if output[cursor] == '"' && candidateInvalid {
				escapedOpenerStart = cursor + 1
			}
			escaped = false
			continue
		}
		if escapedOpenerStart >= 0 {
			switch output[cursor] {
			case ' ', '\t':
				continue
			case '{', '[':
				candidateStart = escapedOpenerStart
				syntheticStart = true
				candidateInvalid = false
				escapedOpenerStart = -1
				unicodeEscapeDigits = 0
			case '\\':
				if isEscapedJSONContainerOpener(output, cursor) {
					candidateStart = escapedOpenerStart
					syntheticStart = true
					candidateInvalid = false
					escapedOpenerStart = -1
					unicodeEscapeDigits = 0
				} else {
					escapedOpenerStart = -1
				}
			default:
				escapedOpenerStart = -1
			}
		}
		if output[cursor] == '\\' {
			escaped = true
			continue
		}
		if output[cursor] < 0x20 {
			candidateInvalid = true
		}
		if output[cursor] != '"' {
			continue
		}

		candidateEnd := cursor + 1
		sanitizeCandidate(candidateStart, candidateEnd, syntheticStart, false)
		candidateStart = cursor
		syntheticStart = false
		candidateInvalid = false
		escapedOpenerStart = -1
		unicodeEscapeDigits = 0
		escaped = false
	}
	if candidateStart >= 0 {
		// A killed hook may leave the outer JSON string unfinished even though
		// its escaped inner token value is complete. Close only for decoding,
		// sanitize the decoded prefix, then drop the synthetic closing quote so
		// the diagnostic remains faithful to the interrupted output.
		sanitizeCandidate(candidateStart, len(output), syntheticStart, true)
	}
	if written == 0 {
		return output
	}
	redacted.WriteString(output[written:])
	return redacted.String()
}

func isJSONHexDigit(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}

func isEscapedJSONContainerOpener(output string, start int) bool {
	if start+6 > len(output) || output[start] != '\\' || output[start+1] != 'u' ||
		output[start+2] != '0' || output[start+3] != '0' {
		return false
	}
	return output[start+4] == '7' && (output[start+5] == 'b' || output[start+5] == 'B') ||
		output[start+4] == '5' && (output[start+5] == 'b' || output[start+5] == 'B')
}

func sanitizeSerializedHookJSONString(
	candidate string,
	syntheticStart, syntheticEnd bool,
	continuation serializedTokenContinuation,
) ([]byte, bool, serializedTokenContinuation) {
	document := candidate
	if syntheticStart {
		document = `"` + document
	}
	if syntheticEnd {
		document += `"`
	}
	var decoded string
	if json.Unmarshal([]byte(document), &decoded) != nil {
		return nil, false, continuation
	}
	sanitized, nextContinuation := redactHookTokenContinuation(decoded, continuation)
	if nextContinuation.phase == serializedTokenNone {
		nextContinuation = hookOutputTokenContinuation(sanitized)
	}
	sanitized = redactHookOutputTokens(sanitized)
	if sanitized == decoded {
		return nil, false, nextContinuation
	}
	encoded, _ := json.Marshal(sanitized)
	start, end := 0, len(encoded)
	if syntheticStart {
		start++
	}
	if syntheticEnd {
		end--
	}
	return encoded[start:end], true, nextContinuation
}

func hookOutputTokenContinuation(output string) serializedTokenContinuation {
	for cursor := 0; cursor < len(output); cursor++ {
		if output[cursor] != '"' {
			continue
		}
		keyEnd, ok := hookTokenKeyEnd(output, cursor)
		if !ok {
			if _, complete := jsonStringEnd(output, cursor); !complete && len(output)-cursor < maxHookTokenKeyEncodedLen {
				return serializedTokenContinuation{
					phase:     serializedTokenPartialKey,
					keyPrefix: output[cursor:],
				}
			}
			continue
		}
		separator := skipJSONWhitespace(output, keyEnd)
		if separator == len(output) {
			return serializedTokenContinuation{phase: serializedTokenColon}
		}
		if output[separator] != ':' {
			continue
		}
		valueStart := skipJSONWhitespace(output, separator+1)
		if valueStart == len(output) {
			return serializedTokenContinuation{phase: serializedTokenValue}
		}
		if output[valueStart] != '"' {
			continue
		}
		valueEnd, complete := jsonStringEnd(output, valueStart)
		if !complete {
			return serializedTokenContinuation{phase: serializedTokenValueTail}
		}
		cursor = valueEnd - 2
	}
	return serializedTokenContinuation{}
}

func redactHookTokenContinuation(
	output string,
	continuation serializedTokenContinuation,
) (string, serializedTokenContinuation) {
	start := 0
	if continuation.phase == serializedTokenPartialKey {
		combined := continuation.keyPrefix + output
		keyEnd, complete := jsonStringEnd(combined, 0)
		if !complete {
			if len(combined) < maxHookTokenKeyEncodedLen {
				continuation.keyPrefix = combined
				return output, continuation
			}
			return output, serializedTokenContinuation{}
		}
		var key string
		if keyEnd > maxHookTokenKeyEncodedLen || json.Unmarshal([]byte(combined[:keyEnd]), &key) != nil ||
			!strings.EqualFold(key, "token") {
			return output, serializedTokenContinuation{}
		}
		start = keyEnd - len(continuation.keyPrefix)
		continuation = serializedTokenContinuation{phase: serializedTokenColon}
	}
	if continuation.phase == serializedTokenColon {
		start = skipJSONWhitespace(output, start)
		if start == len(output) {
			return output, continuation
		}
		if output[start] != ':' {
			return output, serializedTokenContinuation{}
		}
		start++
		continuation = serializedTokenContinuation{phase: serializedTokenValue}
	}
	if continuation.phase == serializedTokenValue {
		start = skipJSONWhitespace(output, start)
		if start == len(output) {
			return output, continuation
		}
		if output[start] != '"' {
			return output, serializedTokenContinuation{}
		}
		end, complete := jsonStringEnd(output, start)
		redacted := output[:start] + `"[REDACTED]"` + output[end:]
		if !complete {
			return redacted, serializedTokenContinuation{phase: serializedTokenValueTail}
		}
		return redacted, serializedTokenContinuation{}
	}
	if continuation.phase == serializedTokenValueTail {
		end, complete := jsonStringContinuationEnd(output)
		if !complete {
			return "", continuation
		}
		return output[end:], serializedTokenContinuation{}
	}
	return output, serializedTokenContinuation{}
}

func jsonStringContinuationEnd(input string) (int, bool) {
	escaped := false
	for cursor := 0; cursor < len(input); cursor++ {
		switch {
		case escaped:
			escaped = false
		case input[cursor] == '\\':
			escaped = true
		case input[cursor] == '"':
			return cursor + 1, true
		}
	}
	return len(input), false
}

// appendSerializedHookJSONReplacement preserves a shared quote boundary between
// malformed adjacent strings. The scanner intentionally reuses every closing
// quote as a possible opener, so two sanitized spans can overlap by that byte.
func appendSerializedHookJSONReplacement(
	redacted *strings.Builder,
	output string,
	written *int,
	start, end int,
	replacement []byte,
) {
	overlap := *written - start
	if overlap <= 0 {
		redacted.WriteString(output[*written:start])
		overlap = 0
	}
	if overlap < len(replacement) {
		redacted.Write(replacement[overlap:])
	}
	if end > *written {
		*written = end
	}
}
