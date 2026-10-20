package config

import (
	"reflect"
	"strings"
)

// candidateLeafProjectionKey returns the map key under which a struct-backed
// candidate stores a case-variant of leaf, so a non-winner candidate that
// spells the leaf differently from the winner (e.g. a global [root_agent]
// ENABLED = false when the winner is a personal [root_agent] Enabled = true)
// projects its own value rather than being marked absent. physicalLeaf is the
// winner's physical spelling; for the winner and any same-source candidate
// whose map still carries that spelling, it is the decoder-winning spelling
// the collapse retained, so it is preferred when present. For a struct-typed
// candidate Value (built-in sources) configLeafValue's struct branch folds
// case via taggedFieldByKey regardless of the key used, so physicalLeaf is
// returned unchanged. Map-backed composites never reach the struct-backed
// origin fallback in ResolvedValuePath, so this helper is only called for
// struct-backed parents. Within one struct, tags are unique by case, so the
// case-insensitive scan is unambiguous (#5397 review).
func candidateLeafProjectionKey(value any, leaf, physicalLeaf string) string {
	if value == nil {
		return physicalLeaf
	}
	reflected := reflect.ValueOf(value)
	for reflected.Kind() == reflect.Pointer || reflected.Kind() == reflect.Interface {
		if reflected.IsNil() {
			return physicalLeaf
		}
		reflected = reflected.Elem()
	}
	if reflected.Kind() != reflect.Map {
		// Struct-typed (built-in) or scalar: configLeafValue's struct branch
		// folds case via taggedFieldByKey, so the winner's physical spelling
		// projects the same field as the canonical leaf. Use it to preserve the
		// physical-spelling KeyPath behaviour on the exact-match path.
		return physicalLeaf
	}
	// The winner's physical spelling is the decoder-winning spelling the
	// collapse retained, so prefer it when the candidate's map still carries
	// it (the winner, and any same-source candidate whose raw map has not been
	// collapsed to a single spelling).
	if key := reflect.ValueOf(physicalLeaf); key.Type().AssignableTo(reflected.Type().Key()) {
		if entry := reflected.MapIndex(key); entry.IsValid() {
			return physicalLeaf
		}
	}
	// A non-winner candidate may spell the leaf differently from the winner;
	// scan its own keys case-insensitively so it projects its own value and is
	// reported as shadowed instead of absent. Within one struct, tags are
	// unique by case, so the scan is unambiguous.
	for _, key := range reflected.MapKeys() {
		if s, ok := key.Interface().(string); ok && strings.EqualFold(s, leaf) {
			return s
		}
	}
	// No case-variant of leaf is present in this candidate's map; the leaf is
	// genuinely absent here. Fall back to the canonical leaf so configLeafValue's
	// exact map lookup returns false and the candidate is correctly marked
	// absent (a candidate that did not configure this leaf should not be
	// reported as shadowed by the winner).
	return leaf
}

// collapsedSpellingForLeaf looks up the decoder-winning physical spelling a
// candidate retained for leaf in collapseStructLeaves' output. The map is keyed
// by the canonical struct-leaf name (canonicalStructLeafName), but a caller may
// pass a case-variant leaf (e.g. root_agent.Enabled, whose canonical spelling is
// enabled), so the lookup folds case; within one struct, tags are unique by
// case, so the match is unambiguous. Returns the physical spelling and whether a
// spelling was retained for this leaf. The empty map (map-backed composites and
// non-struct values, which never set collapsedLeafSpellings) returns (""; false)
// so the caller falls back to candidateLeafProjectionKey's raw-map scan (#5397
// review).
func collapsedSpellingForLeaf(spellings map[string]string, leaf string) (string, bool) {
	if spelling, ok := spellings[leaf]; ok {
		return spelling, true
	}
	for canonical, physical := range spellings {
		if strings.EqualFold(canonical, leaf) {
			return physical, true
		}
	}
	return "", false
}

