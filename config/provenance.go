package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// SourceRef identifies the exact configuration location that supplied an
// effective scalar or composite leaf.
type SourceRef struct {
	Layer   string `json:"layer"`
	Path    string `json:"path,omitempty"`
	Format  string `json:"format,omitempty"`
	KeyPath string `json:"key_path"`
}

// CandidateTrace describes one source considered while resolving a key.
// Value is the configured value from that source (before load-time
// normalization); absent and disallowed candidates carry null.
type CandidateTrace struct {
	Layer   string `json:"layer"`
	Path    string `json:"path,omitempty"`
	Format  string `json:"format,omitempty"`
	KeyPath string `json:"key_path"`
	// Allowed means the manifest admits this layer as a read candidate. A
	// compatibility-only layer can therefore be allowed here without being a
	// supported write destination in ManifestEntry.Sources.
	Allowed bool   `json:"allowed"`
	Present bool   `json:"present"`
	Value   any    `json:"value"`
	Result  string `json:"result"`
	Reason  string `json:"reason"`
}

// ResolvedValue is an effective config value and the complete trace that
// produced it. Replace/list values have Winner; map/table values instead carry
// a source per leaf in Origins so a composite is never assigned a fake winner.
type ResolvedValue struct {
	Key        string               `json:"key"`
	Value      any                  `json:"value"`
	Default    string               `json:"default,omitempty"`
	Merge      string               `json:"merge"`
	Precedence []string             `json:"precedence"`
	Winner     *SourceRef           `json:"winner,omitempty"`
	Origins    map[string]SourceRef `json:"origins,omitempty"`
	Candidates []CandidateTrace     `json:"candidates"`
}

type sourceDocument struct {
	layer          ConfigSource
	metadata       sourceMetadata
	schemas        []any
	valueSemantics sourceValueSemantics
}

// sourceValueSemantics says whether a decoded schema is one source's delta or
// the complete effective snapshot through that source. Global Config is
// decoded on top of DefaultConfig, so treating it as a delta can resurrect a
// built-in map entry that the loader removed (for example a TOML inline map).
type sourceValueSemantics uint8

const (
	sourceValueDelta sourceValueSemantics = iota
	sourceValueSnapshot
)

func (d sourceDocument) isBuiltIn() bool {
	return d.layer == SourceBuiltIn
}

type computedValue struct {
	resolved ResolvedValue
	value    reflect.Value
}

type sourceCandidate struct {
	document         sourceDocument
	traceIndex       int
	typed            reflect.Value
	leaves           map[string]reflect.Value
	configuredCount  int
	normalizedCount  int
	removedInherited int
}

func resolveManifest(entries []ManifestEntry, documents []sourceDocument, requireAllSources bool) ([]computedValue, error) {
	// Source constants are the canonical low-to-high order. Sort a private copy
	// so an obvious caller mistake (passing loaded documents in read order) can
	// never change precedence; only the manifest and ConfigSource ordering may.
	documents = append([]sourceDocument(nil), documents...)
	sort.Slice(documents, func(i, j int) bool {
		return documents[i].layer < documents[j].layer
	})
	byLayer := make(map[ConfigSource]bool, len(documents))
	for _, document := range documents {
		if document.valueSemantics != sourceValueDelta && document.valueSemantics != sourceValueSnapshot {
			return nil, fmt.Errorf("config resolver received invalid value semantics %d for %s source", document.valueSemantics, document.layer)
		}
		if byLayer[document.layer] {
			return nil, fmt.Errorf("config resolver received duplicate %s source", document.layer)
		}
		byLayer[document.layer] = true
	}

	resolved := make([]computedValue, 0, len(entries))
	for _, entry := range entries {
		availablePrecedence := make([]ConfigSource, 0, len(entry.Precedence))
		for _, layer := range entry.Precedence {
			if byLayer[layer] {
				availablePrecedence = append(availablePrecedence, layer)
				continue
			}
			if requireAllSources {
				return nil, fmt.Errorf("config resolver has no %s candidate required by manifest key %q", layer, entry.Key)
			}
		}
		entry.Precedence = availablePrecedence
		value, err := resolveManifestEntry(entry, documents)
		if err != nil {
			return nil, fmt.Errorf("resolve config key %q: %w", entry.Key, err)
		}
		resolved = append(resolved, value)
	}
	return resolved, nil
}

