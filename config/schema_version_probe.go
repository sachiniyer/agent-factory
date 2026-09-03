package config

import (
	"encoding/json"
)

// ProveJSONSchemaVersion reports whether raw is PROVABLY already at version
// want under DetectJSONSchemaVersion's rules, without decoding the document.
//
// It is the fast path for SchemaMigrationPlan.ProveCurrentVersion, and it exists
// because the honest answer was costing 8.5 MB and 92k allocations per read of a
// 1.36 MB instances.json (#3652): DetectJSONSchemaVersion decodes the whole file
// into map[string]any and reflect-grown slices just to look at one integer, and
// the daemon re-runs it on every poll of a file that has had nothing to migrate
// since it was written.
//
// The contract is one-directional and deliberately weak: a true answer must be
// correct, a false answer costs only the full path that would have run anyway.
// So every case this scanner is not certain about — an escaped key, a version
// that is not a plain JSON integer literal, anything that is not a JSON object
// at the root — answers false and lets DetectJSONSchemaVersion decide.
//
// Three things it must NOT get wrong, each of which would make the fast path
// skip a migration the store actually needed:
//
//   - Case. encoding/json matches struct tags case-insensitively, so decoding
//     into a `json:"schema_version"` field would read {"SCHEMA_VERSION":1} as
//     version 1 while DetectJSONSchemaVersion — a plain map lookup — reads it as
//     legacy v0. That is why this walks the object's keys and compares their
//     bytes rather than unmarshalling into a struct.
//   - Duplicates. A decoder assigns each member in order, so the LAST
//     schema_version wins. So does this scan.
//   - Well-formedness. DetectJSONSchemaVersion rejects malformed JSON and
//     trailing data, and callers classify those errors. json.Valid re-establishes
//     that for the whole document before the structural scan below trusts a
//     single byte of it. It is a byte scan, not a parse: its scanner comes from a
//     sync.Pool, so it allocates only when the pool has been drained — 101 bytes
//     per operation amortized over a 1.36 MB file, against the 8.5 MB the decode
//     it replaces cost.
func ProveJSONSchemaVersion(raw []byte, want int) bool {
	if want < 0 || !json.Valid(raw) {
		return false
	}
	value, found := lastTopLevelJSONMember(raw, SchemaVersionField)
	if !found || len(value) == 0 {
		return false
	}
	// DetectJSONSchemaVersion reads the value with UseNumber and then parses
	// json.Number.String(), which is the literal text. Anything that is not a
	// number literal errors there, so it is not provable here.
	if first := value[0]; first != '-' && (first < '0' || first > '9') {
		return false
	}
	version, err := schemaVersionFromString(string(value))
	if err != nil {
		return false
	}
	return version == want
}

// lastTopLevelJSONMember returns the raw text of the last member named exactly
// name in raw's root JSON object, and whether it found one.
//
// raw MUST already have passed json.Valid: the value skipping below counts
// brackets and quotes rather than parsing, which is exact on well-formed input
// and meaningless on anything else. Every unexpected byte returns false, so a
// caller that forgets the json.Valid check gets a refusal to prove rather than a
// wrong proof — but it would also silently lose the malformed-input error the
// full path raises, which is why the check is not optional.
func lastTopLevelJSONMember(raw []byte, name string) ([]byte, bool) {
	i := skipJSONSpace(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return nil, false
	}
	i++
	var value []byte
	found := false
	for {
		i = skipJSONSpace(raw, i)
		if i >= len(raw) {
			return nil, false
		}
		if raw[i] == '}' {
			return value, found
		}
		if raw[i] != '"' {
			return nil, false
		}
		keyEnd, ok := endOfJSONString(raw, i)
		if !ok {
			return nil, false
		}
		key := raw[i+1 : keyEnd-1]
		if hasJSONEscape(key) {
			// "schema_version" decodes to the name without matching its
			// bytes, so a byte comparison could disagree with the decoder about
			// which member wins. Stop proving instead.
			return nil, false
		}
		i = skipJSONSpace(raw, keyEnd)
		if i >= len(raw) || raw[i] != ':' {
			return nil, false
		}
		i = skipJSONSpace(raw, i+1)
		start := i
		end, ok := endOfJSONValue(raw, i)
		if !ok {
			return nil, false
		}
		if string(key) == name {
			value = raw[start:end]
			found = true
		}
		i = skipJSONSpace(raw, end)
		if i >= len(raw) {
			return nil, false
		}
		switch raw[i] {
		case ',':
			i++
		case '}':
			return value, found
		default:
			return nil, false
		}
	}
}

// endOfJSONValue returns the index one past the JSON value starting at i.
func endOfJSONValue(raw []byte, i int) (int, bool) {
	if i >= len(raw) {
		return 0, false
	}
	switch raw[i] {
	case '"':
		return endOfJSONString(raw, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(raw); j++ {
			switch raw[j] {
			case '"':
				end, ok := endOfJSONString(raw, j)
				if !ok {
					return 0, false
				}
				j = end - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1, true
				}
			}
		}
		return 0, false
	default:
		// A number, true, false or null: it ends where the enclosing object
		// resumes. Only the object form matters here, but ']' is cheap to
		// accept and keeps this usable for a value nested one level down.
		for j := i; j < len(raw); j++ {
			switch raw[j] {
			case ',', '}', ']', ' ', '\t', '\r', '\n':
				return j, true
			}
		}
		return 0, false
	}
}

// endOfJSONString returns the index one past the string starting at raw[i],
// which must be its opening quote.
func endOfJSONString(raw []byte, i int) (int, bool) {
	for j := i + 1; j < len(raw); j++ {
		switch raw[j] {
		case '\\':
			j++
		case '"':
			return j + 1, true
		}
	}
	return 0, false
}

func hasJSONEscape(key []byte) bool {
	for _, b := range key {
		if b == '\\' {
			return true
		}
	}
	return false
}

func skipJSONSpace(raw []byte, i int) int {
	for i < len(raw) {
		switch raw[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}
