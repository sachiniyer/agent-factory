// The comparator half of provenance resolution, split from provenance.go when
// that file approached the 1000-line limit (#1145).
//
// resolveReplace and resolveComposite both have to answer "did the loader
// change this configured value before resolution?" to decide whether a
// candidate's reason earns the load-time-normalization note. This file holds
// that layer end to end: jsonEquivalent, and the struct-versus-map alignment
// it performs so a raw decoded map[string]any compares symmetrically against
// the typed struct it decoded into despite omitempty and absent-field marshal
// asymmetries (#4727). provenance_omitempty_test.go is its dedicated guard.
package config

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
)

func jsonEquivalent(left, right any) bool {
	left, right, _ = alignStructMapForComparison(left, right)
	canonical := func(value any) (any, error) {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.UseNumber()
		var decoded any
		if err := decoder.Decode(&decoded); err != nil {
			return nil, err
		}
		return decoded, nil
	}
	leftValue, leftErr := canonical(left)
	rightValue, rightErr := canonical(right)
	return leftErr == nil && rightErr == nil && reflect.DeepEqual(leftValue, rightValue)
}

// alignStructMapForComparison makes a map-versus-struct comparison invariant
// to struct json tags, most importantly omitempty, so jsonEquivalent never
// reports a false value difference caused by the serialization asymmetry between
// a raw decoded map and the typed struct it decoded into.
//
// jsonEquivalent canonicalizes each side with json.Marshal. json.Marshal drops
// zero-valued struct fields tagged omitempty but keeps every key the user wrote
// in a raw map[string]any. So when one operand is a configured map and the other
// is its typed struct, an explicit zero on an omitempty field (for example
// remote_hooks.provision_cmd = "") survives on only the map side, the two
// marshal outputs diverge, and jsonEquivalent reports a difference even though
// the loader performed no normalization.
//
// The fix operates directly on the raw map without re-decoding through the struct
// type, so it is not lossy:
//
//   - Zero-valued omitempty keys are removed from a copy of the map so they
//     match the struct's marshal output (which also omits them).
//   - Absent non-omitempty struct fields are added to the copy with their zero
//     value (the value json.Marshal always emits for the struct), so the
//     comparison is not tripped by a key the loader never touched.
//   - Unknown fields (not present in the struct at all) and explicit non-zero
//     values — including JSON null on a known scalar — are left untouched. A
//     null that the loader coerced to "" still differs from the empty string the
//     struct holds, so the normalization note fires correctly.
//
// Only a struct (or pointer to one) opposite a map is aligned. Every other
// pairing — struct/struct, map/map, scalar/scalar — already marshals
// symmetrically and is returned unchanged, so existing callers keep their
// exact behavior.
func alignStructMapForComparison(left, right any) (any, any, bool) {
	leftStruct, rightStruct := jsonObjectStructType(left), jsonObjectStructType(right)
	var (
		structType reflect.Type
		mapSide    any
		mapIsLeft  bool
	)
	switch {
	case leftStruct != nil && isJSONMap(right):
		structType, mapSide, mapIsLeft = leftStruct, right, false
	case rightStruct != nil && isJSONMap(left):
		structType, mapSide, mapIsLeft = rightStruct, left, true
	default:
		return left, right, false
	}

	// Collect the struct's JSON field metadata: their names, whether they carry
	// omitempty, and the zero value each field produces when marshaled.
	fields := structJSONFields(structType)
	if len(fields) == 0 {
		return left, right, false
	}

	raw, ok := mapSide.(map[string]any)
	if !ok {
		return left, right, false
	}
	trimmed := make(map[string]any, len(raw))
	for k, v := range raw {
		trimmed[k] = v
	}

	for key, fi := range fields {
		if v, present := trimmed[key]; present {
			// Key is present in the map. For omitempty fields: if the user wrote
			// the type-specific zero (e.g. "" for a string field), remove it so
			// it matches the struct's omitted marshal output. An explicit JSON null
			// (decoded as nil) is intentionally left because the loader coerces it
			// to the string zero, producing a genuine difference. Only the same
			// typed zero that the struct field would produce is an artifact.
			if fi.omitempty && reflect.DeepEqual(v, fi.typeZero) {
				delete(trimmed, key)
			}
		} else if !fi.omitempty {
			// Key is absent from the map but the struct always emits it. Add the
			// zero value so both sides agree that no loader change happened.
			trimmed[key] = fi.zeroValue
		}
	}

	if mapIsLeft {
		return trimmed, right, true
	}
	return left, trimmed, true
}