func resolveManifestEntry(entry ManifestEntry, documents []sourceDocument) (computedValue, error) {
	precedence := make([]string, len(entry.Precedence))
	for i, layer := range entry.Precedence {
		precedence[i] = layer.String()
	}
	result := ResolvedValue{
		Key:        entry.Key,
		Default:    entry.Default,
		Merge:      entry.Merge.String(),
		Precedence: precedence,
		Candidates: make([]CandidateTrace, 0, len(documents)),
	}

	candidates := make([]sourceCandidate, 0, len(documents))
	for _, document := range documents {
		allowed := sourceInPrecedence(document.layer, entry.Precedence)
		configured, present, keyPath := document.metadata.topLevelWithKeyPath(entry.Key)
		if document.isBuiltIn() {
			present = allowed
		}
		trace := CandidateTrace{
			Layer:   document.layer.String(),
			Path:    document.metadata.path,
			Format:  sourceFormatName(document.metadata.format),
			KeyPath: keyPath,
			Allowed: allowed,
			Present: present,
			Value:   nil,
		}
		if present {
			if document.isBuiltIn() {
				value, ok := valueFromSchemas(document.schemas, entry.Key)
				if !ok {
					return computedValue{}, fmt.Errorf("built-in source has no typed field")
				}
				trace.Value = clonedInterface(value)
			} else {
				trace.Value = configured
			}
		}
		result.Candidates = append(result.Candidates, trace)
		candidate := sourceCandidate{document: document, traceIndex: len(result.Candidates) - 1}
		if allowed && present {
			var ok bool
			candidate.typed, ok = valueFromSchemas(document.schemas, entry.Key)
			if !ok {
				return computedValue{}, fmt.Errorf("%s is allowed and present but has no typed field", document.layer)
			}
		}
		candidates = append(candidates, candidate)
	}

	switch entry.Merge {
	case MergeReplace, MergeListReplace:
		return resolveReplace(entry, result, candidates)
	case MergeMapByKey, MergeTableByField:
		return resolveComposite(entry, result, candidates)
	default:
		return computedValue{}, fmt.Errorf("unsupported merge policy %s", entry.Merge)
	}
}

func resolveReplace(entry ManifestEntry, result ResolvedValue, candidates []sourceCandidate) (computedValue, error) {
	winner := -1
	for i := range candidates {
		trace := &result.Candidates[candidates[i].traceIndex]
		setNonParticipantResult(trace)
		if trace.Allowed && trace.Present {
			winner = i
		}
	}
	if winner < 0 {
		return computedValue{}, fmt.Errorf("no present candidate (built-in must always participate)")
	}

	winnerTrace := &result.Candidates[candidates[winner].traceIndex]
	winnerTrace.Result = "winner"
	winnerTrace.Reason = "highest-precedence present allowed source"
	if !candidates[winner].document.isBuiltIn() &&
		!jsonEquivalent(winnerTrace.Value, clonedInterface(candidates[winner].typed)) {
		winnerTrace.Reason += "; load-time normalization changed the configured value before resolution"
	}
	ref := sourceReference(candidates[winner].document, winnerTrace.KeyPath)
	result.Winner = &ref

	for i := range candidates {
		if i == winner {
			continue
		}
		trace := &result.Candidates[candidates[i].traceIndex]
		if trace.Allowed && trace.Present {
			trace.Result = "shadowed"
			trace.Reason = "overridden by higher-precedence " + winnerTrace.Layer
		}
	}

	return computedValue{resolved: result, value: cloneExportedValue(candidates[winner].typed)}, nil
}

