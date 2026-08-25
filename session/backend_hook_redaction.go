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
		// A string can itself be a serialized payload, so it gets the same closed-set
		// rule the top level applies to the whole output: parse it, or replace it.
		redacted, parsed := redactHookJSONDocument(value)
		if parsed {
			return redacted, redacted != value
		}
		if hookStringMayCarrySerializedObject(value) {
			return hookOutputRedaction, true
		}
		return value, false
	default:
		return value, false
	}
}

// hookStringMayCarrySerializedObject reports whether a string the JSON parser
// rejected could still be carrying a serialized object, and with it a token
// field.
//
// A token field exists only inside an object, and an object needs a `{`. That
// byte reaches an unparseable string in exactly two spellings: literally, or
// escaped for a serialization layer this decode did not unwrap — `{`, or a
// `{` behind further backslashes. A backslash is the only way to write the
// second, so `{` and `\` together close the set: a string holding neither cannot
// name a token field however many times it is re-parsed, and survives
// byte-exact. That keeps ordinary diagnostics — "[INFO] connection failed",
// "quota exceeded", `he said "token": no` — readable in the error.
//
// The inverse is deliberately blunt. Once a rejected string does hold an object
// opener, this replaces the whole string instead of hunting for the token field
// inside it. That hunt is the open-ended half of the JSON grammar — escapes,
// truncation, raw controls, duplicate members, comments, recovery after a syntax
// error — and each round of hardening it received answered one spelling while
// leaving the next one reachable. Absence of evidence is not the property a
// redaction boundary can be built on, so the boundary is drawn where the parser
// stops instead: over-redacting a brace-bearing diagnostic costs a line of
// context, and under-redacting one persists a credential.
func hookStringMayCarrySerializedObject(value string) bool {
	return strings.ContainsAny(value, `{\`)
}
