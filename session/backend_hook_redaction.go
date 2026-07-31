package session

import (
	"encoding/json"
	"strings"
)

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
	escaped := false
	for cursor := 0; cursor < len(output); cursor++ {
		if candidateStart < 0 {
			if output[cursor] == '"' {
				candidateStart = cursor
				syntheticStart = false
				escaped = false
			}
			continue
		}
		if output[cursor] == '\n' || output[cursor] == '\r' {
			if replacement, changed := sanitizeSerializedHookJSONString(
				output[candidateStart:cursor], syntheticStart, true,
			); changed {
				appendSerializedHookJSONReplacement(&redacted, output, &written,
					candidateStart, cursor, replacement)
			}
			// The next line can continue the impossible outer string with escaped
			// endpoint JSON. Treat its first byte as following a synthetic opening
			// quote until a real unescaped quote supplies the closing boundary.
			candidateStart = cursor + 1
			syntheticStart = true
			escaped = false
			continue
		}
		if escaped {
			escaped = false
			continue
		}
		if output[cursor] == '\\' {
			escaped = true
			continue
		}
		if output[cursor] != '"' {
			continue
		}

		candidateEnd := cursor + 1
		if replacement, changed := sanitizeSerializedHookJSONString(
			output[candidateStart:candidateEnd], syntheticStart, false,
		); changed {
			appendSerializedHookJSONReplacement(&redacted, output, &written,
				candidateStart, candidateEnd, replacement)
		}
		candidateStart = cursor
		syntheticStart = false
		escaped = false
	}
	if candidateStart >= 0 {
		// A killed hook may leave the outer JSON string unfinished even though
		// its escaped inner token value is complete. Close only for decoding,
		// sanitize the decoded prefix, then drop the synthetic closing quote so
		// the diagnostic remains faithful to the interrupted output.
		if replacement, changed := sanitizeSerializedHookJSONString(
			output[candidateStart:], syntheticStart, true,
		); changed {
			appendSerializedHookJSONReplacement(&redacted, output, &written,
				candidateStart, len(output), replacement)
		}
	}
	if written == 0 {
		return output
	}
	redacted.WriteString(output[written:])
	return redacted.String()
}

func sanitizeSerializedHookJSONString(candidate string, syntheticStart, syntheticEnd bool) ([]byte, bool) {
	document := candidate
	if syntheticStart {
		document = `"` + document
	}
	if syntheticEnd {
		document += `"`
	}
	var decoded string
	if json.Unmarshal([]byte(document), &decoded) != nil {
		return nil, false
	}
	sanitized := redactHookOutputTokens(decoded)
	if sanitized == decoded {
		return nil, false
	}
	encoded, _ := json.Marshal(sanitized)
	start, end := 0, len(encoded)
	if syntheticStart {
		start++
	}
	if syntheticEnd {
		end--
	}
	return encoded[start:end], true
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
