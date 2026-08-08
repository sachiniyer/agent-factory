package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The zero value must REFUSE. This is the property the whole mechanism rests on:
// a call site that constructs a copy without naming a policy, or a struct that
// gains this field later, must inherit refusal rather than permission to skip
// (#3066).
func TestUnreadablePolicy_ZeroValueRefuses(t *testing.T) {
	var policy unreadablePolicy
	require.Equal(t, refuseUnreadable, policy,
		"the zero value must be refuse: skipping has to be typed out, or a caller that forgets inherits data loss")
	require.Equal(t, "refuse", policy.String())

	// And a struct that gains the field later inherits it too.
	var carrier struct {
		Unrelated string
		Policy    unreadablePolicy
	}
	require.Equal(t, refuseUnreadable, carrier.Policy)
}

// An unreadable source is classified as its own error type, so the walker can
// tell "this one file is locked" from "the copy is broken" — every other open
// failure must keep aborting.
func TestCopyRegularFile_ClassifiesAnUnreadableSource(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses mode bits, so no file is unreadable")
	}
	sourceDir, destinationDir := t.TempDir(), t.TempDir()
	locked := filepath.Join(sourceDir, "locked")
	require.NoError(t, os.WriteFile(locked, []byte("SECRET"), 0o600))
	require.NoError(t, os.Chmod(locked, 0o000))

	source, err := os.Open(sourceDir)
	require.NoError(t, err)
	defer source.Close()
	destination, err := os.Open(destinationDir)
	require.NoError(t, err)
	defer destination.Close()

	_, err = copyRegularFileAtWithIdentity(
		source, destination, "locked", locked, filepath.Join(destinationDir, "locked"), nil, nil, nil)
	require.Error(t, err)

	var unreadable *unreadableSourceError
	require.True(t, errors.As(err, &unreadable),
		"a permission failure must be classified, not returned as a generic copy failure: got %v", err)
	require.Equal(t, locked, unreadable.path)
	require.ErrorIs(t, err, os.ErrPermission, "the cause must stay inspectable")

	// The refusal message must name the file and tell the operator what to do.
	require.Contains(t, unreadable.Error(), locked)
	require.Contains(t, unreadable.Error(), "permissions")
}

// A refusal must say which operation it is refusing, because the remedy differs:
// an archive can be retried after a chmod; a restore is telling the operator the
// archive holds a file they cannot read and must not proceed without it.
func TestUnreadableSourceError_NamesTheOperation(t *testing.T) {
	for _, operation := range []string{"archive", "restore", "move"} {
		err := &unreadableSourceError{path: "/w/secret", operation: operation}
		require.Contains(t, err.Error(), operation)
		require.Contains(t, err.Error(), "/w/secret")
		require.Contains(t, err.Error(), "without saying so",
			"the message must state WHY omitting it silently is unacceptable")
	}
}

// The operation must be stamped by the caller that knows it, and reach the
// message. Previously the copier hard-coded "copy", so every failure said the
// same thing whether the user was moving or restoring — and the operation-aware
// branch was exercised only by hand-built errors, which proved nothing about the
// wiring (#3087 review).
func TestRelocate_StampsTheOperationOntoAnUnreadableSource(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses mode bits")
	}
	// An unstamped error still has to read correctly.
	bare := &unreadableSourceError{path: "/w/secret"}
	require.Contains(t, bare.Error(), "cannot copy this worktree")
	require.NotContains(t, bare.Error(), "cannot  this",
		"an unstamped operation must not render an empty verb")

	// Stamping is what relocateWorktreeTo does at its boundary; assert the
	// message changes with it, in both directions that matter.
	for _, operation := range []string{"move", "restore"} {
		stamped := &unreadableSourceError{path: "/w/secret"}
		stamped.operation = operation
		require.Contains(t, stamped.Error(), "cannot "+operation+" this worktree")
		require.Contains(t, stamped.Error(), operation+"ing while silently omitting")
	}
}

// The stamp must reach the USER-VISIBLE message, not just the struct.
//
// fmt.Errorf formats and CACHES the inner error's text when the wrapper is
// constructed, so stamping the nested error afterwards updates the field and not
// the message anyone reads. The first version of this fix did exactly that and
// still printed "cannot copy this worktree" on a restore (#3087 review).
func TestUnreadableSourceError_StampMustPrecedeWrapping(t *testing.T) {
	// Wrapping BEFORE the stamp: the outer text is already frozen.
	late := &unreadableSourceError{path: "/w/secret"}
	wrappedFirst := fmt.Errorf("failed to copy worktree into private staging directory /s: %w", late)
	late.operation = "restore"
	require.NotContains(t, wrappedFirst.Error(), "cannot restore this worktree",
		"precondition: a wrapper caches the inner text, so a late stamp cannot reach it")

	// Stamping BEFORE the wrap, which is the order the copier now uses.
	early := &unreadableSourceError{path: "/w/secret"}
	early.operation = "restore"
	wrappedAfter := fmt.Errorf("failed to copy worktree into private staging directory /s: %w", early)
	require.Contains(t, wrappedAfter.Error(), "cannot restore this worktree",
		"the operation must reach the message the user actually sees")

	// And the typed error stays reachable through the wrapper either way.
	var found *unreadableSourceError
	require.True(t, errors.As(wrappedAfter, &found))
	require.Equal(t, "restore", found.operation)
}
