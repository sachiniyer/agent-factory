package ui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The repo-wide separator guard (#2579).
//
// #2399 converted the shared status-menu renderer from " • " to " · " and wrote
// the rule down in ui/menu.go. The sweep stopped there: 25 sibling separator
// sites across 8 files kept rendering a bullet, and the drift was visible SIDE
// BY SIDE in one frame — press `/` at 80x24 and the search overlay's hint row
// and the menu row one line below it disagree; open the task manager and one
// row uses `·` while another two rows down uses `•`.
//
// A per-surface assertion cannot prevent that: it only covers the surface
// somebody remembered to write it for, which is exactly how 25 sites drifted
// past a green suite. This scans every non-test Go source in the repo instead,
// so a new hint row cannot join the row of exceptions quietly.
//
// A bullet used as a LIST MARKER is correct and stays (`• Git branch: …`,
// `af reset`'s teardown plan). The rule distinguishes them structurally rather
// than by an allowlist that would itself rot: a bullet opening a line is a
// marker, a bullet inside one is a separator.
func TestNoBulletSeparatorsInUserFacingCopy(t *testing.T) {
	root := repoRootForConventions(t)

	var violations []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipConventionDir(d.Name()) && path != root {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		violations = append(violations, bulletSeparatorsIn(t, root, path)...)
		return nil
	})
	require.NoError(t, err)

	require.Empty(t, violations,
		"a bullet inside a line joins fragments, and CLAUDE.md's separator for that is \" · \" "+
			"(#2399/#2579). A bullet that OPENS a line is a list marker and is fine:\n%s",
		strings.Join(violations, "\n"))
}

func skipConventionDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", "dist", "testdata":
		return true
	}
	return false
}

// bulletSeparatorsIn reports every string literal in one file that uses "•" to
// join fragments rather than to open a list item. It reads literal VALUES from
// the AST, so a bullet in a comment (including the ones documenting this very
// rule) is not a finding.
func bulletSeparatorsIn(t *testing.T, root, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	require.NoError(t, err, "parse %s", path)

	rel, err := filepath.Rel(root, path)
	require.NoError(t, err)

	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			// A malformed literal would not compile; nothing to judge.
			return true
		}
		if !strings.Contains(value, "•") {
			return true
		}
		for _, line := range strings.Split(value, "\n") {
			// A leading bullet is the list marker; anything after it on the same
			// line is joining fragments.
			body := strings.TrimPrefix(strings.TrimLeft(line, " \t"), "•")
			if strings.Contains(body, "•") {
				found = append(found, "  "+rel+":"+strconv.Itoa(fset.Position(lit.Pos()).Line)+
					": "+strings.TrimSpace(line))
			}
		}
		return true
	})
	return found
}

func repoRootForConventions(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller could not locate this test file")
	return filepath.Dir(filepath.Dir(thisFile))
}
