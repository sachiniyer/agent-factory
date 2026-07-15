package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for the user-supplied path helpers (ExpandTilde, ResolveUserPath).
// Split out of config_test.go to keep it under the file-length limit (#1145).

// TestExpandTilde pins the #924 tilde helper: a bare "~" and "~/x" expand to
// the home directory (filepath.Abs does not), while absolute paths, relative
// paths, the empty string, and unresolvable "~user" forms pass through
// unchanged. The AGENT_FACTORY_HOME inline expansion in GetConfigDir is built
// on this helper, so the cases below also pin that path's behavior.
func TestExpandTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct {
		in   string
		want string
	}{
		{"~", home},
		{"~/project", filepath.Join(home, "project")},
		{"~/a/b/c", filepath.Join(home, "a", "b", "c")},
		{"/abs/path", "/abs/path"},
		{"relative/path", "relative/path"},
		{"", ""},
		{"~user", "~user"},     // Go cannot resolve "~user"; left untouched.
		{"foo~bar", "foo~bar"}, // a tilde that is not a leading prefix is literal.
	}
	for _, c := range cases {
		if got := ExpandTilde(c.in); got != c.want {
			t.Errorf("ExpandTilde(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestResolveUserPath pins the #1842 helper: user-supplied paths get their
// leading "~" expanded BEFORE absolutization, so a tilde the shell never
// expanded (single-quoted '~/repo', or "$VAR" holding it) resolves to the home
// directory instead of being silently rewritten to "<cwd>/~/repo". Relative
// paths still resolve against the cwd, and the "~user" form the helper cannot
// resolve stays literal rather than being guessed at.
func TestResolveUserPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare tilde", "~", home},
		{"tilde path", "~/repo", filepath.Join(home, "repo")},
		{"nested tilde path", "~/a/b/c", filepath.Join(home, "a", "b", "c")},
		{"absolute path untouched", "/abs/path", "/abs/path"},
		{"relative path resolves against cwd", "repo", filepath.Join(cwd, "repo")},
		{"dot resolves to cwd", ".", cwd},
		{"empty resolves to cwd", "", cwd},
		// Go cannot resolve "~user"; it stays literal and is absolutized as an
		// ordinary relative name rather than being mistaken for a home dir.
		{"tilde-user stays literal", "~user", filepath.Join(cwd, "~user")},
		{"non-leading tilde is literal", "/tmp/foo~bar", "/tmp/foo~bar"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ResolveUserPath(c.in)
			if err != nil {
				t.Fatalf("ResolveUserPath(%q) returned error: %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("ResolveUserPath(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestResolveUserPathNeverCorruptsTilde is the direct #1842 regression: the
// pre-fix code called filepath.Abs on raw input, producing the corrupted
// "<cwd>/~/..." path that surfaced as a confusing "not a valid git repository"
// error. No resolved path may ever retain a literal "~" segment.
func TestResolveUserPathNeverCorruptsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	for _, in := range []string{"~", "~/repo", "~/a/b"} {
		got, err := ResolveUserPath(in)
		if err != nil {
			t.Fatalf("ResolveUserPath(%q) returned error: %v", in, err)
		}
		for _, seg := range strings.Split(got, string(filepath.Separator)) {
			if seg == "~" {
				t.Errorf("ResolveUserPath(%q) = %q; kept a literal %q segment (the #1842 corruption)", in, got, "~")
			}
		}
		if !strings.HasPrefix(got, home) {
			t.Errorf("ResolveUserPath(%q) = %q, want a path under home %q", in, got, home)
		}
	}
}
