package pathutil

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsStrictlyInside(t *testing.T) {
	sep := string(filepath.Separator)
	cases := []struct {
		name    string
		absBase string
		absDir  string
		want    bool
	}{
		{"child of filesystem root", filepath.Join(sep, ".agent-factory", "config.json"), sep, true},
		{"filesystem root is not inside itself", sep, sep, false},
		{"child of normal dir", filepath.Join(sep, "repo", ".agent-factory", "config.json"), filepath.Join(sep, "repo"), true},
		{"dir is not inside itself", filepath.Join(sep, "repo"), filepath.Join(sep, "repo"), false},
		{"sibling sharing a string prefix", filepath.Join(sep, "repo2"), filepath.Join(sep, "repo"), false},
		{"parent of the dir", sep, filepath.Join(sep, "repo"), false},
		{"path outside the dir", filepath.Join(sep, "outside", "config.json"), filepath.Join(sep, "repo"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsStrictlyInside(tc.absBase, tc.absDir),
				"IsStrictlyInside(%q, %q)", tc.absBase, tc.absDir)
		})
	}
}

func TestIsAtOrInside(t *testing.T) {
	sep := string(filepath.Separator)
	cases := []struct {
		name    string
		absPath string
		absDir  string
		want    bool
	}{
		{"child of filesystem root", filepath.Join(sep, ".agent-factory", "config.json"), sep, true},
		{"filesystem root is inside itself", sep, sep, true},
		{"child of normal dir", filepath.Join(sep, "repo", ".agent-factory", "config.json"), filepath.Join(sep, "repo"), true},
		{"dir is inside itself", filepath.Join(sep, "repo"), filepath.Join(sep, "repo"), true},
		{"nested descendant", filepath.Join(sep, "repo", "a", "b", "c"), filepath.Join(sep, "repo"), true},
		{"sibling sharing a string prefix", filepath.Join(sep, "x", "wt-backup"), filepath.Join(sep, "x", "wt"), false},
		{"parent of the dir", sep, filepath.Join(sep, "repo"), false},
		{"path outside the dir", filepath.Join(sep, "outside", "config.json"), filepath.Join(sep, "repo"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsAtOrInside(tc.absPath, tc.absDir),
				"IsAtOrInside(%q, %q)", tc.absPath, tc.absDir)
		})
	}
}

// TestContainmentVariantsDifferOnlyOnSelf pins the single documented bit
// separating the two exported forms: over the same inputs they agree
// everywhere EXCEPT on the directory itself, where at-or-inside says yes and
// strictly-inside says no. A helper that collapsed them — or a caller reaching
// for the wrong one — shows up here rather than as a guard that silently
// refuses (or permits) a directory against itself.
func TestContainmentVariantsDifferOnlyOnSelf(t *testing.T) {
	sep := string(filepath.Separator)
	repo := filepath.Join(sep, "repo")
	paths := []string{
		sep,
		repo,
		filepath.Join(repo, "a"),
		filepath.Join(repo, "a", "b"),
		filepath.Join(sep, "repo2"),
		filepath.Join(sep, "outside"),
	}
	for _, dir := range []string{sep, repo} {
		for _, p := range paths {
			atOrInside := IsAtOrInside(p, dir)
			strictly := IsStrictlyInside(p, dir)
			if p == dir {
				assert.True(t, atOrInside, "IsAtOrInside(%q, %q) must accept the dir itself", p, dir)
				assert.False(t, strictly, "IsStrictlyInside(%q, %q) must reject the dir itself", p, dir)
				continue
			}
			assert.Equal(t, atOrInside, strictly,
				"the two forms must agree off the self case: %q vs dir %q", p, dir)
		}
	}
}
