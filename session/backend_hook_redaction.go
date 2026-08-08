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
	if !json.Valid([]byte(document)) {
		return "", false
	}

	decoder := json.NewDecoder(strings.NewReader(document))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", false
	}

	value, changed := redactHookJSONValue(value)
	if !changed {
		return document, true
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		// Values produced by encoding/json are marshalable, but fail closed if that
		// invariant ever changes rather than returning the original token.
		return hookOutputRedaction, true
	}
	return string(encoded), true
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
		// A string may itself be one complete serialized JSON value. Preserve the
		// existing exact path for those values, but do not guess that an arbitrary
		// string which fails JSON validation was intended to be structured data.
		redacted, parsed := redactHookJSONDocument(value)
		if !parsed {
			return value, false
		}
		return redacted, redacted != value
	default:
		return value, false
	}
}
