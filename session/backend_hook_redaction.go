package session

import (
	"encoding/json"
	"strings"
)

const hookOutputRedaction = "[REDACTED]"

// redactHookOutputTokens applies a closed-set policy to hook output that will
// be persisted in an error:
//
//   - Exactly one complete JSON value has trustworthy structure, so token fields
//     can be replaced precisely.
//   - Every other non-empty payload has no trustworthy field boundaries, so the
//     complete payload is replaced.
//
// The second case deliberately does not scan for token-looking bytes. Once the
// JSON parser rejects an input, locating a smaller value inside it would mean
// guessing at the same open-ended JSON grammar that repeatedly leaked new input
// shapes. Over-redacting an error payload is safer than persisting a guessed
// partial redaction.
func redactHookOutputTokens(output string) string {
	if strings.TrimSpace(output) == "" {
		return output
	}

	redacted, parsed := redactHookJSONDocument(output)
	if !parsed {
		return hookOutputRedaction
	}
	return redacted
}

// redactHookJSONDocument accepts exactly one complete JSON value. json.Valid is
// intentionally the admission check: Decoder.Decode alone accepts a valid value
// followed by trailing malformed bytes, which would put an unparseable payload
// back into the supposedly precise branch.
func redactHookJSONDocument(document string) (string, bool) {
	value, parsed := decodeHookJSONDocument(document)
	if !parsed {
		return "", false
	}

	value, _ = redactHookJSONValue(value)
	// Always re-encode, even when traversal found no surviving token field. The
	// decoder collapses duplicate object members; returning the original bytes
	// would resurrect an overwritten earlier member that traversal could not see.
	encoded, err := json.Marshal(value)
	if err != nil {
		// Values produced by encoding/json are marshalable, but fail closed if that
		// invariant ever changes rather than returning the original token.
		return hookOutputRedaction, true
	}
	return string(encoded), true
}

func decodeHookJSONDocument(document string) (any, bool) {
	if !json.Valid([]byte(document)) {
		return nil, false
	}

	decoder := json.NewDecoder(strings.NewReader(document))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	return value, true
}

func redactHookJSONValue(value any) (any, bool) {
	switch value := value.(type) {
	case map[string]any:
		changed := false
		for key, child := range value {
			if strings.EqualFold(key, "token") {
				value[key] = hookOutputRedaction
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
		// A string beginning with a JSON container or string opener may itself be a
		// serialized document. Objects and arrays can structurally contain a token
		// field, while a string can wrap either one through further serialization.
		// Those three openers are the closed set that can eventually reach a token
		// field; ordinary diagnostic strings are not reinterpreted as payloads.
		if !hookJSONStringMayContainJSONDocument(value) {
			return value, false
		}
		redacted, parsed := redactHookJSONDocument(value)
		if !parsed {
			// The opener alone does not distinguish a serialized document from an
			// ordinary diagnostic such as "[INFO] connection failed".
			if malformedHookJSONDocumentContainsToken(value) {
				// Decoder.Token retains duplicate members and the valid prefix before a
				// syntax error, so malformed documents cannot hide an observed token key.
				return hookOutputRedaction, true
			}
			return value, false
		}
		return redacted, redacted != value
	default:
		return value, false
	}
}

func hookJSONStringMayContainJSONDocument(value string) bool {
	trimmed := strings.TrimSpace(value)
	trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "\uFEFF"))
	return trimmed != "" && (trimmed[0] == '{' || trimmed[0] == '[' || trimmed[0] == '"')
}

type hookJSONContainer struct {
	object       bool
	expectingKey bool
}

