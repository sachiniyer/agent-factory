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
