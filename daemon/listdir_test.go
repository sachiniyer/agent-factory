package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the directory-listing read behind the web Add-project picker (#2788).
//
// The load-bearing property is the one the issue names: an unreadable directory
// must come back as an ERROR, never as a successful empty listing. That is the
// fabricated-negative shape this repo keeps re-learning — a confident wrong
// answer ("this directory is empty") is worse than a failure, because the UI
// renders it as fact and the user concludes their repo is not there.

// mkdirAll is require.NoError'd os.MkdirAll, for fixture readability.
func mkdirAll(t *testing.T, path string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(path, 0o755))
	return path
}

// mkrepo makes dir look like a git checkout by planting a .git directory.
func mkrepo(t *testing.T, path string) string {
	t.Helper()
	mkdirAll(t, filepath.Join(path, ".git"))
	return path
}

// entryNames is the listing's entry names, in the order the daemon returned them.
func entryNames(resp ListDirectoryResponse) []string {
	out := make([]string, 0, len(resp.Entries))
	for _, e := range resp.Entries {
		out = append(out, e.Name)
	}
	return out
}

// canonical is the test's INDEPENDENT oracle for "where this path really is":
// plain EvalSymlinks, which works because every path it is handed here exists.
// Deliberately not pathutil.ResolveForCompare — computing the expectation with
// the same function under test would assert only that the code calls itself.
func canonical(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	return resolved
}

// findEntry returns the named entry, or a zero value and false.
func findEntry(resp ListDirectoryResponse, name string) (DirectoryEntry, bool) {
	for _, e := range resp.Entries {
		if e.Name == name {
			return e, true
		}
	}
	return DirectoryEntry{}, false
}

func TestListDirectory_ListsOnlyDirectoriesAndMarksRepos(t *testing.T) {
	root := t.TempDir()
	mkrepo(t, filepath.Join(root, "checkout"))
	mkdirAll(t, filepath.Join(root, "plain"))
	require.NoError(t, os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o644))

	resp, err := listDirectory(root)
	require.NoError(t, err)

	assert.Equal(t, []string{"checkout", "plain"}, entryNames(resp),
		"files are noise in a project picker — directories only")

	checkout, ok := findEntry(resp, "checkout")
	require.True(t, ok)
	assert.True(t, checkout.IsRepo, "a checkout with .git is the only thing that can become a project")
	assert.Equal(t, filepath.Join(resp.Path, "checkout"), checkout.Path,
		"the entry carries the daemon-resolved path the client navigates/registers by")

	plain, ok := findEntry(resp, "plain")
	require.True(t, ok)
	assert.False(t, plain.IsRepo, "a plain directory is navigable but not a project target")

	assert.False(t, resp.IsRepo, "the listed directory itself is not a checkout")
	assert.False(t, resp.Truncated)
}