func resolveComposite(entry ManifestEntry, result ResolvedValue, candidates []sourceCandidate) (computedValue, error) {
	targetType, ok := manifestValueType(entry.Key)
	if !ok {
		return computedValue{}, fmt.Errorf("manifest key has no schema field")
	}

	leafValues := make(map[string]reflect.Value)
	origins := make(map[string]SourceRef)
	materialized := false
	for i := range candidates {
		candidate := &candidates[i]
		trace := &result.Candidates[candidate.traceIndex]
		setNonParticipantResult(trace)
		if !trace.Allowed || !trace.Present {
			continue
		}

		configured, _ := candidate.document.metadata.topLevel(entry.Key)
		leaves, configuredCount, normalizedCount, candidateMaterialized, err := compositeLeaves(
			candidate.typed, configured, candidate.document.isBuiltIn())
		if err != nil {
			return computedValue{}, err
		}
		candidate.leaves = leaves
		candidate.configuredCount = configuredCount
		candidate.normalizedCount = normalizedCount

		if candidate.document.valueSemantics == sourceValueSnapshot {
			full, _, _, snapshotMaterialized, err := compositeLeaves(candidate.typed, nil, true)
			if err != nil {
				return computedValue{}, fmt.Errorf("read materialized %s snapshot: %w", candidate.document.layer, err)
			}
			for leaf := range leafValues {
				if _, retained := full[leaf]; retained {
					continue
				}
				delete(leafValues, leaf)
				delete(origins, leaf)
				candidate.removedInherited++
			}
			for leaf, value := range full {
				if explicit, configuredHere := leaves[leaf]; configuredHere {
					leafValues[leaf] = cloneExportedValue(explicit)
					origins[leaf] = sourceReference(candidate.document, leafKeyPath(entry.Key, leaf))
					continue
				}
				inherited, present := leafValues[leaf]
				if !present {
					return computedValue{}, fmt.Errorf("%s snapshot materialized unconfigured leaf %q with no lower-precedence origin", candidate.document.layer, leaf)
				}
				if !jsonEquivalent(clonedInterface(inherited), clonedInterface(value)) {
					return computedValue{}, fmt.Errorf("%s snapshot changed unconfigured inherited leaf %q", candidate.document.layer, leaf)
				}
			}
			materialized = snapshotMaterialized
			continue
		}

		materialized = materialized || candidateMaterialized
		for leaf, value := range leaves {
			leafValues[leaf] = cloneExportedValue(value)
			origins[leaf] = sourceReference(candidate.document, leafKeyPath(entry.Key, leaf))
		}
	}

	for i := range candidates {
		candidate := &candidates[i]
		trace := &result.Candidates[candidate.traceIndex]
		if !trace.Allowed || !trace.Present {
			continue
		}
		surviving := 0
		for leaf := range candidate.leaves {
			if origin, present := origins[leaf]; present && origin.Layer == trace.Layer {
				surviving++
			}
		}
		ignored := candidate.configuredCount - len(candidate.leaves)
		switch {
		case len(candidate.leaves) == 0 && candidate.removedInherited > 0:
			trace.Result = "replaced"
			trace.Reason = fmt.Sprintf("loaded value removes %d lower-precedence %s and contributes no entries",
				candidate.removedInherited, pluralize("entry", candidate.removedInherited))
			if ignored > 0 {
				trace.Reason += fmt.Sprintf("; load-time validation removed %d configured %s",
					ignored, pluralize("entry", ignored))
			}
		case len(candidate.leaves) == 0 && ignored > 0:
			trace.Result = "ignored"
			trace.Reason = "all configured entries were removed by load-time validation"
		case len(candidate.leaves) == 0:
			trace.Result = "empty"
			trace.Reason = "present but contains no entries"
		case surviving == len(candidate.leaves):
			trace.Result = "contributed"
			trace.Reason = fmt.Sprintf("contributes %d effective %s", surviving, pluralize("entry", surviving))
		case surviving == 0:
			trace.Result = "shadowed"
			trace.Reason = "all entries are overridden by higher-precedence sources"
		default:
			trace.Result = "partially-shadowed"
			trace.Reason = fmt.Sprintf("contributes %d %s; %d overridden by higher-precedence sources",
				surviving, pluralize("entry", surviving), len(candidate.leaves)-surviving)
		}
		if ignored > 0 && len(candidate.leaves) > 0 {
			trace.Reason += fmt.Sprintf("; load-time validation removed %d configured %s", ignored, pluralize("entry", ignored))
		}
		if candidate.removedInherited > 0 && len(candidate.leaves) > 0 {
			trace.Reason += fmt.Sprintf("; loaded value also removes %d lower-precedence %s",
				candidate.removedInherited, pluralize("entry", candidate.removedInherited))
		}
		if candidate.normalizedCount > 0 {
			trace.Reason += fmt.Sprintf("; load-time normalization changed %d configured %s",
				candidate.normalizedCount, pluralize("entry", candidate.normalizedCount))
		}
	}

	value, err := buildComposite(targetType, leafValues, materialized)
	if err != nil {
		return computedValue{}, err
	}
	if len(origins) > 0 {
		result.Origins = origins
	}
	return computedValue{resolved: result, value: value}, nil
}

