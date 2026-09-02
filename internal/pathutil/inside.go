package pathutil

import (
	"path/filepath"
	"strings"
)

// Path containment, decided by filepath.Rel and never by a string prefix:
// "/x/wt-backup" starts with "/x/wt" and is not inside it. Both arguments must
// be absolute, cleaned paths; a pair that cannot be related at all (different
// Windows volumes) is not contained.
//
// The two exported forms differ in exactly one bit — whether the directory
// counts as inside itself — so callers pick the one their guard means instead
// of re-deriving the rel-prefix dance.
//
// Symlink resolution is the CALLER's job (ResolveForCompare): Rel relates two
// spellings, not two inodes, so /tmp/x and /private/tmp/x are unrelated here.

// IsAtOrInside reports whether absPath is absDir itself or a path beneath it.
func IsAtOrInside(absPath, absDir string) bool {
	contained, _ := containment(absPath, absDir)
	return contained
}

// IsStrictlyInside reports whether absPath is a strict descendant of absDir
// (contained, and not absDir itself).
func IsStrictlyInside(absPath, absDir string) bool {
	contained, same := containment(absPath, absDir)
	return contained && !same
}

// containment reports whether absPath lies at or under absDir, and whether it
// is absDir itself. A pair filepath.Rel cannot relate is reported as neither.
func containment(absPath, absDir string) (contained, same bool) {
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil {
		return false, false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, false
	}
	return true, rel == "."
}