// jsonFieldMeta holds per-field alignment metadata derived from a struct's json
// tags and its zero-value marshal output.
type jsonFieldMeta struct {
	omitempty bool
	zeroValue any // JSON-decoded zero for non-omitempty fields (always emitted by zero struct)
	typeZero  any // type-specific JSON-decoded zero for omitempty fields (e.g. "" for string)
}

// structJSONFields returns a map from JSON key name to field metadata for the
// top-level fields of t. Only fields with a json tag are included; fields
// tagged "-" are excluded. Embedded structs are not flattened because the raw
// decoded map also does not flatten them.
func structJSONFields(t reflect.Type) map[string]jsonFieldMeta {
	// Serialize a zero-valued instance of the struct to discover the JSON-level
	// zero for each non-omitempty field (the value json.Marshal always emits).
	zeroInstance := reflect.New(t).Interface()
	encoded, err := json.Marshal(zeroInstance)
	if err != nil {
		return nil
	}
	var zeroMap map[string]any
	if err := json.Unmarshal(encoded, &zeroMap); err != nil {
		return nil
	}

	out := make(map[string]jsonFieldMeta)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		parts := strings.SplitN(tag, ",", 2)
		name := parts[0]
		if name == "" {
			name = f.Name
		}
		var hasOmitempty bool
		if len(parts) == 2 {
			for _, opt := range strings.Split(parts[1], ",") {
				if opt == "omitempty" {
					hasOmitempty = true
					break
				}
			}
		}
		out[name] = jsonFieldMeta{
			omitempty: hasOmitempty,
			zeroValue: zeroMap[name],          // nil when omitempty (not in zero-marshal output)
			typeZero:  goTypeJSONZero(f.Type), // type-specific zero for omitempty comparison
		}
	}
	return out
}

// goTypeJSONZero returns the value that json.Unmarshal produces for the zero
// of the given Go type when decoded into an any. This is used to compare raw
// map values against the type-specific zero for omitempty fields: a Go string
// zero is "" (not nil), a bool zero is false, numeric zeros are 0, and pointer
// or interface zeros are nil. Slice and map zeros are nil. If the type is not
// one of the recognizable scalars the function returns nil.
func goTypeJSONZero(t reflect.Type) any {
	for t.Kind() == reflect.Pointer {
		// A nil pointer marshals to JSON null, which decodes back to nil.
		return nil
	}
	switch t.Kind() {
	case reflect.String:
		return ""
	case reflect.Bool:
		return false
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		// json.Unmarshal into any decodes numbers as float64.
		return float64(0)
	case reflect.Slice, reflect.Map, reflect.Interface:
		return nil
	}
	return nil
}

// isJSONMap reports whether value is a Go map. A raw decoded config shape is
// always a map[string]any for a table, and that is the only side alignment ever
// re-encodes; structs and other operands are rejected here so a struct/struct
// pair stays on the original code path.
func isJSONMap(value any) bool {
	if value == nil {
		return false
	}
	return reflect.TypeOf(value).Kind() == reflect.Map
}

// jsonObjectStructType returns the struct type behind value after unwrapping
// pointers, or nil when value is not a struct or pointer to one. Nil pointers
// carry a type and are still treated as a struct side, so json.Marshal's null
// rendering of a nil pointer compares against the re-encoded map consistently
// with the comparison the caller performed before this guard.
func jsonObjectStructType(value any) reflect.Type {
	if value == nil {
		return nil
	}
	t := reflect.TypeOf(value)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	return t
}
