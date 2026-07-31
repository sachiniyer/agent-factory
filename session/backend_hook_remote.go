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
	start            int
	end              int
	firstChild       int
	lastChild        int
	nextSibling      int
	keyStart         int
	keyEnd           int
	propertyValue    bool
	embedded         bool
	endpointEmbedded bool
}

func extractJSONAt(output string, start int) (string, int) {
	value, next, _ := extractJSONCandidateAt(output, start, true)
	return value, next
}

// extractEndpointJSONAt also reports whether a recovered value was independent
// of a surrounding array. Generic malformed-prose parsing may recover such a
// value, but launch must not promote an endpoint-shaped log array element into
// the provisioner's top-level endpoint record.
func extractEndpointJSONAt(output string, start int) (string, int, bool) {
	return extractJSONCandidateAt(output, start, true)
}

func extractJSONCandidateAt(output string, start int, allowEOFResync bool) (string, int, bool) {
	var candidates []jsonCandidateRange
	var stack []int
	inString := false
	escape := false
	jsonStringStart := -1
	stringCandidateStart := -1
	lineRescanned := false
	for cursor := start; cursor < len(output); cursor++ {
		c := output[cursor]
		if len(stack) == 0 {
			if c != '{' && c != '[' {
				continue
			}
			candidates = append(candidates[:0], jsonCandidateRange{
				start:       cursor,
				firstChild:  -1,
				lastChild:   -1,
				nextSibling: -1,
				keyStart:    -1,
				keyEnd:      -1,
			})
			stack = append(stack[:0], 0)
			inString = false
			escape = false
			jsonStringStart = -1
			continue
		}

		// Retain the most recent opener in an impossible JSON string. Retrying
		// only that suffix recovers a later endpoint past any number of malformed
		// wrapper openers without making suffix retries grow with input size.
		if inString && (c == '{' || c == '[') {
			stringCandidateStart = cursor
		}
		// A raw newline cannot occur inside a JSON string. Treat it as a hard
		// resynchronization boundary so an unterminated stderr diagnostic cannot
		// hide a complete endpoint record written on the next line to stdout.
		if inString && (c == '\n' || c == '\r') {
			if stringCandidateStart >= 0 && !lineRescanned {
				cursor = stringCandidateStart - 1
				candidates = candidates[:0]
				stack = stack[:0]
				inString = false
				escape = false
				jsonStringStart = -1
				stringCandidateStart = -1
				lineRescanned = true
				continue
			}
			candidates = candidates[:0]
			stack = stack[:0]
			inString = false
			escape = false
			jsonStringStart = -1
			stringCandidateStart = -1
			lineRescanned = false
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
			if inString {
				inString = false
				parent := stack[len(stack)-1]
				candidates[parent].keyStart = jsonStringStart
				candidates[parent].keyEnd = cursor + 1
				candidates[parent].propertyValue = false
				jsonStringStart = -1
			} else {
				inString = true
				jsonStringStart = cursor
			}
			continue
		}
		if inString {
			continue
		}
		if c == '\n' || c == '\r' {
			stringCandidateStart = -1
			lineRescanned = false
			continue
		}

		parent := stack[len(stack)-1]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			continue
		}
		if c == ':' {
			keyStart := candidates[parent].keyStart
			keyEnd := candidates[parent].keyEnd
			candidates[parent].propertyValue = output[candidates[parent].start] == '{' &&
				keyStart >= 0 && keyEnd > keyStart && json.Valid([]byte(output[keyStart:keyEnd]))
			candidates[parent].keyStart = -1
			candidates[parent].keyEnd = -1
			continue
		}
		if c == '{' || c == '[' {
			propertyValue := candidates[parent].propertyValue
			candidates[parent].keyStart = -1
			candidates[parent].keyEnd = -1
			candidates[parent].propertyValue = false
			parent := stack[len(stack)-1]
			index := len(candidates)
			candidates = append(candidates, jsonCandidateRange{
				start:            cursor,
				firstChild:       -1,
				lastChild:        -1,
				nextSibling:      -1,
				keyStart:         -1,
				keyEnd:           -1,
				embedded:         candidates[parent].embedded || propertyValue,
				endpointEmbedded: candidates[parent].endpointEmbedded || propertyValue || output[candidates[parent].start] == '[',
			})
			if candidates[parent].firstChild < 0 {
				candidates[parent].firstChild = index
			} else {
				candidates[candidates[parent].lastChild].nextSibling = index
			}
			candidates[parent].lastChild = index
			stack = append(stack, index)
			continue
		}
		candidates[parent].keyStart = -1
		candidates[parent].keyEnd = -1
		candidates[parent].propertyValue = false
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
		valid, errorAt, recoverable := inspectJSONCandidate(output, candidate)
		if valid {
			return output[candidate.start:candidate.end], candidate.end, !candidate.endpointEmbedded
		}
		if recoverable {
			candidate, recoverable = firstJSONCandidateAfterError(output, candidates, 0, errorAt)
		}
		if recoverable {
			return output[candidate.start:candidate.end], candidate.end, !candidate.endpointEmbedded
		}
		candidates = candidates[:0]
		stringCandidateStart = -1
	}
	if allowEOFResync && stringCandidateStart >= 0 && !lineRescanned {
		return extractJSONCandidateAt(output, stringCandidateStart, false)
	}
	if len(candidates) > 0 {
		unfinished := candidates[0]
		unfinished.end = len(output)
		if _, errorAt, ok := inspectJSONCandidate(output, unfinished); ok {
			if candidate, ok := firstJSONCandidateAfterError(output, candidates, 0, errorAt); ok {
				return output[candidate.start:candidate.end], candidate.end, !candidate.endpointEmbedded
			}
		}
	}
	return "", len(output), false
}

// firstJSONCandidateAfterError descends only through children that begin after
// their parent's first syntax error. Children before that point are structured
// values embedded in the malformed parent, not independent launch records. At
// each level direct children are disjoint; recursion happens only after an early
// syntax error, avoiding overlapping full-document validation.
func firstJSONCandidateAfterError(
	output string,
	candidates []jsonCandidateRange,
	parent int,
	errorAt int,
) (jsonCandidateRange, bool) {
	for index := candidates[parent].firstChild; index >= 0; index = candidates[index].nextSibling {
		candidate := candidates[index]
		if candidate.embedded || candidate.end <= candidate.start || candidate.start < errorAt {
			continue
		}
		valid, childErrorAt, recoverable := inspectJSONCandidate(output, candidate)
		if valid {
			return candidate, true
		}
		if !recoverable {
			continue
		}
		if nested, ok := firstJSONCandidateAfterError(output, candidates, index, childErrorAt); ok {
			return nested, true
		}
	}
	return jsonCandidateRange{}, false
}

// inspectJSONCandidate parses directly from the output string so an early
// syntax error does not copy the candidate's entire remaining suffix. The error
// position tells recovery which descendants occurred only after the parent had
// already ceased to be valid JSON.
func inspectJSONCandidate(output string, candidate jsonCandidateRange) (bool, int, bool) {
	decoder := json.NewDecoder(strings.NewReader(output[candidate.start:candidate.end]))
	var raw json.RawMessage
	err := decoder.Decode(&raw)
	if err == nil {
		return true, 0, false
	}
	syntaxErr, ok := err.(*json.SyntaxError)
	if !ok {
		return false, 0, false
	}
	position := candidate.start + int(syntaxErr.Offset) - 1
	if position < candidate.start {
		position = candidate.start
	}
	if position > candidate.end {
		position = candidate.end
	}
	return false, position, true
}
