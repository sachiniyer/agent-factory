package session

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/git"
)

// #7084 review, P3s on the setup note's path to the row.

// A worktree run's notes reach the row through noteWorktreeSetup and nothing
// else: a call site that read SetupOverlayError (or SetupFailureNote) directly
// would surface half of the run's outcome — the payload's — and silently drop
// a post_worktree_commands note, which no behaviour test of one site would see.
func TestWorktreeSetupNotesReachTheRowThroughOneHelper(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	helperCalls := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "SetupOverlayError":
				t.Errorf("%s: a worktree setup note read with SetupOverlayError() bypasses noteWorktreeSetup and drops post_worktree_commands notes", fset.Position(call.Pos()))
			case "SetupFailureNote":
				if name != "setup_hook_failed.go" {
					t.Errorf("%s: SetupFailureNote() belongs to noteWorktreeSetup alone", fset.Position(call.Pos()))
				}
			case "noteWorktreeSetup":
				helperCalls++
			}
			return true
		})
	}
	require.GreaterOrEqual(t, helperCalls, 4, "create, adopt, recover-rebuild and worktree-gone rebuild each record their run")
}

// A later run replaces a "did not run" note an earlier run left, and keeps
// every other note exactly as written — a note that itself contains "; ".
func TestNoteWorktreeSetupReplacesAStaleNotRunNote(t *testing.T) {
	gw, err := git.NewGitWorktreeFromStorage(t.TempDir(), t.TempDir(), "stale-note", "main", "", true, false)
	require.NoError(t, err)
	other := "entry 1 failed: exit status 3; output kept at /tmp/x.log"
	stale := git.HookNotRunNotePrefix + "couldn't read the project config for /repo in time (3 attempts) — retry"
	inst := &Instance{Title: "stale-note", setupHookFailed: stale + "; " + other}

	inst.noteWorktreeSetup(gw) // this run started its commands: no note

	require.Equal(t, other, inst.SetupHookFailed(),
		"the stale not-run note is gone and the other note is untouched")

	inst.noteWorktreeSetup(gw)
	require.Equal(t, other, inst.SetupHookFailed(), "replacing is idempotent")
}
