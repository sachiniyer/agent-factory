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

type productionString struct {
	position string
	value    string
}

func productionStrings(t *testing.T) []productionString {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repoRoot := filepath.Dir(filepath.Dir(thisFile))

	var found []productionString
	fset := token.NewFileSet()
	for _, dir := range []string{"app", "ui"} {
		err := filepath.WalkDir(filepath.Join(repoRoot, dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(node ast.Node) bool {
				lit, ok := node.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(lit.Value)
				if err == nil {
					found = append(found, productionString{
						position: fset.Position(lit.Pos()).String(),
						value:    value,
					})
				}
				return true
			})
			return nil
		})
		require.NoError(t, err)
	}
	return found
}

func TestProductionInlineSeparatorsUseMiddleDot(t *testing.T) {
	var bad []string
	for _, literal := range productionStrings(t) {
		if strings.Contains(literal.value, " • ") {
			bad = append(bad, literal.position)
		}
	}
	require.Empty(t, bad, "inline fragment separators must use middle dots")
}

func TestClauseSeparatorsUseEmDash(t *testing.T) {
	var bad []string
	for _, literal := range productionStrings(t) {
		for _, clause := range []string{" · selected:", " · Scroll", " · this cannot"} {
			if strings.Contains(literal.value, clause) {
				bad = append(bad, literal.position)
			}
		}
	}
	require.Empty(t, bad, "middle dots join fragments; em dashes set off clauses")
}
