package pathutil

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestResolveForCompare_MissingLeafBehindSymlink pins the property the whole
// helper exists for: two spellings of the same path must compare EQUAL even
// when the final component does not exist. A plain filepath.EvalSymlinks
// returns an error there and leaves callers comparing un-canonicalized strings,
// which is how a path-identity probe silently returns the wrong answer on any
// platform whose working root is a symlink (macOS /var -> /private/var, #2110).
func TestResolveForCompare_MissingLeafBehindSymlink(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(base, "link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}

	// The leaf deliberately does NOT exist — this is the post-deletion state
	// every worktree registration probe runs in.
	viaLink := filepath.Join(linkRoot, "gone")
	viaReal := filepath.Join(realRoot, "gone")

	if _, err := filepath.EvalSymlinks(viaLink); err == nil {
		t.Fatal("sanity: EvalSymlinks should fail on a missing leaf")
	}

	// The contract is path IDENTITY, so assert that — not a literal string. The
	// expectation cannot be spelled as a constant: under a symlinked TMPDIR the
	// test's own `realRoot` still carries the symlinked prefix, so comparing
	// against it would fail for the very reason this helper exists.
	if ResolveForCompare(viaLink) != ResolveForCompare(viaReal) {
		t.Errorf("both spellings of the same path must compare equal: %q vs %q",
			ResolveForCompare(viaLink), ResolveForCompare(viaReal))
	}

	// And it must genuinely resolve rather than pass the input through: the
	// symlinked ancestor is replaced, the missing leaf is preserved.
	got := ResolveForCompare(viaLink)
	resolvedRoot, err := filepath.EvalSymlinks(linkRoot)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(resolvedRoot, "gone"); got != want {
		t.Errorf("ResolveForCompare(%q) = %q, want %q", viaLink, got, want)
	}
	if filepath.Base(got) != "gone" {
		t.Errorf("the missing leaf must be preserved, got %q", got)
	}
}

// TestResolveForCompare_FallsBackToCleaned keeps the documented floor: a path
// with nothing resolvable is still cleaned, so callers are never worse off than
// comparing filepath.Clean output.
func TestResolveForCompare_FallsBackToCleaned(t *testing.T) {
	in := filepath.Join(string(filepath.Separator), "definitely", "not", "here", "..", "here", "x")
	if got, want := ResolveForCompare(in), filepath.Clean(in); got != want {
		t.Errorf("ResolveForCompare(%q) = %q, want cleaned %q", in, got, want)
	}
}

// TestResolveForCompareWith_ReportsALookupThatDidNotHappen pins #7286: the walk
// falls back past a missing component as it always has, and that is an answer;
// falling back past any other failure (EIO from a soft NFS mount, EACCES) still
// returns the same path ResolveForCompare does, but reports the failure, since
// the path past it is a guess.
func TestResolveForCompareWith_ReportsALookupThatDidNotHappen(t *testing.T) {
	path := filepath.Join(string(filepath.Separator), "mnt", "share", "repo")
	for _, tc := range []struct {
		name   string
		fail   error
		answer bool
	}{
		{"absent", &os.PathError{Op: "lstat", Path: path, Err: syscall.ENOENT}, true},
		{"not a directory", &os.PathError{Op: "lstat", Path: path, Err: syscall.ENOTDIR}, true},
		{"an I/O error", &os.PathError{Op: "lstat", Path: path, Err: syscall.EIO}, false},
		{"a stale handle", &os.PathError{Op: "lstat", Path: path, Err: syscall.ESTALE}, false},
		{"permission denied", &os.PathError{Op: "lstat", Path: path, Err: syscall.EACCES}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eval := func(p string) (string, error) {
				if p == path {
					return "", tc.fail
				}
				return p, nil
			}
			got, err := ResolveForCompareWith(path, eval)
			if got != path {
				t.Errorf("the walk's path is unchanged: got %q, want %q", got, path)
			}
			if tc.answer && err != nil {
				t.Errorf("a missing component is an answer, got %v", err)
			}
			if !tc.answer && !errors.Is(err, tc.fail) {
				t.Errorf("a failed lookup is reported: got %v, want %v", err, tc.fail)
			}
		})
	}
}
