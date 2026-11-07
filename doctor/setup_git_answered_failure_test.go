package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// The #3794 twin: the daemon's equivalent site (daemon/rootagent_repoprobe.go,
// commit 71e09ae4) was fixed for this class of failure without updating the
// doctor. An ANSWERED git failure — dubious ownership, an unreadable .git, an
// invalid .git file — is not a verdict about the path, so checkGit must not
// report it as "not inside a git repository" with "git init" advice for a
// checkout that already exists.
//
// config.ErrNotGitRepository's own doc (config/repo.go) says only the specific
// "not a git repository (or any of the parent directories...)" diagnostic with
// no .git metadata present may be called "not inside a git repository"; other
// answered failures "must remain visible rather than being mistaken for
// 'outside git.'" The resolver's classification (resolveRepoRootsContext)
// enforces that: it requires both the stderr match AND !gitMetadataMayExist,
// so a present .git plus a non-matching stderr never returns ErrNotGitRepository.
// checkGit contradicted that contract by treating every answered failure as
// "not a repository", and this test pins the fix.

// answeredFailureRepo returns a path git ANSWERS about with a completed
// non-zero exit that is NOT "not a repository": a directory whose .git is a
// gitfile pointing nowhere.
//
// Mirrors the daemon's answeredFailureRepo
// (daemon/rootagent_answered_failure_test.go): the portable member of the class,
// the way breakPersonalRootAgentToml is for #3241. chmod 000 needs a non-root
// runner and means nothing on Windows; a safe.directory rejection needs a
// foreign owner. A .git file pointing nowhere runs everywhere. The returned
// restore puts the real repository back.
func answeredFailureRepo(t *testing.T, repoPath string) (restore func()) {
	t.Helper()
	gitDir := filepath.Join(repoPath, ".git")
	aside := filepath.Join(repoPath, ".git-aside")
	if err := os.Rename(gitDir, aside); err != nil {
		t.Fatalf("set .git aside: %v", err)
	}
	if err := os.WriteFile(gitDir, []byte("gitdir: /nonexistent-target\n"), 0o644); err != nil {
		t.Fatalf("write invalid gitfile: %v", err)
	}
	// Prove the fixture really produces the state under test, rather than
	// assuming it: git must ANSWER, and its answer must not be the
	// not-a-repository verdict.
	_, err := config.RepoFromPath(repoPath)
	if err == nil {
		t.Fatal("fixture: the broken checkout still resolves")
	}
	if config.RepoProbeUnanswered(err) {
		t.Fatalf("fixture: this failure must be ANSWERED, got an unanswered probe: %v", err)
	}
	if errors.Is(err, config.ErrNotGitRepository) {
		t.Fatalf("fixture: this failure must NOT be the not-a-repository verdict, got: %v", err)
	}
	return func() {
		if err := os.Remove(gitDir); err != nil {
			t.Fatalf("remove invalid gitfile: %v", err)
		}
		if err := os.Rename(aside, gitDir); err != nil {
			t.Fatalf("restore .git: %v", err)
		}
	}
}

// TestAnsweredGitFailureIsNotANotRepositoryVerdict is the classification, at
// the function that makes it: a real repository git refuses to serve must not
// be reported as "not inside a git repository" with "git init" advice.
//
// Before the fix, this test's fixture produced the word-for-word same finding
// as a genuine non-repo directory — the doctor could not distinguish "git
// answered: this path is outside git" from "git answered: this repository is
// unusable as-is" — and checkGitIdentity was silently skipped because
// checkGitIdentity runs only on a successful probe.
func TestAnsweredGitFailureIsNotANotRepositoryVerdict(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	repo := setupDoctorRepo(t)
	chdir(t, repo)
	restore := answeredFailureRepo(t, repo)
	defer restore()

	report := &Report{}
	checkGit(report)
	findings := findByCheck(report, "git-repo")
	require.NotEmpty(t, findings)
	f := findings[0]
	require.NotContains(t, f.Detail, "is not inside a git repository",
		"an answered failure that is not a not-a-repository verdict must not be reported as one (#3794 twin)")
	require.NotContains(t, f.Remediation, "git init",
		"a checkout that already exists must not be told to `git init` (#3794 twin)")
	require.Contains(t, f.Remediation, "af doctor --setup",
		"the fix line should say how to re-check")
	require.Contains(t, f.Detail, repo,
		"the finding should surface the underlying git error for the path")
}

// TestGenuineNonRepoStillGetsNotRepositoryVerdict confirms the fix did not
// soften the true non-repo case: a plain directory with no .git must still get
// the "not inside a git repository" finding with "git init" advice. This is the
// regression lock — the fix is a distinction, not a softening.
func TestGenuineNonRepoStillGetsNotRepositoryVerdict(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	chdir(t, t.TempDir()) // a directory that is not a repository

	report := &Report{}
	checkGit(report)
	findings := findByCheck(report, "git-repo")
	require.NotEmpty(t, findings)
	require.Contains(t, findings[0].Detail, "is not inside a git repository")
	require.Contains(t, findings[0].Remediation, "git init")
	require.Contains(t, findings[0].Remediation, "af doctor --setup")
}

// TestAnsweredGitFailureSurfacesGitError verifies the "answered but not a
// verdict" finding names the underlying git error and points the user at the
// concrete remedy (e.g. safe.directory for an ownership refusal) rather than
// at `git init`.
func TestAnsweredGitFailureSurfacesGitError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	repo := setupDoctorRepo(t)
	chdir(t, repo)
	restore := answeredFailureRepo(t, repo)
	defer restore()

	report := &Report{}
	checkGit(report)
	findings := findByCheck(report, "git-repo")
	require.NotEmpty(t, findings)
	f := findings[0]
	require.Contains(t, f.Detail, "git could not resolve a repository root",
		"the finding should say git could not resolve, not that the path is not a repository")
	require.Contains(t, f.Remediation, "safe.directory",
		"the remediation should name the concrete remedy for an ownership refusal")
	require.NotContains(t, f.Remediation, "git init",
		"the remediation must not suggest git init for a checkout that already exists")
}

// TestAnsweredGitFailureRestoresRepo confirms the fixture's restore puts the
// real repository back, so a healthy checkGit run after restore reports a PASS
// and runs checkGitIdentity. This guards against a fixture that silently
// corrupts the repo it was supposed to restore.
func TestAnsweredGitFailureRestoresRepo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	repo := setupDoctorRepo(t)
	chdir(t, repo)
	restore := answeredFailureRepo(t, repo)
	restore()

	report := &Report{}
	checkGit(report)
	findings := findByCheck(report, "git-repo")
	require.Empty(t, findings, "after restore, the repo should resolve and checkGit should not raise a git-repo finding")
	require.True(t, okContains(report, "git repo"), "the restored repo should produce a git repo PASS row")
	require.True(t, okContains(report, "git identity"), "checkGitIdentity should run after a successful probe")
}
