package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writerConventionExemptions are the calls in this package that put bytes on
// disk WITHOUT going through AtomicWriteFile, each with the reason it may.
//
// The list is the point of the test, not a way around it: every entry is a
// decision someone made, and an unlisted call is a writer that quietly opted out
// of symlink handling, atomicity, and AF-home hardening at once. Adding a line
// here should feel like asking for permission.
//
// Keys are file:function:call, NOT file:call. Keying by file alone would exempt
// every future call of that kind anywhere in the file — a new config-content
// os.OpenFile in config_load.go would have inherited the first-run
// materializer's exemption and passed silently (#3660 review). The function name
// is specific without being brittle: unlike a line number it survives edits
// above it, and renaming the function is exactly when someone should re-read
// why the exemption exists.
var writerConventionExemptions = map[string]writerExemption{
	"config_load.go:convertJSONToTOML:os.Rename": {calls: 1, reason: "moves the LEGACY config.json aside after conversion; it renames the old file rather than writing config content"},
	"config_load.go:writeConfigIfMissing:os.OpenFile": {calls: 1, reason: "first-run materialization, deliberately O_CREATE|O_EXCL so two racing af starts cannot both claim it — " +
		"exclusive create is the race guard, and it cannot meet an existing link because it fails if anything is already there"},
	"schema_migration.go:writeFileExclusive:os.OpenFile":  {calls: 1, reason: "schema-migration backup, same O_CREATE|O_EXCL contract: it must fail rather than overwrite an existing backup"},
	"filelock.go:TryWithFileLock:os.OpenFile":             {calls: 1, reason: "lock file, not config content"},
	"filelock.go:WithFileLockTimeout:os.OpenFile":         {calls: 1, reason: "lock file, not config content"},
	"filelock.go:WithFileLock:os.OpenFile":                {calls: 1, reason: "lock file, not config content"},
	"filelock.go:atomicWrite:os.CreateTemp":               {calls: 1, reason: "the shared writer's own temp file"},
	"filelock.go:atomicWrite:os.Rename":                   {calls: 1, reason: "the shared writer's own rename"},
	"project_registry.go:writeNewProjectRecord:os.Rename": {calls: 1, reason: "publishes a staged project DIRECTORY into place; the metadata FILE inside it was already written with AtomicWriteFile, and a directory rename has no content to follow a link with"},
	// The in-repo writer is the deliberate ASYMMETRY, not an oversight. A global
	// config.toml is the user's own file and a link there is their arrangement,
	// so AtomicWriteFileFollowingLink follows it (#3660). An in-repo
	// .agent-factory/config is checked into a repository someone else may
	// control, so its link is followed only as far as the repository goes:
	// inRepoConfigWriteTarget resolves the link and returns the TARGET's
	// directory when the target is still strictly inside the repo (#1092 — the
	// link is preserved and its target rewritten), and refuses the save naming
	// both ends when it is not. The O_NOFOLLOW pin is what makes that check hold
	// at the moment of the write rather than merely at the moment of the check:
	// the rename goes through a directory fd opened on the RESOLVED directory
	// without following links, so a parent-dir link swapped in afterwards is
	// rejected instead of followed. AtomicWriteFile can express neither half —
	// do not "fix" it to use that.
	"inrepo.go:atomicWriteFileInDirNoFollow:golang.org/x/sys/unix.Renameat": {calls: 1, reason: "in-repo writer's rename through a directory fd opened O_NOFOLLOW; a config-file link IS followed when its target stays inside the repo (#1092) — what the pin refuses is a parent-dir link swapped in after the containment check"},
	"inrepo.go:atomicWriteFileInDirNoFollow:golang.org/x/sys/unix.Unlinkat": {calls: 1, reason: "the same writer's temp-file cleanup"},
}

// writerExemption records how many calls of that kind the function is allowed
// and why. The COUNT is the point: keying by file, function and API alone let a
// SECOND os.OpenFile in writeConfigIfMissing inherit the first one's permission,
// so the guard would have accepted the exact bypass it exists to prevent
// (#3660 review). A line number would be specific but brittle against edits
// above it; a count is specific where it matters and fails closed — add a call
// and the number stops matching, which is precisely when someone should have to
// look.
type writerExemption struct {
	calls  int
	reason string
}

