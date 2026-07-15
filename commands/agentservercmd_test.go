package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveAgentServerRepoExpandsTilde pins `agent-server --repo '~/repo'`
// against #1842. This entry point failed worse than the others: NewInstance does
// not validate the repo at construct time, so a corrupted "<cwd>/~/repo" still
// bound a listener and printed its addr/token, and only blew up later during
// worktree setup.
func TestResolveAgentServerRepoExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"tilde path", "~/repo", filepath.Join(home, "repo")},
		{"bare tilde", "~", home},
		{"nested tilde path", "~/a/b", filepath.Join(home, "a", "b")},
		{"absolute path untouched", "/srv/repo", "/srv/repo"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveAgentServerRepo(c.in)
			if err != nil {
				t.Fatalf("resolveAgentServerRepo(%q) returned error: %v", c.in, err)
			}
			for _, seg := range strings.Split(got, string(filepath.Separator)) {
				if seg == "~" {
					t.Fatalf("resolveAgentServerRepo(%q) = %q; kept a literal %q segment (the #1842 corruption)", c.in, got, "~")
				}
			}
			if got != c.want {
				t.Errorf("resolveAgentServerRepo(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestResolveAgentServerRepoDefaultsToCwd pins the documented default: an unset
// --repo means the current directory.
func TestResolveAgentServerRepoDefaultsToCwd(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	got, err := resolveAgentServerRepo("")
	if err != nil {
		t.Fatalf("resolveAgentServerRepo(\"\") returned error: %v", err)
	}
	if got != cwd {
		t.Errorf("resolveAgentServerRepo(\"\") = %q, want cwd %q", got, cwd)
	}
}

// TestResolveAgentServerRepoRelative pins that relative paths still resolve
// against the cwd — the tilde fix must not change their meaning.
func TestResolveAgentServerRepoRelative(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	got, err := resolveAgentServerRepo("sub/repo")
	if err != nil {
		t.Fatalf("resolveAgentServerRepo(\"sub/repo\") returned error: %v", err)
	}
	if want := filepath.Join(cwd, "sub", "repo"); got != want {
		t.Errorf("resolveAgentServerRepo(\"sub/repo\") = %q, want %q", got, want)
	}
}
