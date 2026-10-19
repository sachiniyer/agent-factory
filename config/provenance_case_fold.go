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
func collapseStructLeaves(targetType reflect.Type, leaves map[string]reflect.Value) map[string]reflect.Value {
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
	collapsed := make(map[string]reflect.Value, len(byCanonical))
	for _, canonical := range order {
		entries := byCanonical[canonical]
		if len(entries) == 1 {
			collapsed[entries[0].physical] = entries[0].value
			continue
		}
		chosen := entries[0]
		for _, e := range entries {
			if e.physical == canonical {
				chosen = e
				break
			}
		}
		if chosen.physical != canonical {
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
