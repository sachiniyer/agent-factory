package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The #3504 sweep gave the narrating sites one predicate, one classifier, and
// one sentence. These pin the contract those sites depend on: an unanswered
// probe is recognisable across package boundaries, a caller running its OWN git
// probe classifies identically to the resolver, and the honest sentence never
// asserts anything about the path.

// TestRepoProbeUnansweredRecognisesTheResolverError: the predicate must see
// through every layer of wrapping the resolver and its callers add, or each
// call site quietly falls back to the verdict wording.
func TestRepoProbeUnansweredRecognisesTheResolverError(t *testing.T) {
	installFakeGit(t, `#!/bin/sh
kill -9 $$
`)

	_, err := RepoFromPath(t.TempDir())
	require.Error(t, err)
	assert.True(t, RepoProbeUnanswered(err),
		"the predicate must recognise an unanswered probe through RepoFromPath's wrapping")
	assert.True(t, RepoProbeUnanswered(fmt.Errorf("--repo %q: %w", "/x", err)),
		"and through a caller's own wrapping, which is how every #3504 site uses it")
	assert.False(t, RepoProbeUnanswered(errors.New("some unrelated failure")))
	assert.False(t, RepoProbeUnanswered(nil))
}

// TestClassifyGitProbeErrorMatchesTheResolver: a caller running its own probe
// (session/git's repo check, doctor's setup probe) must get the same verdict the
// resolver would give for the same outcome. Sharing the function is the point —
// the enumeration this replaced had a measured gap.
func TestClassifyGitProbeErrorMatchesTheResolver(t *testing.T) {
	dir := t.TempDir()
	killer := filepath.Join(dir, "killer")
	require.NoError(t, os.WriteFile(killer, []byte("#!/bin/sh\nkill -9 $$\n"), 0o755))
	refuser := filepath.Join(dir, "refuser")
	require.NoError(t, os.WriteFile(refuser, []byte("#!/bin/sh\nexit 128\n"), 0o755))
	unstartable := filepath.Join(dir, "unstartable")
	require.NoError(t, os.WriteFile(unstartable, []byte("not a program\n"), 0o755))

	killed := ClassifyGitProbeError(context.Background(), exec.Command(killer).Run())
	assert.True(t, RepoProbeUnanswered(killed), "a killed probe answered nothing")

	refused := ClassifyGitProbeError(context.Background(), exec.Command(refuser).Run())
	require.Error(t, refused)
	assert.False(t, RepoProbeUnanswered(refused), "a clean non-zero exit IS an answer")

	unstarted := ClassifyGitProbeError(context.Background(), exec.Command(unstartable).Run())
	assert.True(t, RepoProbeUnanswered(unstarted), "a probe that could not be started answered nothing")

	assert.NoError(t, ClassifyGitProbeError(context.Background(), nil),
		"a successful probe must stay successful")
}

// TestRepoProbeUnansweredClaimAssertsNothingAboutThePath is a copy test, and it
// guards the whole point of the sweep: the sentence names the subprocess
// outcome and must never contain the verdict it replaced.
func TestRepoProbeUnansweredClaimAssertsNothingAboutThePath(t *testing.T) {
	claim := RepoProbeUnansweredClaim("--repo", "/home/u/project")

	assert.Contains(t, claim, "/home/u/project", "the claim must name what could not be checked")
	assert.Contains(t, claim, "--repo", "and the subject the caller was resolving")
	assert.Contains(t, claim, "git never answered", "it must name the subprocess outcome")
	assert.Contains(t, claim, "unknown", "and say the question is open")
	assert.NotContains(t, claim, "is not a git repository",
		"an unanswered probe must never carry the verdict this sweep exists to remove")
}
