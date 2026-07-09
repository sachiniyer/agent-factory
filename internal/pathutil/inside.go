package pathutil

import (
	"path/filepath"
	"strings"
)

// IsStrictlyInside reports whether absBase is a strict descendant of absDir
// (absBase != absDir and absBase is not outside absDir). Both arguments must
// be absolute, cleaned paths.
func IsStrictlyInside(absBase, absDir string) bool {
	rel, err := filepath.Rel(absDir, absBase)
	if err != nil {
		return false
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}
