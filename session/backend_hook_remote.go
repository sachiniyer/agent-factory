package session

import (
	"encoding/json"
	"regexp"
	"strings"
)

var slugRegexp = regexp.MustCompile(`[^a-z0-9-]`)

// Slugify converts a title to a slug-safe string for the remote hook scripts.
// The slug is the stable identifier launch_cmd and delete_cmd receive via
// --name (docs/remote-hooks.md): launch_cmd tags the provisioned sandbox with
// it and delete_cmd reaps by it, so two sessions whose titles slugify the same
// must not coexist (FindSlugCollision guards that at create time).
func Slugify(title string) string {
	s := strings.ToLower(title)
	s = strings.ReplaceAll(s, " ", "-")
	s = slugRegexp.ReplaceAllString(s, "")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "session"
	}
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
// endpoint JSON to stdout, and CombinedOutput mixes the two. Returns empty
// string if no valid JSON value is found.
func extractJSON(output string) string {
	for i := 0; i < len(output); i++ {
		if output[i] != '{' && output[i] != '[' {
			continue
		}

		var depth int
		inString := false
		escape := false

		for j := i; j < len(output); j++ {
			c := output[j]

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

			if !inString {
				if c == '{' || c == '[' {
					depth++
				}
				if c == '}' || c == ']' {
					depth--
					if depth == 0 {
						candidate := output[i : j+1]
						var test interface{}
						if json.Unmarshal([]byte(candidate), &test) == nil {
							return candidate
						}
						break
					}
				}
			}
		}
	}
	return ""
}
