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
			got := sanitizeBranchName(tt.input)
			if got != tt.expected {
				t.Errorf("sanitizeBranchName(%q) = %q, want %q", tt.input, got, tt.expected)
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
			got := sanitizeBranchName(input)
			if got == "" {
				t.Errorf("sanitizeBranchName(%q) returned empty string, expected fallback name", input)
			}
			if !strings.HasPrefix(got, "session-") {
				t.Errorf("sanitizeBranchName(%q) = %q, expected prefix \"session-\"", input, got)
			}
		})
	}
}

func TestSanitizeBranchName_FallbackIsUnique(t *testing.T) {
	// Each call with an empty-producing input should return a unique fallback.
	a := sanitizeBranchName("")
	b := sanitizeBranchName("")
	if a == b {
		t.Errorf("expected unique fallback names, got %q twice", a)
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
