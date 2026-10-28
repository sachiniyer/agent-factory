package theme

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Fixtures exercise arbitrary PTY bytes; production chrome must consume roles.
// Reject constructors as well as hex strings, so named constants, ANSI indexes,
// and adaptive colour pairs cannot introduce another palette.
func TestChromeUsesGeneratedColors(t *testing.T) {
	root := filepath.Join("..", "..")
	hex := regexp.MustCompile(`(?i)^#[0-9a-f]{3,8}$|(?:38|48);(?:2;[0-9]+;[0-9]+;[0-9]+|5;[0-9]+)|\x1b\[(?:[0-9]+;)*(?:3[0-7]|4[0-7]|9[0-7]|10[0-7])(?:;[0-9]+)*m`)
	for _, dir := range []string{"ui", "app"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || path == filepath.Join(root, "ui", "theme", "theme.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			packages := map[string]string{}
			for _, imp := range f.Imports {
				name, _ := strconv.Unquote(imp.Path.Value)
				alias := filepath.Base(name)
				if imp.Name != nil {
					alias = imp.Name.Name
				}
				packages[alias] = name
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					value, _ := strconv.Unquote(lit.Value)
					if hex.MatchString(value) {
						t.Errorf("%s: local colour literal; use generated roles", fset.Position(lit.Pos()))
					}
				}
				if value, ok := n.(*ast.CompositeLit); ok {
					if typ, ok := value.Type.(*ast.SelectorExpr); ok {
						if pkg, ok := typ.X.(*ast.Ident); ok && strings.HasSuffix(packages[pkg.Name], "/lipgloss") && (typ.Sel.Name == "AdaptiveColor" || typ.Sel.Name == "CompleteColor" || typ.Sel.Name == "CompleteAdaptiveColor") {
							t.Errorf("%s: local palette definition; use generated roles", fset.Position(value.Pos()))
						}
					}
				}
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				source := packages[pkg.Name]
				if (strings.HasSuffix(source, "/lipgloss") && (sel.Sel.Name == "Color" || sel.Sel.Name == "ANSIColor")) || (strings.HasSuffix(source, "/termenv") && (sel.Sel.Name == "RGBColor" || sel.Sel.Name == "ANSIColor" || sel.Sel.Name == "ANSI256Color")) {
					t.Errorf("%s: local colour constructor; use generated roles", fset.Position(call.Pos()))
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
