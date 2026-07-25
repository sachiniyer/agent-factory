package git

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
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
	// A Unicode-only title under a non-empty prefix must not collapse to the
	// bare sanitized prefix; it gets a random suffix so distinct such titles
	// stay distinct (#1640).
	jp := BranchForTitle("af-", "日本語")
	if !strings.HasPrefix(jp, "af-") || jp == "af" {
		t.Errorf("BranchForTitle(\"af-\", \"日本語\") = %q, want an \"af-\"-prefixed name with a random suffix", jp)
	}
	if ar := BranchForTitle("af-", "مرحبا"); jp == ar {
		t.Errorf("distinct unicode-only titles produced the same branch %q", jp)
	}
}

// TestBranchAndWorktreeNamesBoundLongTitles is the #2528 regression: a long title
// (creatable via the CLI/API, no TUI 32-char cap) must not derive a branch ref or
// worktree path component that overruns NAME_MAX and fails with "File name too
// long". Against master these are 500+ chars.
func TestBranchAndWorktreeNamesBoundLongTitles(t *testing.T) {
	long := strings.Repeat("a", 500)
	// The invariant: every derived filesystem/ref component stays under Linux
	// NAME_MAX (255), with margin for git's transient "<ref>.lock". Asserting the
	// real limit, not the impl bound, so this fails on master (500 chars).
	const safeMax = 255 - len(".lock")

	branch := BranchForTitle("af-jdoe/", long)
	for _, part := range strings.Split(branch, "/") {
		if len(part) > safeMax {
			t.Errorf("branch component %q is %d bytes, over the %d NAME_MAX budget", part, len(part), safeMax)
		}
	}

	// The default local placement is sibling mode (config.DefaultConfig), where the
	// worktree DIRECTORY name is the JOINED "<repoName>-<segment>" component — the
	// real thing that must fit NAME_MAX. A realistic repo name eats into the budget,
	// so capping the segment alone (as the first #2528 cut did) still overruns:
	// 60-byte repo name + a 200-byte segment cap = a 261-byte component. Assert the
	// joined component at the join site, not the segment proxy (#2528 review).
	repoBase := strings.Repeat("r", 60) // a plausible long repo directory name
	repoRoot := filepath.Join(t.TempDir(), repoBase)
	worktreeDir := t.TempDir()
	cfg := &config.Config{WorktreeRoot: config.WorktreeRootSibling}
	p, err := resolveWorktreePlacement(cfg, repoRoot, worktreeDir, long, branch)
	if err != nil {
		t.Fatalf("resolveWorktreePlacement: %v", err)
	}
	comp := filepath.Base(p)
	if len(comp) > 255 {
		t.Errorf("worktree dir component is %d bytes, over NAME_MAX 255", len(comp))
	}
	if !strings.HasPrefix(comp, repoBase+"-") {
		t.Errorf("worktree dir component lost its %q- repo-name prefix", repoBase)
	}

	// Short titles are unaffected.
	if got := BranchForTitle("af-", "MyApp"); got != "af-myapp" {
		t.Errorf("short title branch changed: %q", got)
	}
}

// TestSanitizeBranchName_TruncationCannotReexposeDotLock is the #2528 P3-a
// regression: truncating a long component must run BEFORE the ".lock" cleanup, so
// a cut that lands on — or newly exposes — a ".lock" suffix cannot leave it on a
// NON-final component. git reserves ".lock" on every slash-separated component,
// and trimBranchNameEdges only repairs the final one.
func TestSanitizeBranchName_TruncationCannotReexposeDotLock(t *testing.T) {
	// Component 0 is 195 'a's + ".lock" + filler, so a 200-byte cut lands exactly
	// after the ".lock"; the trailing "/tail" keeps it a NON-final component. Under
	// the old strip-then-truncate order the cut re-exposed ".lock" and nothing
	// removed it.
	title := strings.Repeat("a", 195) + ".lock" + strings.Repeat("b", 50) + "/tail"
	branch := SanitizeBranchName(title)

	for _, part := range strings.Split(branch, "/") {
		if strings.HasSuffix(part, ".lock") {
			t.Errorf("branch component %q ends in .lock — truncation re-exposed it", part)
		}
	}
	// Ground truth: git itself must accept the ref.
	if out, err := exec.Command("git", "check-ref-format", "refs/heads/"+branch).CombinedOutput(); err != nil {
		t.Errorf("git check-ref-format rejected %q: %v\n%s", branch, err, out)
	}
}

// TestBoundTitleForDisambiguation_KeepsSuffixesInjective is the #2528 P3-b
// mechanism lock. The daemon's uniquifying walks append "-N" / " (archived N)" to
// a base title and judge availability on the DERIVED branch. For a long base,
// truncation makes every suffixed rung derive the SAME branch as the base — a
// non-convergent walk. Bounding the base keeps distinct suffixes distinct.
func TestBoundTitleForDisambiguation_KeepsSuffixesInjective(t *testing.T) {
	const prefix = "jdoe/"
	long := strings.Repeat("a", 300)

	// Precondition (the bug): a RAW long base+suffix derives the same truncated
	// branch as the base, so the reserve is load-bearing.
	if BranchForTitle(prefix, long+"-2") != BranchForTitle(prefix, long) {
		t.Fatal("precondition changed: a raw long base+suffix no longer collides")
	}

	bounded := BoundTitleForDisambiguation(long)
	base := BranchForTitle(prefix, long)
	b2 := BranchForTitle(prefix, bounded+"-2")
	b3 := BranchForTitle(prefix, bounded+"-3")
	if b2 == base || b3 == base || b2 == b3 {
		t.Errorf("bounded suffixes must derive distinct branches: base=%q -2=%q -3=%q", base, b2, b3)
	}

	// Short titles pass through untouched.
	if got := BoundTitleForDisambiguation("myapp"); got != "myapp" {
		t.Errorf("short base was altered: %q", got)
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
		// A non-empty prefix keeps "<prefix><unsafe-only title>" non-empty, so
		// SanitizeBranchName's random fallback never fires and every unsafe-only
		// title would otherwise collapse to the sanitized prefix. BranchForTitle
		// appends a random suffix in that case so distinct Unicode-only titles
		// still get distinct branches (#1640).
		{name: "distinct unicode-only titles do not collide under a prefix (#1640)", a: "日本語", b: "مرحبا", prefix: "af-", want: false},
		{name: "identical unicode-only titles still collide under a prefix", a: "日本語", b: "日本語", prefix: "af-", want: true},
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