// A .git FILE is what a linked worktree carries; it is as much a checkout as a
// .git directory, and marking only the directory form would render half the
// real repos on a developer's disk as un-selectable.
func TestListDirectory_MarksGitFileCheckoutAsRepo(t *testing.T) {
	root := t.TempDir()
	linked := mkdirAll(t, filepath.Join(root, "linked"))
	require.NoError(t, os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: /elsewhere\n"), 0o644))

	resp, err := listDirectory(root)
	require.NoError(t, err)
	entry, ok := findEntry(resp, "linked")
	require.True(t, ok)
	assert.True(t, entry.IsRepo, "a .git file (linked worktree) is a checkout too")
}

// Descending INTO a checkout must still let it be picked, so the response
// reports whether the listed directory is itself a repo.
func TestListDirectory_ReportsWhetherTheListedDirectoryIsARepo(t *testing.T) {
	repo := mkrepo(t, filepath.Join(t.TempDir(), "checkout"))
	mkdirAll(t, filepath.Join(repo, "src"))

	resp, err := listDirectory(repo)
	require.NoError(t, err)
	assert.True(t, resp.IsRepo, "you must be able to pick the repo you just navigated into")
	assert.Equal(t, []string{"src", ".git"}, entryNames(resp), "hidden directories sort last, not away")
}

// THE test the issue is about: an unreadable directory must be an error, not an
// empty list. os.ReadDir returns partial entries ALONGSIDE its error, so a
// handler that ignores the error answers 200 with [] — indistinguishable from a
// genuinely empty directory.
func TestListDirectory_PermissionDeniedIsAnErrorNotAnEmptyList(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permissions")
	}
	root := t.TempDir()
	locked := mkdirAll(t, filepath.Join(root, "locked"))
	mkdirAll(t, filepath.Join(locked, "hidden-child"))
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	// Never assert a negative the environment cannot produce. root bypasses the
	// DAC check, so under a root test runner this directory IS readable and the
	// assertion below would be testing nothing — say so instead of passing.
	if _, probe := os.ReadDir(locked); probe == nil {
		t.Skip("this runner can read a 0o000 directory (running as root?), so permission denial is unobservable here")
	}

	resp, err := listDirectory(locked)
	require.Error(t, err, "an unreadable directory must not answer with a successful empty listing")
	assert.Contains(t, err.Error(), "permission denied", "the message must name the reason, not just fail")
	assert.Contains(t, err.Error(), locked, "the message must name the directory that was refused")
	assert.Empty(t, resp.Entries, "no partial listing may leak out alongside the error")
}

