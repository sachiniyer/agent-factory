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
var writerConventionExemptions = map[string]string{
	"config_load.go:convertJSONToTOML:os.Rename": "moves the LEGACY config.json aside after conversion; it renames the old file rather than writing config content",
	"config_load.go:writeConfigIfMissing:os.OpenFile": "first-run materialization, deliberately O_CREATE|O_EXCL so two racing af starts cannot both claim it — " +
		"exclusive create is the race guard, and it cannot meet an existing link because it fails if anything is already there",
	"schema_migration.go:writeFileExclusive:os.OpenFile":  "schema-migration backup, same O_CREATE|O_EXCL contract: it must fail rather than overwrite an existing backup",
	"filelock.go:TryWithFileLock:os.OpenFile":             "lock file, not config content",
	"filelock.go:WithFileLockTimeout:os.OpenFile":         "lock file, not config content",
	"filelock.go:WithFileLock:os.OpenFile":                "lock file, not config content",
	"filelock.go:atomicWrite:os.CreateTemp":               "the shared writer's own temp file",
	"filelock.go:atomicWrite:os.Rename":                   "the shared writer's own rename",
	"project_registry.go:writeNewProjectRecord:os.Rename": "publishes a staged project DIRECTORY into place; the metadata FILE inside it was already written with AtomicWriteFile, and a directory rename has no content to follow a link with",
	// The in-repo writer is the deliberate ASYMMETRY, not an oversight. A global
	// config.toml is the user's own file and a link there is their arrangement,
	// so AtomicWriteFile follows it (#3660). An in-repo .agent-factory/config is
	// checked into a repository someone else may control, so a link there could
	// aim af's write at any path the cloner never chose — which is why
	// atomicWriteFileInDirNoFollow resolves the destination and then pins the
	// directory with O_NOFOLLOW, rejecting a parent-dir link swapped in after the
	// guard rather than following it. Do not "fix" it to use AtomicWriteFile.
	"inrepo.go:atomicWriteFileInDirNoFollow:unix.Renameat": "in-repo writer's O_NOFOLLOW rename; following a link from a checked-in config is the thing it exists to prevent",
	"inrepo.go:atomicWriteFileInDirNoFollow:unix.Unlinkat": "the same writer's temp-file cleanup",
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
// fails on any disk-writing call that is neither AtomicWriteFile nor a listed
// exemption.
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
		"unix.Renameat": true, "unix.Unlinkat": true, "unix.Linkat": true,
		"unix.Symlinkat": true, "unix.Mkdirat": true,
	}

	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	used := map[string]bool{}
	var offenders []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err, "parse %s", name)

		// Walk per top-level declaration so every call carries the function it
		// sits in; a call outside any function body gets "" and can never match
		// an exemption, which is the conservative direction.
		for _, decl := range file.Decls {
			enclosing := ""
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name != nil {
				enclosing = fn.Name.Name
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
				qualified := pkg.Name + "." + sel.Sel.Name
				if !writingCalls[qualified] {
					return true
				}
				key := name + ":" + enclosing + ":" + qualified
				if _, exempt := writerConventionExemptions[key]; exempt {
					used[key] = true
					return true
				}
				offenders = append(offenders,
					fset.Position(call.Pos()).String()+": "+qualified+" (in "+enclosing+")")
				return true
			})
		}
	}

	assert.Empty(t, offenders,
		"these calls write to disk without going through AtomicWriteFile (or its "+
			"AtomicWriteFileFollowingLink variant).\n"+
			"A config writer must use it — that is where symlink following (#3660), atomicity, and "+
			"AF-home hardening live, and adding a writer that skips it silently reacquires all three bugs.\n"+
			"If a call genuinely cannot use it, add it to writerConventionExemptions with the reason.")

	// A stale exemption is its own defect: it makes the list look considered
	// while describing code that no longer exists, and the next reader trusts it.
	for key := range writerConventionExemptions {
		assert.True(t, used[key], "exemption %q no longer matches any call — remove it", key)
	}

	// And the guard must actually be pointed at this package.
	assert.FileExists(t, filepath.Join(".", "filelock.go"))
}

// TestFollowingWriterStaysInsideTheConfigPackage is the fence that keeps the
// symlink-following promise scoped to the file it was decided for.
//
// AtomicWriteFileFollowingLink exists for the GLOBAL CONFIG: the one file af
// rewrites on the user's behalf rather than owns, where a link into a dotfiles
// repository is the user's arrangement and every editor writes through it
// (#3660). af's own managed files — the bearer token, autostart units, the task
// store, skills, plugins — deliberately keep the plain writer, because "write
// through whatever this points at" is a stronger promise than any of them asked
// for, and #3672 records that as a per-caller decision still to be made rather
// than a default to inherit.
//
// Nothing in the language stops a caller elsewhere from picking the follow
// variant because it sounds safer. This does: the promise spreads only by
// someone editing this list and saying why.
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
		// The config package is where the variant lives and is used.
		if filepath.Dir(path) == filepath.Join(root, "config") {
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
		"%s follows a symlink, and that promise was decided for the global config only (#3660, #3672).\n"+
			"A caller outside config/ writes a file af manages, where following a link changes semantics "+
			"nobody asked for — daemon/autostart.go, for one, cleans up with os.Remove of the SAME path it "+
			"wrote, so a followed write would leave the cleanup unlinking a link whose content went elsewhere.\n"+
			"Use AtomicWriteFile, or take the decision on #3672 first.", followingWriter)
}