func setNonParticipantResult(trace *CandidateTrace) {
	switch {
	case !trace.Allowed:
		trace.Result = "disallowed"
		trace.Reason = "manifest policy does not allow this key at this source"
	case !trace.Present:
		trace.Result = "absent"
		trace.Reason = "key is not present at this source"
	}
}

func sourceInPrecedence(source ConfigSource, precedence []ConfigSource) bool {
	for _, candidate := range precedence {
		if candidate == source {
			return true
		}
	}
	return false
}

func sourceReference(document sourceDocument, keyPath string) SourceRef {
	return SourceRef{
		Layer:   document.layer.String(),
		Path:    document.metadata.path,
		Format:  sourceFormatName(document.metadata.format),
		KeyPath: keyPath,
	}
}

func sourceFormatName(format ConfigFormat) string {
	if format == FormatInvalid {
		return ""
	}
	return format.String()
}

func valueFromSchemas(schemas []any, key string) (reflect.Value, bool) {
	for _, schema := range schemas {
		if field, ok := taggedFieldByKey(reflect.ValueOf(schema), key); ok {
			return field, true
		}
	}
	return reflect.Value{}, false
}

func taggedFieldByKey(value reflect.Value, key string) (reflect.Value, bool) {
	if alias, ok := configAliasForCanonical(canonicalConfigKey(key)); ok {
		key = alias.legacy
	}
	for value.IsValid() && value.Kind() == reflect.Pointer {
		if value.IsNil() {
			value = reflect.Zero(value.Type().Elem())
			break
		}
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return reflect.Value{}, false
	}
	typeOf := value.Type()
	for i := 0; i < value.NumField(); i++ {
		field := typeOf.Field(i)
		if !field.IsExported() {
			continue
		}
		if structTagName(field.Tag.Get("toml")) == key || structTagName(field.Tag.Get("json")) == key || field.Tag.Get("config") == key {
			return value.Field(i), true
		}
	}
	return reflect.Value{}, false
}

func manifestValueType(key string) (reflect.Type, bool) {
	for _, schema := range []any{Config{}, InRepoConfig{}} {
		if field, ok := taggedFieldByKey(reflect.ValueOf(schema), key); ok {
			return field.Type(), true
		}
	}
	return nil, false
}

func compositeLeaves(value reflect.Value, configured any, builtIn bool) (map[string]reflect.Value, int, int, bool, error) {
	materialized := false
	for value.IsValid() && value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return map[string]reflect.Value{}, configuredChildCount(configured, builtIn), 0, false, nil
		}
		value = value.Elem()
	}
	if !value.IsValid() {
		return nil, 0, 0, false, fmt.Errorf("composite candidate has no typed value")
	}
	materialized = value.Kind() != reflect.Map || !value.IsNil()

	var names []string
	if builtIn {
		names = compositeNames(value)
	} else if configuredMap, ok := configured.(map[string]any); ok {
		for name := range configuredMap {
			names = append(names, name)
		}
		sort.Strings(names)
		materialized = true
	} else {
		// JSON null is a present, typed nil table. It contributes no fields but
		// remains visible as a present candidate in the trace.
		return map[string]reflect.Value{}, 0, 0, false, nil
	}

	leaves := make(map[string]reflect.Value, len(names))
	normalized := 0
	for _, name := range names {
		leaf, ok := compositeLeaf(value, name)
		if ok {
			leaves[name] = leaf
			if configuredMap, mapped := configured.(map[string]any); !builtIn && mapped {
				if !jsonEquivalent(configuredMap[name], clonedInterface(leaf)) {
					normalized++
				}
			}
		}
	}
	return leaves, len(names), normalized, materialized, nil
}