func TestListDirectory_RejectsNonDirectoryAndMissingPaths(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "notes.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))

	_, err := listDirectory(file)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")

	_, err = listDirectory(filepath.Join(root, "nope"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no such directory")
}

// A relative path has no meaning on the daemon: it would resolve against the
// daemon's own cwd (/ under systemd), silently listing the wrong tree.
func TestListDirectory_RejectsRelativePaths(t *testing.T) {
	_, err := listDirectory("relative/path")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute")

	// "~user" is unexpandable, so it must be refused rather than treated as a
	// literal directory named "~user".
	_, err = listDirectory("~someone/repos")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute")
}

func TestListDirectory_DefaultsToHomeAndExpandsTilde(t *testing.T) {
	home := t.TempDir()
	mkdirAll(t, filepath.Join(home, "repos"))
	t.Setenv("HOME", home)

	// The canonical spelling of the temp home: on macOS /var is a symlink to
	// /private/var, so the daemon's answer is the RESOLVED path and comparing
	// against the raw t.TempDir() would fail there and only there (#2110).
	canonicalHome := canonical(t, home)

	empty, err := listDirectory("")
	require.NoError(t, err)
	assert.Equal(t, canonicalHome, empty.Path, "an empty path starts the picker at the daemon host's home")

	tilde, err := listDirectory("~/repos")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(canonicalHome, "repos"), tilde.Path)
	assert.Equal(t, canonicalHome, tilde.Parent, "the parent affordance comes from the daemon, not client string surgery")
	assert.Equal(t, canonicalHome, tilde.Home, "the client needs the host's home to offer a 'Home' shortcut")
}

// A daemon whose home cannot be resolved still browses absolute paths — the
// listing just carries no Home, which is how the client knows to hide the
// shortcut rather than render one that goes nowhere.
func TestListDirectory_UnresolvableHomeStillBrowsesAbsolutePaths(t *testing.T) {
	root := t.TempDir()
	mkdirAll(t, filepath.Join(root, "repos"))
	t.Setenv("HOME", "")
	if _, err := os.UserHomeDir(); err == nil {
		t.Skip("this platform resolves a home without $HOME, so the unresolvable case is unobservable here")
	}

	resp, err := listDirectory(root)
	require.NoError(t, err, "an absolute path needs no home")
	assert.Equal(t, []string{"repos"}, entryNames(resp))
	assert.Empty(t, resp.Home, "no home means no Home shortcut, not a wrong one")

	_, err = listDirectory("")
	require.Error(t, err, "with no home there is no sensible default to start at")
	assert.Contains(t, err.Error(), "absolute")
}

// The parent is computed server-side from the CANONICAL path, so a client never
// has to do the ".." string surgery the issue rules out. At the filesystem root
// there is no parent, and the affordance must disappear rather than loop.
func TestListDirectory_ParentIsEmptyAtTheFilesystemRoot(t *testing.T) {
	resp, err := listDirectory(string(filepath.Separator))
	require.NoError(t, err)
	assert.Equal(t, string(filepath.Separator), resp.Path)
	assert.Empty(t, resp.Parent, "the root has no parent; an up affordance there would loop")
}

// A "..", or any other spelling, is resolved by the daemon before it is used —
// and the path it answers with is the canonical one, so the client's next
// request round-trips to the same place.
func TestListDirectory_CanonicalizesTheRequestedPath(t *testing.T) {
	root := t.TempDir()
	mkdirAll(t, filepath.Join(root, "a", "b"))
	canonicalRoot := canonical(t, root)

	resp, err := listDirectory(filepath.Join(root, "a", "b", "..", "..", "a"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(canonicalRoot, "a"), resp.Path)
	assert.Equal(t, []string{"b"}, entryNames(resp))
}

// A symlinked directory stays navigable, but the path it hands back is the
// TARGET's canonical path — so descending through one lands the client (and its
// header) on where it actually is, rather than on a spelling that resolves
// somewhere else on the next request.
func TestListDirectory_SymlinkedDirectoryResolvesToItsTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	root := t.TempDir()
	target := mkrepo(t, filepath.Join(root, "real"))
	view := mkdirAll(t, filepath.Join(root, "view"))
	require.NoError(t, os.Symlink(target, filepath.Join(view, "link")))
	require.NoError(t, os.Symlink(filepath.Join(root, "gone"), filepath.Join(view, "dangling")))

	resp, err := listDirectory(view)
	require.NoError(t, err)
	assert.Equal(t, []string{"link"}, entryNames(resp), "a dangling symlink is not a navigable directory")

	link, ok := findEntry(resp, "link")
	require.True(t, ok)
	assert.True(t, link.IsSymlink, "the client must be able to show that this row is not where it looks")
	assert.Equal(t, canonical(t, target), link.Path,
		"the path is the resolved target, so a descent cannot silently land elsewhere")
	assert.True(t, link.IsRepo)
}

// A directory with tens of thousands of children must not become a
// tens-of-megabytes response. The cap is REPORTED, not silent: a truncated
// listing that looked complete would be the same confident wrong answer as an
// empty one.
func TestListDirectory_CapsEntriesAndSaysSo(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < maxDirectoryEntries+5; i++ {
		mkdirAll(t, filepath.Join(root, fmt.Sprintf("d%05d", i)))
	}

	resp, err := listDirectory(root)
	require.NoError(t, err)
	assert.Len(t, resp.Entries, maxDirectoryEntries)
	assert.True(t, resp.Truncated, "a silent cap reads as 'that is everything'")
}

func TestListDirectory_SortsHiddenLastThenCaseInsensitively(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{".cache", "Zebra", "apple", ".config", "Banana"} {
		mkdirAll(t, filepath.Join(root, name))
	}

	resp, err := listDirectory(root)
	require.NoError(t, err)
	assert.Equal(t, []string{"apple", "Banana", "Zebra", ".cache", ".config"}, entryNames(resp))
}

// The RPC is a thin wrapper, but it is the thing the route table binds — pin
// that it forwards the request and does not swallow the error into a 200.
func TestControlServer_ListDirectory(t *testing.T) {
	root := t.TempDir()
	mkrepo(t, filepath.Join(root, "checkout"))

	cs := &controlServer{}
	var resp ListDirectoryResponse
	require.NoError(t, cs.ListDirectory(ListDirectoryRequest{Path: root}, &resp))
	assert.Equal(t, []string{"checkout"}, entryNames(resp))

	var failed ListDirectoryResponse
	err := cs.ListDirectory(ListDirectoryRequest{Path: filepath.Join(root, "nope")}, &failed)
	require.Error(t, err)
	assert.Empty(t, failed.Entries)
}
