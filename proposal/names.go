// Package proposal stores proposals: per-project folders of markdown docs that
// a user and one or more agent sessions edit together. The store is a private
// git repository under the AF home, so every edit has an author and every
// session has a cursor into the history.
package proposal

import (
	"fmt"
	"regexp"
	"strings"
)

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// ValidateSlug reports whether slug is a valid proposal name.
func ValidateSlug(slug string) error {
	if !namePattern.MatchString(slug) {
		return fmt.Errorf("invalid proposal slug %q: use 1-63 lowercase letters, digits and dashes, starting with a letter or digit", slug)
	}
	return nil
}

// NormalizeDocName validates a doc name and returns it without a ".md"
// extension. Callers may pass "api" or "api.md".
func NormalizeDocName(name string) (string, error) {
	base := strings.TrimSuffix(name, ".md")
	if !namePattern.MatchString(base) {
		return "", fmt.Errorf("invalid doc name %q: use 1-63 lowercase letters, digits and dashes, starting with a letter or digit", name)
	}
	return base, nil
}

func validateSessionID(id string) error {
	if !sessionIDPattern.MatchString(id) {
		return fmt.Errorf("invalid session id %q", id)
	}
	return nil
}

func docFile(name string) string { return name + ".md" }
