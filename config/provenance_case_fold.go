package config

import "reflect"

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
