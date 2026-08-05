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
// multi-line JSON and prose around (but not inside) the JSON payload. Returns
// empty string if no valid JSON value is found.
//
// This is a diagnostic-grade scanner: it reports what it can recognize in
// arbitrary text and stays silent otherwise. Endpoint selection does NOT rely on
// its recovery of nested values — see selectHookEndpoint, which takes only
// complete top-level values off stdout.
func extractJSON(output string) string {
	value, _ := extractJSONAt(output, 0)
	return value
}

// jsonRange is one balanced delimiter pair found by the scanner, linked to its
// children so a malformed wrapper can be searched without rescanning overlapping
// suffixes. end is -1 while the pair is still open.
type jsonRange struct {
	start       int
	end         int
	firstChild  int
	lastChild   int
	nextSibling int
}

// maxJSONResyncQuotes bounds how many quote boundaries are retried when a first
// pass finds nothing. Prose with an unmatched quote phase-shifts every delimiter
// after it, and a raw newline only resynchronizes at end of line — this covers
// the same-line case with a fixed number of extra passes instead of a restart at
// every opener, which is what made the pre-#2637 scanner quadratic.
const maxJSONResyncQuotes = 8

// extractJSONAt returns the first complete JSON value at or after start and the
// byte offset immediately after it. The cursor lets callers walk every value in
// a stream without rescanning prior output.
func extractJSONAt(output string, start int) (string, int) {
	return findJSONAt(output, start, true)
}

func findJSONAt(output string, start int, recover bool) (string, int) {
	if value, next := scanJSONAt(output, start, recover); value != "" {
		return value, next
	}
	quotes := 0
	for cursor := start; cursor < len(output) && quotes < maxJSONResyncQuotes; cursor++ {
		if output[cursor] != '"' {
			continue
		}
		quotes++
		if value, next := scanJSONAt(output, cursor+1, recover); value != "" {
			return value, next
		}
	}
	return "", len(output)
}

func scanJSONAt(output string, start int, recover bool) (string, int) {
	var ranges []jsonRange
	var stack []int
	inString := false
	escape := false

	openRange := func(cursor int) {
		index := len(ranges)
		ranges = append(ranges, jsonRange{start: cursor, end: -1, firstChild: -1, lastChild: -1, nextSibling: -1})
		if len(stack) > 0 {
			parent := stack[len(stack)-1]
			if ranges[parent].firstChild < 0 {
				ranges[parent].firstChild = index
			} else {
				ranges[ranges[parent].lastChild].nextSibling = index
			}
			ranges[parent].lastChild = index
		}
		stack = append(stack, index)
	}

	for cursor := start; cursor < len(output); cursor++ {
		c := output[cursor]
		if inString {
			switch {
			case escape:
				escape = false
			case c == '\\':
				escape = true
			case c == '"':
				inString = false
			case c == '\n' || c == '\r':
				// A raw newline cannot appear inside a JSON string, so the quote that
				// opened this one was prose, not a string. Resynchronize here or an
				// unterminated diagnostic swallows every delimiter after it.
				inString = false
				escape = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
			escape = false
		case '{', '[':
			openRange(cursor)
		case '}', ']':
			if len(stack) == 0 {
				continue
			}
			top := stack[len(stack)-1]
			ranges[top].end = cursor + 1
			stack = stack[:len(stack)-1]
			if len(stack) > 0 {
				continue
			}
			if value, end, ok := firstValidJSONRange(output, ranges, top, recover); ok {
				// Report the recovered value's OWN end, not the wrapper's. Callers
				// derive its start as end-len(value) to rewrite that exact span, and
				// a wrapper end there points the rewrite at the wrong bytes (Codex P2
				// on #2718).
				return value, end
			}
			// Nothing in this tree parsed; it is prose. Drop it and keep scanning.
			ranges = ranges[:0]
		}
	}
	// Wrappers left open at EOF: their completed children may still be valid.
	if len(ranges) > 0 {
		if value, end, ok := firstValidJSONRange(output, ranges, 0, recover); ok {
			return value, end
		}
	}
	return "", len(output)
}

// firstValidJSONRange returns the first range in this subtree that parses as a
// complete JSON value, preferring the outermost. Each range is inspected at most
// once, and descent is bounded to children that begin after the parent's first
// syntax error — everything before it was already covered by the parent's failed
// parse, so total inspected bytes stay linear in the output size.
func firstValidJSONRange(output string, ranges []jsonRange, index int, recover bool) (string, int, bool) {
	candidate := ranges[index]
	errorAt := candidate.start
	if candidate.end > candidate.start {
		valid, position, recoverable := inspectJSONRange(output, candidate)
		if valid {
			return output[candidate.start:candidate.end], candidate.end, true
		}
		if !recoverable {
			return "", 0, false
		}
		errorAt = position
	}
	if !recover {
		return "", 0, false
	}
	for child := candidate.firstChild; child >= 0; child = ranges[child].nextSibling {
		if ranges[child].start < errorAt {
			continue
		}
		if value, end, ok := firstValidJSONRange(output, ranges, child, recover); ok {
			return value, end, true
		}
	}
	return "", 0, false
}

// inspectJSONRange parses directly from the output string so an early syntax
// error does not copy the range's entire remaining suffix. The error position
// tells recovery which descendants the failed parse had not already reached.
func inspectJSONRange(output string, candidate jsonRange) (bool, int, bool) {
	decoder := json.NewDecoder(strings.NewReader(output[candidate.start:candidate.end]))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err == nil {
		return true, 0, false
	} else if syntaxErr, ok := err.(*json.SyntaxError); ok {
		position := candidate.start + int(syntaxErr.Offset) - 1
		if position < candidate.start {
			position = candidate.start
		}
		if position > candidate.end {
			position = candidate.end
		}
		return false, position, true
	}
	return false, 0, false
}
