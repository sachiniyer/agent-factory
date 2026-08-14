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
		trimmed := strings.TrimSpace(value)
		if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[' && trimmed[0] != '"') {
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
			return false
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
			if nested, ok := token.(string); ok && malformedHookJSONDocumentContainsToken(nested) {
				return true
			}
			continue
		}
		container := &containers[len(containers)-1]
		if !container.object {
			if nested, ok := token.(string); ok && malformedHookJSONDocumentContainsToken(nested) {
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
		if nested, ok := token.(string); ok && malformedHookJSONDocumentContainsToken(nested) {
			return true
		}
		container.expectingKey = true
	}
}
