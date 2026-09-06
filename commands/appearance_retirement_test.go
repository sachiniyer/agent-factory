package commands

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/require"
)

// The launch command must never call a daemon config mutation. Appearance is
// client-local; even a reachable daemon must retain pending listener/auth edits.
func TestPaletteRetirementLaunchNeverAppliesConfig(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "root.go", nil, 0)
	require.NoError(t, err)
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		require.NotContains(t, []string{"RequestApplyConfig", "RequestApplyTheme", "ApplyConfig", "ApplyTheme"}, sel.Sel.Name, "launch cannot activate pending listener/auth edits")
		return true
	})
}
func TestPaletteRetirementGetDiagnostic(t *testing.T) {
	for _, key := range []string{"theme", "theme.accent"} {
		err := unknownConfigKeyError(key)
		require.ErrorContains(t, err, "retired")
		require.ErrorContains(t, err, "appearance")
	}
}
