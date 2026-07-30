package session

import (
	"encoding/json"
	"regexp"
	"strings"
)

var slugRegexp = regexp.MustCompile(`[^a-z0-9-]`)

// maxSlugLen bounds the slug so it stays a legal filesystem component wherever a
// backend uses it as one: the ssh backend's `mktemp -d "$HOME/<root>/<slug>.XXXXXX"`
// and the hook backend's `mkdir -p "$STATE/<slug>"`. Linux NAME_MAX is 255 bytes
// per component; 200 leaves room for the ssh ".XXXXXX" suffix and any prefix a hook
// script adds. Without it a long title (creatable via the CLI/API, which don't share
// the TUI's 32-char cap) failed deep in provisioning with a cryptic ENAMETOOLONG
// (#2528). The slug is ASCII ([a-z0-9-]), so a byte truncation is rune-safe.
const maxSlugLen = 200

// RemoteHookTitleHasSpecificSlug reports whether the exact sanitization and
// truncation Slugify applies retains a title-derived name. A raw title may have
// ASCII content only after the bounded slug prefix (for example, 200 hyphens
// followed by "a"), so scanning the unbounded input is not equivalent.
//
// Do not infer this from Slugify(title) == "session": valid titles such as
// "SESSION!" deliberately derive that same slug.
func RemoteHookTitleHasSpecificSlug(title string) bool {
	return slugWithoutFallback(title) != ""
}

// Slugify converts a title to a slug-safe string for the remote hook scripts.
// The slug is the stable identifier launch_cmd and delete_cmd receive via
// --name (docs/remote-hooks.md): launch_cmd tags the provisioned sandbox with
// it and delete_cmd reaps by it, so two sessions whose titles slugify the same
// must not coexist (FindSlugCollision guards that at create time — including two
// long titles that truncate to the same slug).
func Slugify(title string) string {
	s := slugWithoutFallback(title)
	if s == "" {
		s = "session"
	}
	return s
}

func slugWithoutFallback(title string) string {
	s := strings.ToLower(title)
	s = strings.ReplaceAll(s, " ", "-")
	s = slugRegexp.ReplaceAllString(s, "")
	if len(s) > maxSlugLen {
		s = s[:maxSlugLen]
	}
	s = strings.Trim(s, "-")
	return s
}

// FindSlugCollision returns the title of the first existing remote instance
// whose hook slug collides with candidate, or "" if none do. Two titles that
// slugify to the same value would key delete_cmd on the same remote sandbox, so
// the create path rejects the collision before provisioning.
func FindSlugCollision(candidate string, existing []*Instance) string {
	if candidate == "" {
		return ""
	}
	want := Slugify(candidate)
	for _, inst := range existing {
		if inst == nil || inst.Title == candidate {
			continue
		}
		if Slugify(inst.Title) == want {
			return inst.Title
		}
	}
	return ""
}

// extractJSON finds the first complete top-level JSON value (object or array)
// in output, ignoring text outside JSON delimiters. It handles pretty-printed /
// multi-line JSON and stderr interleaving around (but not inside) the JSON
// payload — launch_cmd may write provisioning progress to stderr and echo its
// endpoint JSON to stdout, and the shared capture file contains both. Returns
// empty string if no valid JSON value is found.
func extractJSON(output string) string {
	value, _ := extractJSONAt(output, 0)
	return value
}

// extractJSONAt returns the first complete JSON value at or after start and the
// byte offset immediately after it. The cursor lets shape-aware callers inspect
// every value in a combined stdout/stderr stream without rescanning prior output.
type jsonCandidateRange struct {
	start  int
	end    int
	parent int
}

func extractJSONAt(output string, start int) (string, int) {
	var candidates []jsonCandidateRange
	var stack []int
	inString := false
	escape := false
	for cursor := start; cursor < len(output); cursor++ {
		c := output[cursor]
		if len(stack) == 0 {
			if c != '{' && c != '[' {
				continue
			}
			candidates = append(candidates[:0], jsonCandidateRange{start: cursor, parent: -1})
			stack = append(stack[:0], 0)
			inString = false
			escape = false
			continue
		}

		if escape {
			escape = false
			continue
		}
		if c == '\\' && inString {
			escape = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}

		if c == '{' || c == '[' {
			candidates = append(candidates, jsonCandidateRange{
				start:  cursor,
				parent: stack[len(stack)-1],
			})
			stack = append(stack, len(candidates)-1)
			continue
		}
		if c != '}' && c != ']' {
			continue
		}

		candidateIndex := stack[len(stack)-1]
		candidates[candidateIndex].end = cursor + 1
		stack = stack[:len(stack)-1]
		if len(stack) != 0 {
			continue
		}

		candidate := candidates[0]
		if json.Valid([]byte(output[candidate.start:candidate.end])) {
			return output[candidate.start:candidate.end], candidate.end
		}
		candidates = candidates[:0]
	}
	if candidate, ok := firstRecoverableJSONCandidate(output, candidates); ok {
		return output[candidate.start:candidate.end], candidate.end
	}
	return "", len(output)
}

// If an outer delimiter never closed, a later complete JSON value would
// otherwise be hidden inside it. Only validate completed direct children of an
// unresolved candidate. Those ranges are disjoint, which preserves recovery
// without repeatedly validating every nested suffix of malformed output.
func firstRecoverableJSONCandidate(output string, candidates []jsonCandidateRange) (jsonCandidateRange, bool) {
	for _, candidate := range candidates {
		if candidate.end <= candidate.start {
			continue
		}
		if candidate.parent >= 0 && candidates[candidate.parent].end != 0 {
			continue
		}
		if json.Valid([]byte(output[candidate.start:candidate.end])) {
			return candidate, true
		}
	}
	return jsonCandidateRange{}, false
}