// malformedHookJSONDocumentContainsToken walks the trustworthy token prefix of
// a JSON-looking string. Unlike decoding into a map, the token stream preserves
// duplicate object members; unlike json.Valid, it exposes keys parsed before a
// missing closer or trailing syntax error. Strings encountered as values are
// checked recursively because hooks can serialize JSON through multiple layers.
func malformedHookJSONDocumentContainsToken(document string) bool {
	decoder := json.NewDecoder(strings.NewReader(document))
	decoder.UseNumber()
	var containers []hookJSONContainer

	for {
		token, err := decoder.Token()
		if err != nil {
			// A malformed value can stop Decoder.Token before a later object member.
			// Fall back to the narrower lexical shape of a valid JSON string named
			// "token" followed by a colon; arbitrary token-looking text is ignored.
			return malformedHookJSONDocumentContainsTokenKey(document)
		}

		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{':
				containers = append(containers, hookJSONContainer{object: true, expectingKey: true})
			case '[':
				containers = append(containers, hookJSONContainer{})
			case '}', ']':
				if len(containers) == 0 {
					return false
				}
				containers = containers[:len(containers)-1]
				if len(containers) > 0 {
					parent := &containers[len(containers)-1]
					if parent.object && !parent.expectingKey {
						parent.expectingKey = true
					}
				}
			}
			continue
		}

		if len(containers) == 0 {
			if nested, ok := token.(string); ok && hookJSONStringMayContainJSONDocument(nested) && malformedHookJSONDocumentContainsToken(nested) {
				return true
			}
			continue
		}
		container := &containers[len(containers)-1]
		if !container.object {
			if nested, ok := token.(string); ok && hookJSONStringMayContainJSONDocument(nested) && malformedHookJSONDocumentContainsToken(nested) {
				return true
			}
			continue
		}
		if container.expectingKey {
			key, ok := token.(string)
			if !ok {
				return false
			}
			if strings.EqualFold(key, "token") {
				return true
			}
			container.expectingKey = false
			continue
		}
		if nested, ok := token.(string); ok && hookJSONStringMayContainJSONDocument(nested) && malformedHookJSONDocumentContainsToken(nested) {
			return true
		}
		container.expectingKey = true
	}
}

func malformedHookJSONDocumentContainsTokenKey(document string) bool {
	for index := 0; index < len(document); index++ {
		// Examine every unescaped quote as a possible recovery boundary. Advancing
		// only to a previously assumed closing quote can consume the opening quote
		// of a later key when an earlier string contains an illegal raw newline.
		if document[index] != '"' ||
			(hookJSONQuoteEscaped(document, index) && !hookJSONKeyMayStartAt(document, index)) {
			continue
		}

		end := index + 1
		for end < len(document) && document[end] != '"' {
			if document[end] == '\\' {
				end++
			}
			end++
		}
		if end >= len(document) {
			// A serialized child can be the final, truncated string in the malformed
			// document. Close only that string literal for decoding; its decoded value
			// must still pass the JSON-opener gate before recursive inspection.
			var value string
			if err := json.Unmarshal([]byte(document[index:]+"\""), &value); err == nil &&
				hookJSONStringMayContainJSONDocument(value) && malformedHookJSONDocumentContainsToken(value) {
				return true
			}
			if malformedHookJSONStringContentsContainToken(document[index+1:]) {
				return true
			}
			continue
		}

		var value string
		if err := json.Unmarshal([]byte(document[index:end+1]), &value); err != nil {
			after := strings.TrimLeft(document[end+1:], " \t\r\n")
			if strings.HasPrefix(after, ":") && hookJSONKeyMayStartAt(document, index) &&
				malformedHookJSONKeyEqualsToken(document[index+1:end]) {
				return true
			}
			if malformedHookJSONStringContentsContainToken(document[index+1 : end]) {
				return true
			}
			continue
		}
		// Inspect serialized values before using the following colon to classify a
		// string as a key. Malformed input can put a colon after an otherwise valid
		// serialized child, and secrecy wins over trusting that broken boundary.
		if hookJSONStringMayContainJSONDocument(value) && malformedHookJSONDocumentContainsToken(value) {
			return true
		}
		after := strings.TrimLeft(document[end+1:], " \t\r\n")
		if strings.HasPrefix(after, ":") && hookJSONKeyMayStartAt(document, index) {
			if strings.EqualFold(value, "token") {
				return true
			}
			continue
		}
	}
	return false
}

