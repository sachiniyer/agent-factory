package git

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The bare-layout probe (#5532, bounding refactor #7118) is the existence gate
// of `af reset`'s sole branch-deletion path: when a recorded repo root has no
// `.git` child, localBranchExists recognizes a bare metadata layout (HEAD,
// config, objects/, refs/) via bareMetadataLayout so a bare-clone origin falls
// through to the `git show-ref` probe instead of being declared a de-git'd repo.
//
// The probe's #3243 fail-closed contract — the comment in branch_prune.go
// reads "A stalled layout probe is an error, never 'not bare': read as 'not
// bare', it would make a missing .git determinate absence" — must hold for
// EVERY failed observation, not only context.DeadlineExceeded. Before the fix,
// bareMetadataLayout folded every non-deadline stat error (EIO, ESTALE, EACCES,
// ELOOP, …) into (false, nil) "not bare", so localBranchExists took its
// `if !isBare { return false, false, nil }` arm and reported DETERMINATE
// ABSENCE of a branch that may verifiably exist. DeleteLocalBranch then
// returned a clean idempotent no-op (false, nil), commands/reset.go did NOT
// retain the branch in unverifiedBranches, and the session instance records —
// the only durable pointer naming the branch — were deleted, permanently
// orphaning a branch that verifiably exists.

// TestBareMetadataLayout_DistinguishesENOENTFromFailedObservation pins the
// fail-closed contract at the helper itself: a genuinely missing entry is
// determinate "not bare", a structural mismatch is determinate "not bare",
// and EVERY other failed observation (EIO, EACCES, ELOOP, a deadline, …) is a
// failed probe returned as an error so callers that read "not bare" as
// determinate absence cannot fabricate it from an observation that proved
// nothing.
func TestBareMetadataLayout_DistinguishesENOENTFromFailedObservation(t *testing.T) {
	dir := t.TempDir()
	// A real bare layout: build the four entries the probe inspects.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config"), []byte("[core]\n"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "objects"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "refs"), 0o755))

	t.Run("complete layout is bare", func(t *testing.T) {
		bare, err := bareMetadataLayout(dir, os.Stat)
		require.NoError(t, err)
		assert.True(t, bare, "a complete bare layout must be recognized as bare")
	})

	t.Run("ENOENT on an entry is determinate not-bare", func(t *testing.T) {
		// Remove HEAD: a bare repo always has HEAD, so its genuine absence is
		// the structural signal of a de-git'd root, never a failed observation.
		head := filepath.Join(dir, "HEAD")
		require.NoError(t, os.Remove(head))
		t.Cleanup(func() {
			require.NoError(t, os.WriteFile(head, []byte("ref: refs/heads/main\n"), 0o644))
		})
		bare, err := bareMetadataLayout(dir, os.Stat)
		require.NoError(t, err, "a genuinely missing entry is determinate 'not bare', not a failed probe")
		assert.False(t, bare)
	})

	t.Run("structural mismatch is determinate not-bare", func(t *testing.T) {
		// Replace objects/ (a directory) with a file: a real bare repo's
		// objects is a directory, so a file there is a structural mismatch
		// that genuinely is not a bare layout.
		objects := filepath.Join(dir, "objects")
		require.NoError(t, os.RemoveAll(objects))
		require.NoError(t, os.WriteFile(objects, []byte("not a directory\n"), 0o644))
		t.Cleanup(func() {
			require.NoError(t, os.Remove(objects))
			require.NoError(t, os.Mkdir(objects, 0o755))
		})
		bare, err := bareMetadataLayout(dir, os.Stat)
		require.NoError(t, err, "a structural mismatch is determinate 'not bare'")
		assert.False(t, bare)
	})

	// Every non-ENOENT observation must surface as a failed probe (error),
	// never (false, nil). The mechanism is errno-agnostic; ELOOP, EACCES and
	// a deadline stand in for the EIO/ESTALE classes on flaky NFS/FUSE mounts
	// this code's own threat model (bounded_fs.go) is built around.
	statErrCases := []struct {
		name string
		err  error
	}{
		{"ELOOP", syscall.ELOOP},
		{"EACCES", syscall.EACCES},
		{"EIO", syscall.EIO},
		{"deadline", context.DeadlineExceeded},
	}
	for _, entry := range []string{"HEAD", "config", "objects", "refs"} {
		entry := entry
		for _, tc := range statErrCases {
			tc := tc
			t.Run(tc.name+" on "+entry, func(t *testing.T) {
				failing := func(p string) (os.FileInfo, error) {
					if p == filepath.Join(dir, entry) {
						return nil, tc.err
					}
					return os.Stat(p)
				}
				bare, err := bareMetadataLayout(dir, failing)
				require.Error(t, err,
					"a non-ENOENT stat error (%v) on %s must surface as a failed probe, never (false, nil)",
					tc.err, entry)
				assert.False(t, bare)
				assert.Contains(t, err.Error(), "stat ",
					"the error must name the failed observation")
			})
		}
	}
}

// TestBareMetadataLayout_BareMetadataDirectoryIgnoresErrorForResolveRepoMetadataRoot
// pins the inherited-caller invariant the fix's doc comment promises: the
// resolveRepoMetadataRoot caller goes through bareMetadataDirectory, which
// discards the error and reports only the bool. A non-ENOENT stat inside the
// probe still reads as "not bare" there, so resolveRepoMetadataRoot — which
// already reports the outer `.git` stat as an inspection error in that branch —
// is unaffected by the fail-closed tightening.
func TestBareMetadataLayout_BareMetadataDirectoryIgnoresErrorForResolveRepoMetadataRoot(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config"), []byte("[core]\n"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "objects"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "refs"), 0o755))

	// Inject an ELOOP on HEAD through the package's test seam for BoundedStat.
	// bareMetadataDirectory calls bareMetadataLayout(repoPath, os.Stat) — the
	// seam is NOT used there, so drive the failure through a real self-referential
	// symlink that os.Stat follows into ELOOP.
	head := filepath.Join(dir, "HEAD")
	require.NoError(t, os.Remove(head))
	require.NoError(t, os.Symlink("HEAD", head)) // self-referential → os.Stat reports ELOOP
	t.Cleanup(func() {
		require.NoError(t, os.Remove(head))
		require.NoError(t, os.WriteFile(head, []byte("ref: refs/heads/main\n"), 0o644))
	})

	// bareMetadataDirectory must keep reporting "not bare" (discarding the
	// error) so resolveRepoMetadataRoot's behavior is unchanged by the fix.
	assert.False(t, bareMetadataDirectory(dir),
		"bareMetadataDirectory keeps its inherited meaning for resolveRepoMetadataRoot: a failed observation reads as 'not bare' there")
}

// TestLocalBranchExists_BareLayoutStatErrorIsFailedProbeNotAbsence is THE
// REGRESSION: a transient, non-deadline filesystem error inside the bare-layout
// probe of a LIVE bare origin must surface as a failed probe (error, UNKNOWN),
// never as determinate absence (false, nil). The mechanism is errno-agnostic:
// ELOOP stands in for any prompt non-ENOENT failure of the HEAD/config/objects/
// refs stats, including the EIO/ESTALE classes on flaky NFS/FUSE mounts.
func TestLocalBranchExists_BareLayoutStatErrorIsFailedProbeNotAbsence(t *testing.T) {
	const branch = "af-bare-eloop"
	bare := bareCloneRepo(t, branch)
	// Sanity: the bare origin has no .git child, and the branch verifiably
	// exists before the fault is installed.
	_, statErr := os.Stat(filepath.Join(bare, ".git"))
	require.ErrorIs(t, statErr, os.ErrNotExist,
		"sanity: a bare clone has no .git child")
	require.True(t, refExists(t, bare, branch),
		"sanity: the branch verifiably exists in the bare origin before the fault")

	// Capture a real HEAD so it can be restored after the fault; git show-ref
	// below confirms the branch survives only against a healthy HEAD.
	head := filepath.Join(bare, "HEAD")
	originalHead, err := os.ReadFile(head)
	require.NoError(t, err)

	// Install a self-referential symlink at HEAD: BoundedStat follows symlinks
	// (by design — a root may be a symlink, as macOS /tmp is), so this produces
	// a real ELOOP, asserted to be non-ENOENT and non-deadline. This is the
	// exact repro from the bug report.
	require.NoError(t, os.Remove(head))
	require.NoError(t, os.Symlink("HEAD", head))
	t.Cleanup(func() {
		require.NoError(t, os.Remove(head))
		require.NoError(t, os.WriteFile(head, originalHead, 0o644))
	})

	// THE BUG: before the fix, LocalBranchExists collapsed the ELOOP into
	// determinate absence and returned (false, nil).
	exists, err := LocalBranchExists(bare, branch)
	require.Error(t, err,
		"a non-deadline stat error inside the bare-layout probe must surface as UNKNOWN, never (false, nil)")
	assert.False(t, exists)
	assert.NotContains(t, err.Error(), "still running after an earlier deadline",
		"a non-deadline errno is not a deadline-stall message")

	// DOWNSTREAM CONSEQUENCE: DeleteLocalBranch must NOT return the clean
	// idempotent no-op (false, nil) it returned before the fix — that no-op
	// is what made commands/reset.go delete the session instance records of a
	// branch that verifiably exists. It must surface the error so the caller
	// retains the record in unverifiedBranches (#3243).
	deleted, delErr := DeleteLocalBranch(bare, branch)
	require.Error(t, delErr,
		"DeleteLocalBranch must not fabricate a clean no-op from a failed bare-layout probe")
	assert.False(t, deleted, "no branch may be reported deleted from a failed probe")

	// The loose ref file is still present: a failed probe is neither a
	// deletion nor an absence confirmation.
	loose := filepath.Join(bare, "refs", "heads", branch)
	_, looseStatErr := os.Stat(loose)
	assert.NoError(t, looseStatErr,
		"the loose ref file must still be present after a failed probe — the branch was never deleted")

	// RESTORE HEAD and confirm the branch verifiably survives via git show-ref,
	// proving the failed probe claimed nothing about whether the branch exists.
	require.NoError(t, os.Remove(head))
	require.NoError(t, os.WriteFile(head, originalHead, 0o644))
	assert.True(t, refExists(t, bare, branch),
		"the branch must verifiably still exist after restoring HEAD — a failed probe must not have touched it")

	// CONTROL: against the same bare origin with a healthy HEAD, the
	// bare-aware fall-through correctly reports (true, nil). This proves the
	// setup is valid and the ELOOP error was the ONLY thing that flipped the
	// verdict to UNKNOWN — not a setup defect.
	exists, err = LocalBranchExists(bare, branch)
	require.NoError(t, err, "control: a healthy bare origin's existing branch is (true, nil)")
	assert.True(t, exists)
}

// TestLocalBranchExists_BareLayoutStatErrorEachEntry covers every entry the
// bare-layout probe inspects (HEAD, config, objects, refs), not just HEAD: a
// transient errno on ANY one of them must surface as a failed probe, never as
// determinate absence, because the probe's contract is "the layout is bare
// only if every entry is observed and matches".
func TestLocalBranchExists_BareLayoutStatErrorEachEntry(t *testing.T) {
	const branch = "af-bare-entries"
	for _, entry := range []struct {
		name string
	}{
		{"HEAD"},
		{"config"},
		{"objects"},
		{"refs"},
	} {
		entry := entry
		t.Run(entry.name, func(t *testing.T) {
			bare := bareCloneRepo(t, branch)
			require.True(t, refExists(t, bare, branch), "sanity: branch exists before the fault")

			target := filepath.Join(bare, entry.name)
			// Rename the real entry aside and install a self-referential
			// symlink in its place. Rename-aside (not RemoveAll) is essential
			// for the directory entries (objects, refs): RemoveAll would
			// destroy the loose ref under refs/heads/<branch> and the object
			// store, defeating the "branch verifiably survives" check after
			// restore. For a file entry (HEAD, config) rename-aside is equally
			// correct and keeps the test uniform. The symlink yields ELOOP
			// from os.Stat, which follows it (as BoundedStat does by design).
			aside := filepath.Join(t.TempDir(), "set-aside")
			require.NoError(t, os.Rename(target, aside))
			require.NoError(t, os.Symlink(entry.name, target))

			// While the fault is installed, the loose ref file (the durable
			// record naming the branch) must still be present — a failed probe
			// is neither a deletion nor an absence confirmation. git itself
			// cannot verify the ref while the layout is broken (its own
			// discovery reads HEAD/config/objects/refs), so check the loose
			// ref file directly. Skip when the fault is on refs itself: the
			// loose ref lives under refs/ and is shadowed by the symlink.
			loose := filepath.Join(bare, "refs", "heads", branch)
			if entry.name != "refs" {
				_, looseStatErr := os.Stat(loose)
				assert.NoError(t, looseStatErr,
					"the loose ref file must still be present while the %s probe fails", entry.name)
			}

			exists, err := LocalBranchExists(bare, branch)
			require.Error(t, err,
				"a non-deadline stat error on %s must surface as UNKNOWN, never (false, nil)",
				entry.name)
			assert.False(t, exists)

			// Restore the real entry and confirm the branch verifiably survives
			// via git show-ref, proving the failed probe touched nothing.
			require.NoError(t, os.Remove(target))
			require.NoError(t, os.Rename(aside, target))
			assert.True(t, refExists(t, bare, branch),
				"the branch must verifiably survive a failed probe of %s after restore", entry.name)
		})
	}
}
