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