func malformedHookJSONStringContentsContainToken(contents string) bool {
	var recovered strings.Builder
	var reassembled strings.Builder
	for len(contents) > 0 {
		boundary, skip := hookJSONStringRecoveryBoundary(contents)
		segment := contents[:boundary]
		recovered.WriteString(decodeHookJSONStringContentSegment(segment))
		reassembled.WriteString(segment)
		if skip == 0 {
			break
		}
		if contents[boundary] < ' ' {
			recovered.WriteByte(contents[boundary])
		} else {
			// Preserve a boundary where an invalid escape was removed so unrelated
			// words cannot be joined into a protected key spelling.
			recovered.WriteByte(' ')
			reassembled.WriteByte(' ')
		}
		contents = contents[boundary+skip:]
	}

	document := recovered.String()
	if malformedHookRecoveredDocumentContainsToken(document) {
		return true
	}

	// A raw control may split a Unicode escape that encodes the container opener.
	// Reassemble the original encoded segments without those already-invalid
	// controls, then decode them together so the split escape becomes whole.
	normalized := decodeHookJSONStringContentSegment(reassembled.String())
	if normalized == document {
		return false
	}
	return malformedHookRecoveredDocumentContainsToken(decodeHookJSONEscapedContainerOpeners(normalized))
}

func decodeHookJSONEscapedContainerOpeners(document string) string {
	var decoded strings.Builder
	decoded.Grow(len(document))
	for index := 0; index < len(document); index++ {
		if index+1 < len(document) && document[index] == '\\' && document[index+1] == '"' {
			decoded.WriteByte('"')
			index++
			continue
		}
		if index+5 < len(document) && document[index] == '\\' && document[index+1] == 'u' {
			codepoint := document[index+2 : index+6]
			switch {
			case strings.EqualFold(codepoint, "007b"):
				decoded.WriteByte('{')
				index += 5
				continue
			case strings.EqualFold(codepoint, "005b"):
				decoded.WriteByte('[')
				index += 5
				continue
			}
		}
		decoded.WriteByte(document[index])
	}
	return decoded.String()
}

func malformedHookRecoveredDocumentContainsToken(document string) bool {
	opener := strings.IndexAny(document, "{[")
	if opener < 0 {
		return false
	}
	// The malformed-document fallback scans the complete recovered suffix, so
	// starting it once at the first possible container covers every later opener
	// without repeatedly rescanning overlapping suffixes.
	return malformedHookJSONDocumentContainsToken(document[opener:])
}

func hookJSONKeyMayStartAt(document string, index int) bool {
	for index > 0 {
		for index > 0 && (document[index-1] <= ' ' || document[index-1] == '\\') {
			index--
		}
		if index >= 2 && document[index-2:index] == "*/" {
			comment := strings.LastIndex(document[:index-2], "/*")
			if comment < 0 {
				return false
			}
			index = comment
			continue
		}

		line := strings.LastIndexByte(document[:index], '\n') + 1
		if comment := strings.LastIndex(document[line:index], "//"); comment >= 0 {
			index = line + comment
			continue
		}
		return document[index-1] == '{' || document[index-1] == ','
	}
	return false
}

func malformedHookJSONKeyEqualsToken(contents string) bool {
	var normalized strings.Builder
	normalized.Grow(len(contents))
	for _, character := range contents {
		if character >= ' ' {
			normalized.WriteRune(character)
		}
	}

	var key string
	if err := json.Unmarshal([]byte("\""+normalized.String()+"\""), &key); err != nil {
		return false
	}
	return strings.EqualFold(key, "token")
}

func decodeHookJSONStringContentSegment(contents string) string {
	for {
		var decoded string
		if err := json.Unmarshal([]byte("\""+contents+"\""), &decoded); err != nil || decoded == contents {
			return contents
		}
		contents = decoded
	}
}

func hookJSONStringRecoveryBoundary(contents string) (int, int) {
	for index := 0; index < len(contents); index++ {
		if contents[index] < ' ' {
			return index, 1
		}
		if contents[index] != '\\' {
			continue
		}

		if index+1 >= len(contents) {
			return index, 1
		}
		switch contents[index+1] {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			index++
		case 'u':
			if index+5 >= len(contents) {
				return index, 1
			}
			for offset := 2; offset <= 5; offset++ {
				if contents[index+offset] < ' ' {
					return index + offset, 1
				}
				if !isHookJSONHex(contents[index+offset]) {
					return index, 1
				}
			}
			index += 5
		default:
			return index, 2
		}
	}
	return len(contents), 0
}

func isHookJSONHex(character byte) bool {
	return character >= '0' && character <= '9' ||
		character >= 'a' && character <= 'f' ||
		character >= 'A' && character <= 'F'
}

func hookJSONQuoteEscaped(document string, index int) bool {
	backslashes := 0
	for index > 0 && document[index-1] == '\\' {
		backslashes++
		index--
	}
	return backslashes%2 != 0
}