// approvedWriters are the shared writers a config-package caller may reach disk
// through. There are THREE now, one per answer to "what does a write do when its
// destination is a symlink" (#3672), and the scan below names them so a rename
// that left one behind fails here rather than quietly shrinking the convention
// to whatever still compiles.
var approvedWriters = map[string]string{
	"AtomicWriteFile":              "REPLACE the link — os.Rename's own behaviour, and the unchanged default",
	"AtomicWriteFileFollowingLink": "FOLLOW the link — the global config (#3660) and the task store (#3672)",
	"AtomicWriteFileRefusingLink":  "REFUSE the link, naming both ends — af's own managed files (#3672)",
}

// TestEveryConfigWriterGoesThroughAtomicWriteFile pins the convention that is
// the only thing holding this package's writers together.
//
// #3660 had to be fixed once, in AtomicWriteFile, precisely because every writer
// reaches disk through it. Nothing in the language enforces that: a new writer
// calling os.WriteFile directly would compile, pass its own tests, and silently
// reacquire the symlink bug this change just closed — along with the atomicity
// and AF-home hardening that helper also provides.
//
// So the scan is the guard. It parses every non-test file in the package and
// fails on any disk-writing call that is none of the approvedWriters and not a
// listed exemption.
func TestEveryConfigWriterGoesThroughAtomicWriteFile(t *testing.T) {
	// Calls that put FILE CONTENT on disk or move a file into place.
	// os.Create/WriteFile are the ones a new writer reaches for by habit; the
	// rest are here so an exemption cannot be smuggled in under a different
	// spelling.
	//
	// Directory creation (MkdirAll/Mkdir) is deliberately NOT here. It writes no
	// content, so none of symlink following, atomicity, or the AF-home hardening
	// applies to it — listing it would force half a dozen exemptions that say
	// nothing and would train the next reader to skim the list.
	writingCalls := map[string]bool{
		"os.WriteFile": true, "os.Create": true, "os.OpenFile": true,
		"os.CreateTemp": true, "os.Rename": true,
		"os.Symlink": true, "os.Link": true,
		// The *at syscalls too, or the in-repo writer's shape would be a way to
		// add an unguarded writer that the os.* list never sees.
		"golang.org/x/sys/unix.Renameat": true, "golang.org/x/sys/unix.Unlinkat": true,
		"golang.org/x/sys/unix.Linkat": true, "golang.org/x/sys/unix.Symlinkat": true,
		"golang.org/x/sys/unix.Mkdirat": true,
	}

	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	seen := map[string]int{}
	declared := map[string]bool{}
	var offenders []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err, "parse %s", name)

		// Local qualifier -> import path, so an alias cannot hide a writer.
		// A dot-import would put os.WriteFile in scope unqualified; it is
		// rejected outright rather than silently unmatched.
		imports := map[string]string{}
		for _, spec := range file.Imports {
			path := strings.Trim(spec.Path.Value, `"`)
			name := path[strings.LastIndex(path, "/")+1:]
			if spec.Name != nil {
				require.NotEqual(t, ".", spec.Name.Name,
					"%s dot-imports %q; this scan cannot see unqualified writer calls", name, path)
				name = spec.Name.Name
			}
			imports[name] = path
		}

		// Walk per top-level declaration so every call carries the function it
		// sits in; a call outside any function body gets "" and can never match
		// an exemption, which is the conservative direction.
		for _, decl := range file.Decls {
			enclosing := ""
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name != nil {
				enclosing = fn.Name.Name
				if fn.Recv == nil {
					declared[fn.Name.Name] = true
				}
			}
			ast.Inspect(decl, func(n ast.Node) bool {
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
				// Resolve the qualifier through the file's IMPORTS rather than
				// trusting the identifier text. `import stdos "os"` makes an
				// ordinary os.WriteFile read as stdos.WriteFile, which matched
				// nothing and slipped through a guard whose whole claim is that
				// it cannot be slipped through (#3660 review). Valid Go, and no
				// compiler error to catch it.
				qualified := imports[pkg.Name] + "." + sel.Sel.Name
				if !writingCalls[qualified] {
					return true
				}
				key := name + ":" + enclosing + ":" + qualified
				if _, exempt := writerConventionExemptions[key]; exempt {
					seen[key]++
					return true
				}
				offenders = append(offenders,
					fset.Position(call.Pos()).String()+": "+qualified+" (in "+enclosing+")")
				return true
			})
		}
	}

	// The three approved writers must actually be here. Without this the scan
	// would keep passing after one was renamed or deleted — describing a
	// convention with a hole in it, which is worse than no scan at all.
	for name, answer := range approvedWriters {
		assert.True(t, declared[name],
			"approvedWriters names %s (%s) but this package declares no such function; "+
				"if it was renamed, rename it here and re-read every caller's decision (#3672)",
			name, answer)
	}

	assert.Empty(t, offenders,
		"these calls write to disk without going through one of the three shared writers "+
			"(AtomicWriteFile, AtomicWriteFileFollowingLink, AtomicWriteFileRefusingLink).\n"+
			"A config writer must use one — that is where symlink policy (#3660, #3672), atomicity, and "+
			"AF-home hardening live, and adding a writer that skips them silently reacquires all three bugs.\n"+
			"If a call genuinely cannot use one, add it to writerConventionExemptions with the reason.")

	// A stale exemption is its own defect: it makes the list look considered
	// while describing code that no longer exists, and the next reader trusts it.
	// A count that no longer matches is the same defect in the other direction —
	// the function grew a call nobody re-examined.
	for key, exempt := range writerConventionExemptions {
		assert.Equal(t, exempt.calls, seen[key],
			"exemption %q covers %d call(s) but the code has %d — a new one does not inherit the old one's reason (%s)",
			key, exempt.calls, seen[key], exempt.reason)
	}

	// And the guard must actually be pointed at this package.
	assert.FileExists(t, filepath.Join(".", "filelock.go"))
}

