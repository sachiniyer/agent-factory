package session

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The orchestrator section uses --self, which resolves the calling session, so
// it is taught only inside af and never in the plugin reference.
func TestOrchestratorUsageIsInsideOnly(t *testing.T) {
	assert.Contains(t, afUsageReference, afUsageOrchestratorInside)
	for _, s := range []string{
		"--orchestrate", "af backlog orchestrate <group> --self", "af backlog orchestrate <group> --clear",
		"--handoff-file", "af backlog orchestrate --self --stay", "af backlog escalate <id>",
		"Events in effort", "af backlog done <id>",
	} {
		assert.Contains(t, afUsageOrchestratorInside, s)
		assert.NotContains(t, AfPluginUsageReference, s)
	}
}

// The section lists one line per orchestrator event kind. Every Event* string
// constant in package backlog must have exactly one line, and every line must
// be one of them: a kind added to backlog fails here until the agent is taught
// it, and the text cannot teach a kind af never sends.
func TestOrchestratorUsageNamesEveryEventKind(t *testing.T) {
	kinds := backlogEventKinds(t)
	require.Contains(t, kinds, backlog.EventQuestion, "the parse found backlog's event constants")
	require.Contains(t, kinds, backlog.EventEscalationAnswered)

	events := afUsageOrchestratorInside[strings.Index(afUsageOrchestratorInside, "Events in effort"):]
	var taught []string
	for _, m := range regexp.MustCompile(`(?m)^  ([a-z_]+): `).FindAllStringSubmatch(events, -1) {
		taught = append(taught, m[1])
	}
	slices.Sort(taught)
	assert.Equal(t, kinds, taught)
}

// backlogEventKinds returns the values of every Event* string constant in
// package backlog's non-test sources, sorted.
func backlogEventKinds(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join("..", "backlog")
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	require.NoError(t, err)
	var kinds []string
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, decl := range f.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.CONST {
					continue
				}
				for _, spec := range gen.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, name := range vs.Names {
						if !strings.HasPrefix(name.Name, "Event") || i >= len(vs.Values) {
							continue
						}
						lit, ok := vs.Values[i].(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						v, err := strconv.Unquote(lit.Value)
						require.NoError(t, err)
						kinds = append(kinds, v)
					}
				}
			}
		}
	}
	slices.Sort(kinds)
	return kinds
}