// collapseStructLeaves folds case-variant spellings of the same struct field
// within one candidate's leaf set to a single entry. encoding/json and
// go-toml/v2 both decode case-variant keys onto the same struct field
// (last-write-wins for the value), so a source that contains both "image" and
// "Image" leaves them as two physical spellings in candidate.leaves that both
// point at the same reflect.Value. Without collapsing, the merge loop in
// resolveComposite processes both under the same canonical mergeKey, deleting
// and re-adding the origin in map-iteration order so the reported winning
// KeyPath is non-deterministic, and the surviving-count accounting reports the
// candidate as partially-shadowed even though both spellings came from the same
// source (#5397 review). Collapse to one physical spelling per canonical struct
// field, preferring the canonical tag name if the source used it (so the
// origin's KeyPath is deterministic) and otherwise the lexicographically
// smallest spelling. The value is the same reflect.Value for every spelling of
// a field (the decoder already resolved them), so the retained value is
// unambiguous. Returns leaves unchanged when no two spellings fold to the same
// canonical field (the common single-spelling case).
//
// When the source supplied conflicting values for the case-variant spellings
// (e.g. {"image":"loser","Image":"winner"}), the decoder is last-write-wins, so
// the typed reflect.Value holds the final value ("winner") but the raw
// configured map still carries each spelling's original value. The retained
// physical spelling must be the one whose raw value matches the decoded value,
// otherwise a ResolvedValuePath projection that looks up the candidate's raw
// map by the retained spelling projects the losing sibling ("loser") and
// reports an internally inconsistent trace. configured is the candidate's raw
// metadata (a map[string]any for non-built-in sources); when it is unavailable
// or the decoder winner is ambiguous (multiple spellings share the decoded
// value, or none do), the spelling selection falls back to the canonical-then
// lexicographic preference above so the collapse stays deterministic.
func collapseStructLeaves(targetType reflect.Type, leaves map[string]reflect.Value, configured any) map[string]reflect.Value {
	if len(leaves) <= 1 {
		return leaves
	}
	type leafEntry struct {
		physical string
		value    reflect.Value
	}
	byCanonical := make(map[string][]leafEntry, len(leaves))
	order := make([]string, 0, len(leaves))
	for physical, value := range leaves {
		canonical := canonicalStructLeafName(targetType, physical)
		if _, ok := byCanonical[canonical]; !ok {
			order = append(order, canonical)
		}
		byCanonical[canonical] = append(byCanonical[canonical], leafEntry{physical, value})
	}
	if len(byCanonical) == len(leaves) {
		return leaves
	}
	configuredMap, _ := configured.(map[string]any)
	collapsed := make(map[string]reflect.Value, len(byCanonical))
	for _, canonical := range order {
		entries := byCanonical[canonical]
		if len(entries) == 1 {
			collapsed[entries[0].physical] = entries[0].value
			continue
		}
		chosen := entries[0]
		// When the source supplied conflicting values for the case-variant
		// spellings, the decoder (last-write-wins) left the typed field set to
		// one value but the raw map still carries each spelling's original
		// value. Retain the spelling whose raw value matches the decoded value
		// so a projection that reads the candidate's raw map by this spelling
		// reports the value the decoder kept. Only act when exactly one
		// spelling matches; when several (or none) match the decoded value the
		// decoder winner is ambiguous from the map alone, so fall through to
		// the deterministic canonical preference below.
		decoderWinnerKnown := false
		if configuredMap != nil {
			decoded := clonedInterface(entries[0].value)
			var matched leafEntry
			matchedCount := 0
			for _, e := range entries {
				if raw, ok := configuredMap[e.physical]; ok && jsonEquivalent(raw, decoded) {
					matched = e
					matchedCount++
				}
			}
			if matchedCount == 1 {
				chosen = matched
				decoderWinnerKnown = true
			}
		}
		// When the decoder winner is ambiguous (or no configured map is
		// available, e.g. a built-in source), prefer the canonical tag
		// spelling the source used (deterministic KeyPath) and otherwise the
		// lexicographically smallest spelling.
		if !decoderWinnerKnown {
			for _, e := range entries {
				if e.physical == canonical {
					chosen = e
					break
				}
			}
		}
		if !decoderWinnerKnown && chosen.physical != canonical {
			for _, e := range entries {
				if e.physical < chosen.physical {
					chosen = e
				}
			}
		}
		collapsed[chosen.physical] = chosen.value
	}
	return collapsed
}

// resolutionNormalizationNote extracts the load-time normalization note a
// candidate's Reason carries (if any) so the ResolvedValuePath projection can
// append it to a shadowed candidate's projected reason. The markers are the
// same strings resolveReplace and resolveComposite set when load-time
// normalization changed the configured value before resolution.
func resolutionNormalizationNote(reason string) string {
	for _, marker := range []string{"load-time normalization changed", "effective key bindings normalize"} {
		if index := strings.Index(reason, marker); index >= 0 {
			return reason[index:]
		}
	}
	return ""
}

// configLeafValue projects a single leaf from a candidate's raw Value (the
// metadata map for non-built-in sources, the typed struct for built-in sources)
// by exact key. For a struct-typed Value the lookup folds case via
// taggedFieldByKey, so a case-variant leaf the operator wrote (e.g. [docker]
// Image) projects the same field as the canonical tag. For a map-typed Value
// the lookup is exact (map keys are not case-folded; see
// TestResolveConfigCompositeLeafMapBranchStaysExact). Returns false for a nil
// value, a missing key, or a type mismatch so the projection loop can mark the
// candidate absent (#5397 review).
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
