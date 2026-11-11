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

// #7088 review, P3: #7082 (9d9cdfa4ab) wrote one not-run note with a "; "
// inside it, the separator the row joins notes with. Dropping only the part
// that starts with the prefix left the rest on the row — "run them in the
// worktree yourself, or recreate the session once it recovers: …". The next
// run drops that whole note, head and tail, and nothing else.
func TestNoteWorktreeSetupDropsTheWholeNoteAnEarlierBuildWrote(t *testing.T) {
	gw, err := git.NewGitWorktreeFromStorage(t.TempDir(), t.TempDir(), "legacy-note", "main", "", true, false)
	require.NoError(t, err)
	other := "entry 1 failed: exit status 3; output kept at /tmp/x.log"
	legacy := "post_worktree_commands did not run: couldn't read the project config for /repo in time (3 attempts) — " +
		"the machine may be under heavy load, or its filesystem may not be responding; " +
		"run them in the worktree yourself, or recreate the session once it recovers: read checkout marker for /repo: context deadline exceeded"
	for _, tc := range []struct{ name, notes, want string }{
		{"alone", legacy, ""},
		{"before another note", legacy + "; " + other, other},
		{"after another note", other + "; " + legacy, other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := &Instance{Title: "legacy-note", setupHookFailed: tc.notes}
			inst.noteWorktreeSetup(gw)
			require.Equal(t, tc.want, inst.SetupHookFailed(), "no fragment of the earlier note is left on the row")
		})
	}
}