func compositeNames(value reflect.Value) []string {
	var names []string
	switch value.Kind() {
	case reflect.Map:
		if value.IsNil() {
			return nil
		}
		iter := value.MapRange()
		for iter.Next() {
			names = append(names, fmt.Sprint(iter.Key().Interface()))
		}
	case reflect.Struct:
		typeOf := value.Type()
		for i := 0; i < value.NumField(); i++ {
			field := typeOf.Field(i)
			if !field.IsExported() {
				continue
			}
			name := structTagName(field.Tag.Get("toml"))
			if name != "" && name != "-" {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names
}

func compositeLeaf(value reflect.Value, name string) (reflect.Value, bool) {
	switch value.Kind() {
	case reflect.Map:
		key := reflect.ValueOf(name)
		if !key.Type().AssignableTo(value.Type().Key()) {
			if !key.Type().ConvertibleTo(value.Type().Key()) {
				return reflect.Value{}, false
			}
			key = key.Convert(value.Type().Key())
		}
		leaf := value.MapIndex(key)
		return leaf, leaf.IsValid()
	case reflect.Struct:
		return taggedFieldByKey(value, name)
	default:
		return reflect.Value{}, false
	}
}

func configuredChildCount(configured any, builtIn bool) int {
	if builtIn {
		return 0
	}
	if configuredMap, ok := configured.(map[string]any); ok {
		return len(configuredMap)
	}
	return 0
}

func buildComposite(targetType reflect.Type, leaves map[string]reflect.Value, materialized bool) (reflect.Value, error) {
	isPointer := targetType.Kind() == reflect.Pointer
	baseType := targetType
	if isPointer {
		baseType = targetType.Elem()
	}

	var value reflect.Value
	switch baseType.Kind() {
	case reflect.Map:
		if !materialized && len(leaves) == 0 {
			value = reflect.Zero(baseType)
		} else {
			value = reflect.MakeMapWithSize(baseType, len(leaves))
			for name, leaf := range leaves {
				key := reflect.ValueOf(name)
				if key.Type() != baseType.Key() {
					key = key.Convert(baseType.Key())
				}
				if !leaf.Type().AssignableTo(baseType.Elem()) {
					return reflect.Value{}, fmt.Errorf("leaf %q has type %s, want %s", name, leaf.Type(), baseType.Elem())
				}
				value.SetMapIndex(key, leaf)
			}
		}
	case reflect.Struct:
		value = reflect.New(baseType).Elem()
		for name, leaf := range leaves {
			field, ok := taggedFieldByKey(value, name)
			if !ok || !field.CanSet() {
				return reflect.Value{}, fmt.Errorf("table has no settable field %q", name)
			}
			if !leaf.Type().AssignableTo(field.Type()) {
				return reflect.Value{}, fmt.Errorf("field %q has type %s, want %s", name, leaf.Type(), field.Type())
			}
			field.Set(leaf)
		}
	default:
		return reflect.Value{}, fmt.Errorf("merge policy requires map or table, got %s", targetType)
	}

	if isPointer {
		if !materialized && len(leaves) == 0 {
			return reflect.Zero(targetType), nil
		}
		pointer := reflect.New(baseType)
		pointer.Elem().Set(value)
		return pointer, nil
	}
	return value, nil
}

func clonedInterface(value reflect.Value) any {
	if !value.IsValid() {
		return nil
	}
	return cloneExportedValue(value).Interface()
}

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

	raw := mapSide.(map[string]any)
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

// isJSONZero reports whether v is the JSON zero value for its Go type as
// decoded from a raw JSON document into an any. Those zero values are: nil,
// the empty string, the number 0, bool false, and empty slice/map. These are
// precisely the values that json.Marshal would omit for an omitempty struct field.
func isJSONZero(v any) bool {
	if v == nil {
		return true
	}
	switch c := v.(type) {
	case string:
		return c == ""
	case bool:
		return !c
	case json.Number:
		f, err := c.Float64()
		return err == nil && f == 0
	case float64:
		return c == 0
	case int64:
		return c == 0
	}
	// Slice or map: zero if empty.
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Map:
		return rv.Len() == 0
	}
	return false
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

func leafKeyPath(key, leaf string) string {
	if isBareConfigPathPart(leaf) {
		return key + "." + leaf
	}
	return key + "[" + strconv.Quote(leaf) + "]"
}

func isBareConfigPathPart(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func pluralize(word string, count int) string {
	if count == 1 {
		return word
	}
	return word + "s"
}

// ResolvedValuePath returns either a top-level resolution or a dotted leaf
// projection such as program_overrides.codex or root_agent.program. Only values with
// a real per-leaf origin can be projected: borrowing a replace-table's winner
// would fabricate a source for omitted fields materialized as zero values. A
// leaf is projected from the already-resolved value and origins; it never runs
// a second precedence algorithm.
func (r *ResolvedConfig) ResolvedValuePath(keyPath string) (ResolvedValue, bool) {
	keyPath = canonicalConfigKey(keyPath)
	if value, ok := r.ResolvedValue(keyPath); ok {
		return value, true
	}
	key, leaf, dotted := strings.Cut(keyPath, ".")
	if !dotted || key == "" || leaf == "" || strings.Contains(leaf, ".") {
		return ResolvedValue{}, false
	}
	parent, ok := r.ResolvedValue(key)
	if !ok {
		return ResolvedValue{}, false
	}
	effective, ok := configLeafValue(parent.Value, leaf)
	if !ok {
		return ResolvedValue{}, false
	}

	projected := parent
	projected.Key = keyPath
	projected.Value = effective
	projected.Default = ""
	projected.Origins = nil
	winner, present := parent.Origins[leaf]
	if !present {
		return ResolvedValue{}, false
	}
	winner.KeyPath = keyPath
	projected.Winner = &winner
	precedenceRank := make(map[string]int, len(parent.Precedence))
	for i, layer := range parent.Precedence {
		precedenceRank[layer] = i
	}
	winnerRank := precedenceRank[winner.Layer]

	projected.Candidates = make([]CandidateTrace, len(parent.Candidates))
	for i, candidate := range parent.Candidates {
		parentReason := candidate.Reason
		candidate.KeyPath = keyPath
		leafValue, present := configLeafValue(candidate.Value, leaf)
		candidate.Present = candidate.Present && present
		if candidate.Present {
			candidate.Value = leafValue
		} else {
			candidate.Value = nil
		}
		switch {
		case !candidate.Allowed:
			candidate.Result = "disallowed"
			candidate.Reason = "manifest policy does not allow this key at this source"
		case !candidate.Present:
			candidate.Result = "absent"
			candidate.Reason = "leaf is not present at this source"
		case candidate.Layer == winner.Layer:
			candidate.Result = "winner"
			candidate.Reason = "supplies the effective leaf"
		case precedenceRank[candidate.Layer] > winnerRank:
			candidate.Result = "ignored"
			candidate.Reason = "configured leaf did not survive load-time normalization; lower-precedence " + winner.Layer + " supplies the effective leaf"
		default:
			candidate.Result = "shadowed"
			candidate.Reason = "leaf is overridden by higher-precedence " + winner.Layer
		}
		if note := resolutionNormalizationNote(parentReason); note != "" && candidate.Present {
			candidate.Reason += "; " + note
		}
		projected.Candidates[i] = candidate
	}
	return projected, true
}

func resolutionNormalizationNote(reason string) string {
	for _, marker := range []string{"load-time normalization changed", "effective key bindings normalize"} {
		if index := strings.Index(reason, marker); index >= 0 {
			return reason[index:]
		}
	}
	return ""
}

func configLeafValue(value any, leaf string) (any, bool) {
	if value == nil {
		return nil, false
	}
	reflected := reflect.ValueOf(value)
	for reflected.Kind() == reflect.Pointer || reflected.Kind() == reflect.Interface {
		if reflected.IsNil() {
			return nil, false
		}
		reflected = reflected.Elem()
	}
	switch reflected.Kind() {
	case reflect.Map:
		key := reflect.ValueOf(leaf)
		if !key.Type().AssignableTo(reflected.Type().Key()) {
			if !key.Type().ConvertibleTo(reflected.Type().Key()) {
				return nil, false
			}
			key = key.Convert(reflected.Type().Key())
		}
		entry := reflected.MapIndex(key)
		if !entry.IsValid() {
			return nil, false
		}
		return clonedInterface(entry), true
	case reflect.Struct:
		field, ok := taggedFieldByKey(reflected, leaf)
		if !ok {
			return nil, false
		}
		return clonedInterface(field), true
	default:
		return nil, false
	}
}
