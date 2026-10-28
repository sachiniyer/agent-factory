package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #3504: the admission paths are the worst place to fabricate this verdict —
// they tell a user, in the moment, that the path they just typed is bad, when a
// retry would have worked. These drive the real flag-resolution entry point
// rather than the classifier, so the wiring is what is pinned.

// installUnanswerableGit puts a git shim first on PATH that dies on a signal
// before answering anything. exec reports a signalled exit as a negative exit
// code, which is the "no answer came back" shape.
func installUnanswerableGit(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "git"), []byte("#!/bin/sh\nkill -9 $$\n"), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestRepoFromFlagDoesNotCallAnUncheckablePathInvalid: --repo names a real
// directory and git never answers. The error must not tell the user their path
// is not a valid repository.
func TestRepoFromFlagDoesNotCallAnUncheckablePathInvalid(t *testing.T) {
	path := t.TempDir()
	prev := repoFlag
	t.Cleanup(func() { repoFlag = prev })
	repoFlag = path
	installUnanswerableGit(t)

	_, err := repoFromFlag()
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "is not a valid git repository",
		"an unanswered probe is not a verdict on the user's --repo path (#3504)")
	assert.Contains(t, err.Error(), "git never answered",
		"the error must name the subprocess outcome that actually happened")
	assert.Contains(t, err.Error(), path, "and still name the path it could not check")
	assert.Contains(t, strings.ToLower(err.Error()), "retry",
		"a transient failure should tell the user it is worth retrying")
}

// TestRepoFromFlagStillRejectsARealNonRepository is the over-correction guard:
// when git answers, the existing verdict and its wording must survive — the
// pinned copy other tests assert on.
func TestRepoFromFlagStillRejectsARealNonRepository(t *testing.T) {
	path := t.TempDir()
	prev := repoFlag
	t.Cleanup(func() { repoFlag = prev })
	repoFlag = path

	_, err := repoFromFlag()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not a valid git repository",
		"git answered, and the answer is no: that claim must still be made")
	assert.NotContains(t, err.Error(), "git never answered")
}