// followingWriterPackages are the packages allowed to call the symlink-FOLLOWING
// writer, each with the reason its file is the user's rather than af's.
//
// The list is the fence, so a line here is the whole of the permission and the
// reason is what the next reader is owed. Do not add one to make a caller
// compile.
var followingWriterPackages = map[string]string{
	"config": "the global config.toml — the file the variant was decided for (#3660): " +
		"a link into a dotfiles repository is the user's arrangement, and every editor writes through it",
	"task": "tasks.json — the one USER-AUTHORED store in the af home (#3672). People write it by " +
		"hand, so a user may reasonably keep it in dotfiles the way they keep config.toml, and af " +
		"rewriting the target rather than replacing the link is what that arrangement means. This is a " +
		"second package, not a weakening: af's own managed files went the other way in the same " +
		"decision, to AtomicWriteFileRefusingLink",
}

// TestFollowingWriterStaysInsideTheConfigPackage is the fence that keeps the
// symlink-following promise scoped to the files it was decided for.
//
// AtomicWriteFileFollowingLink exists for content af rewrites on the USER's
// behalf rather than owns: the global config (#3660) and the task store
// (#3672). af's own managed files — the bearer token, autostart units, the
// editor-origin secret, the VS Code owner record, the upgrade interlock, the
// auto-update cache, the event-queue cursor, skills and plugins — refuse a link
// outright (AtomicWriteFileRefusingLink), because "write through whatever this
// points at" is a stronger promise than any of them offered and replacing a link
// silently is not what anyone asked for either.
//
// Nothing in the language stops a caller elsewhere from picking the follow
// variant because it sounds safer. This does: the promise spreads only by
// someone editing followingWriterPackages and saying why.
func TestFollowingWriterStaysInsideTheConfigPackage(t *testing.T) {
	const followingWriter = "AtomicWriteFileFollowingLink"

	root, err := filepath.Abs("..")
	require.NoError(t, err)

	var outside []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip ONLY trees that cannot hold Go by construction. Skipping by
			// package name is how a fence acquires a hole: `web` was on this list
			// as "frontend", but web/embed.go is a real Go package, so a call
			// there would have bypassed a guard whose whole job is to have no
			// exceptions (#3660 review). The .go suffix filter below is what
			// decides; this is only about not walking hundreds of thousands of
			// files that cannot match it.
			switch d.Name() {
			case ".git", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// A permitted package, named with its reason above. Matched on the
		// DIRECTORY, not on a path prefix: "config" must not silently cover a
		// future config/subpkg/ that never took the decision.
		if _, permitted := followingWriterPackages[filepath.Base(filepath.Dir(path))]; permitted &&
			filepath.Dir(filepath.Dir(path)) == root {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(data), followingWriter) {
			rel, _ := filepath.Rel(root, path)
			outside = append(outside, rel)
		}
		return nil
	})
	require.NoError(t, err)

	assert.Empty(t, outside,
		"%s follows a symlink, and that promise was decided for the global config (#3660) and the task "+
			"store (#3672) only — see followingWriterPackages for the reason each was allowed.\n"+
			"Everything else in the af home is a file af MANAGES, where following a link is a promise the "+
			"caller never made: daemon/autostart.go, for one, writes a unit and later removes it, so a "+
			"followed write would leave the cleanup unlinking a link whose content went elsewhere.\n"+
			"Use AtomicWriteFileRefusingLink for an af-managed file, AtomicWriteFile for the unchanged "+
			"replace semantics, or take the decision on #3672 first.", followingWriter)
}
