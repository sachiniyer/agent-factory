package git

import (
	"strings"
	"testing"
)

func TestSanitizeBranchName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple lowercase string",
			input:    "feature",
			expected: "feature",
		},
		{
			name:     "string with spaces",
			input:    "new feature branch",
			expected: "new-feature-branch",
		},
		{
			name:     "mixed case string",
			input:    "FeAtUrE BrAnCh",
			expected: "feature-branch",
		},
		{
			name:     "string with special characters",
			input:    "feature!@#$%^&*()",
			expected: "feature",
		},
		{
			name:     "string with allowed special characters",
			input:    "feature/sub_branch.v1",
			expected: "feature/sub_branch.v1",
		},
		{
			name:     "string with multiple dashes",
			input:    "feature---branch",
			expected: "feature-branch",
		},
		{
			name:     "string with leading and trailing dashes",
			input:    "-feature-branch-",
			expected: "feature-branch",
		},
		{
			name:     "string with leading and trailing slashes",
			input:    "/feature/branch/",
			expected: "feature/branch",
		},
		{
			name:     "complex mixed case with special chars",
			input:    "USER/Feature Branch!@#$%^&*()/v1.0",
			expected: "user/feature-branch/v1.0",
		},
		{
			name:     "leading dot in path component",
			input:    "john/.env",
			expected: "john/env",
		},
		{
			name:     "name ending with .lock",
			input:    "john/config.lock",
			expected: "john/config",
		},
		{
			name:     "multiple .lock suffixes",
			input:    "john/config.lock.lock",
			expected: "john/config",
		},
		{
			name:     ".lock in intermediate path segment",
			input:    "foo.lock/bar",
			expected: "foo/bar",
		},
		{
			name:     ".locked is not stripped (internal .lock preserved)",
			input:    "foo.locked/bar",
			expected: "foo.locked/bar",
		},
		{
			name:     ".lock in multiple segments",
			input:    "foo.lock/bar.lock/baz",
			expected: "foo/bar/baz",
		},
		{
			name:     "double dots in name",
			input:    "feature..branch",
			expected: "feature-branch",
		},
		{
			name:     "trailing dots",
			input:    "feature.branch...",
			expected: "feature.branch",
		},
		{
			name:     "final trim cannot reveal trailing dot",
			input:    "myteam/feat.-fix-bug.-.",
			expected: "myteam/feat.-fix-bug",
		},
		{
			name:     "final trim cannot reveal .lock suffix",
			input:    ".-test.lock.-.",
			expected: "test",
		},
		{
			name:     "path component is only dots",
			input:    "john/.../file",
			expected: "john/file",
		},
		{
			name:     "multiple leading dots",
			input:    "john/...hidden",
			expected: "john/hidden",
		},
		{
			name:     "standalone dotfile name",
			input:    ".env",
			expected: "env",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeBranchName(tt.input)
			if got != tt.expected {
				t.Errorf("SanitizeBranchName(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestSanitizeBranchName_FallbackOnEmpty(t *testing.T) {
	// Inputs that would sanitize to an empty string should get a fallback name.
	inputs := []string{
		"",
		"!@#$%^&*()",
		"---",
		"///",
		"-/-/-/",
		"...",
		"/.../",
	}
	for _, input := range inputs {
		t.Run("input="+input, func(t *testing.T) {
			got := SanitizeBranchName(input)
			if got == "" {
				t.Errorf("SanitizeBranchName(%q) returned empty string, expected fallback name", input)
			}
			if !strings.HasPrefix(got, "session-") {
				t.Errorf("SanitizeBranchName(%q) = %q, expected prefix \"session-\"", input, got)
			}
		})
	}
}

func TestSanitizeBranchName_FallbackIsUnique(t *testing.T) {
	// Each call with an empty-producing input should return a unique fallback.
	a := SanitizeBranchName("")
	b := SanitizeBranchName("")
	if a == b {
		t.Errorf("expected unique fallback names, got %q twice", a)
	}
}

func TestBranchForTitle(t *testing.T) {
	if got := BranchForTitle("af-", "MyApp"); got != "af-myapp" {
		t.Errorf("BranchForTitle(\"af-\", \"MyApp\") = %q, want %q", got, "af-myapp")
	}
	if got := BranchForTitle("af-", "A B"); got != "af-a-b" {
		t.Errorf("BranchForTitle(\"af-\", \"A B\") = %q, want %q", got, "af-a-b")
	}
	if got := BranchForTitle("", "feature"); got != "feature" {
		t.Errorf("BranchForTitle(\"\", \"feature\") = %q, want %q", got, "feature")
	}
}

// TestTitlesCollide pins the shared collision rule used by both the daemon's
// authoritative create-time check and the TUI's naming pre-check (#936). The
// rule is: case-insensitive equality OR sanitize-to-the-same-branch.
func TestTitlesCollide(t *testing.T) {
	tests := []struct {
		name   string
		a      string
		b      string
		prefix string
		want   bool
	}{
		{name: "exact match collides", a: "myapp", b: "myapp", prefix: "af-", want: true},
		{name: "case variant collides (#605)", a: "MyApp", b: "myapp", prefix: "af-", want: true},
		{name: "uppercase vs mixed case collides", a: "MYAPP", b: "MyApp", prefix: "af-", want: true},
		{name: "space vs dash sanitize collision (#741)", a: "a b", b: "a-b", prefix: "af-", want: true},
		{name: "space-and-case sanitize collision", a: "My App", b: "my-app", prefix: "af-", want: true},
		{name: "distinct names do not collide", a: "alpha", b: "beta", prefix: "af-", want: false},
		{name: "substring is not a collision", a: "app", b: "myapp", prefix: "af-", want: false},
		// With an empty prefix, unsafe-only titles sanitize to a random
		// "session-<hex>" fallback that never compares equal across calls. The
		// EqualFold guard is what still makes two identical such titles collide,
		// and keeps two different ones from colliding by accident.
		{name: "identical unsafe-only titles collide via EqualFold guard", a: "!!!", b: "!!!", prefix: "", want: true},
		{name: "distinct unsafe-only titles do not collide via random fallback", a: "!!!", b: "???", prefix: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TitlesCollide(tt.a, tt.b, tt.prefix); got != tt.want {
				t.Errorf("TitlesCollide(%q, %q, %q) = %v, want %v", tt.a, tt.b, tt.prefix, got, tt.want)
			}
			// Collision is symmetric.
			if got := TitlesCollide(tt.b, tt.a, tt.prefix); got != tt.want {
				t.Errorf("TitlesCollide(%q, %q, %q) = %v, want %v (symmetry)", tt.b, tt.a, tt.prefix, got, tt.want)
			}
		})
	}
}

// TestEnsureRepo_DistinguishesMissingGit verifies the #737 fix: when git is not
// on PATH, EnsureRepo reports a "git is not installed" error rather than the
// misleading "must be run from within a git repository" message.
func TestEnsureRepo_DistinguishesMissingGit(t *testing.T) {
	// Point PATH at an empty directory so exec.LookPath("git") fails.
	emptyDir := t.TempDir()
	t.Setenv("PATH", emptyDir)

	if IsGitInstalled() {
		t.Fatal("expected IsGitInstalled() to be false with git absent from PATH")
	}

	err := EnsureRepo(emptyDir)
	if err == nil {
		t.Fatal("expected EnsureRepo to return an error when git is not installed")
	}
	msg := err.Error()
	if !strings.Contains(msg, "git is not installed") {
		t.Errorf("expected missing-git error, got %q", msg)
	}
	if strings.Contains(msg, "must be run from within a git repository") {
		t.Errorf("missing-git case must not return the non-repo message, got %q", msg)
	}
}

// TestEnsureRepo_NonRepoWithGitInstalled verifies that when git is installed but
// the path is not inside a repository, EnsureRepo returns the repo-context
// message rather than the missing-git message.
func TestEnsureRepo_NonRepoWithGitInstalled(t *testing.T) {
	if !IsGitInstalled() {
		t.Skip("git binary not available in test environment")
	}
	// A bare temp dir under the OS temp root is not inside a git repository.
	nonRepo := t.TempDir()
	// Guard against the rare case where the temp root itself is tracked.
	if IsGitRepo(nonRepo) {
		t.Skip("temp dir unexpectedly inside a git repository")
	}

	err := EnsureRepo(nonRepo)
	if err == nil {
		t.Fatal("expected EnsureRepo to return an error for a non-repo path")
	}
	if got := err.Error(); !strings.Contains(got, "must be run from within a git repository") {
		t.Errorf("expected non-repo error, got %q", got)
	}
}
