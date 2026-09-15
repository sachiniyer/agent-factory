package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// committedKillWarningTestError stands in for the daemon's
// *mutationCommittedError on the kill path: a durable tombstone that committed
// whose post-commit teardown/storage follow-up failed. Implementing
// MutationCommitted() bool { return true } makes apiclient.IsMutationCommitted
// classify it, exactly as the real control-socket error (rpcMutationCommittedError)
// would be classified by the CLI's RunE.
type committedKillWarningTestError struct{}

func (committedKillWarningTestError) Error() string {
	return "kill committed, but terminal teardown could not confirm the pane was gone"
}

func (committedKillWarningTestError) MutationCommitted() bool { return true }

// plainKillError is a non-committed failure (it does NOT implement
// MutationCommitted), so apiclient.IsMutationCommitted returns false and the
// CLI's else-if branch must forward it as a hard failure instead of swallowing
// it as a silent success.
type plainKillError struct{ msg string }

func (e *plainKillError) Error() string { return e.msg }

// TestSessionsKillReportsCommittedTeardownAsSuccess pins #3252 at the CLI: a
// kill whose durable tombstone committed but whose post-commit follow-up failed
// must be reported as ok:true with a warning and a ZERO exit code — not as
// jsonError with a non-zero exit. archive and restore in this same file already
// classify this outcome; kill was the only lifecycle verb that did not, so a
// committed kill read as a hard failure and automation that gates on the exit
// code retried it, racing the in-flight guard ("kill already in progress for
// session %q") or re-tombstoning an already-UserKilled row. Mirrors
// TestSessionsArchiveReportsCommittedHookWarningAsSuccess by substituting the
// killSessionViaDaemon seam with a committed-marker error.
func TestSessionsKillReportsCommittedTeardownAsSuccess(t *testing.T) {
	setupRepoForCmd(t)

	prev := killSessionViaDaemon
	killSessionViaDaemon = func(daemon.KillSessionRequest) error {
		return committedKillWarningTestError{}
	}
	defer func() { killSessionViaDaemon = prev }()

	out, err := runCmdCaptureStdout(t, sessionsKillCmd, []string{"worker"})
	require.NoError(t, err, "a committed kill must not be presented as a retryable failure (non-zero exit)")

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(out, &parsed))
	assert.Equal(t, true, parsed["ok"], "a committed kill must report ok:true")
	assert.Contains(t, parsed["warning"], "teardown could not confirm",
		"the committed outcome's warning text must be surfaced to the caller")
}

// TestSessionsKill_StillHardFailsOnCleanError pins that the committed
// classification does not swallow a genuine failure: a plain (non-committed)
// daemon error must still come back as a non-zero exit via jsonError, so a kill
// that actually failed-with-nothing-committed remains a hard failure for
// automation to retry. Guards against an over-broad committed classification
// that would turn every kill error into a success.
func TestSessionsKill_StillHardFailsOnCleanError(t *testing.T) {
	setupRepoForCmd(t)

	prev := killSessionViaDaemon
	killSessionViaDaemon = func(daemon.KillSessionRequest) error {
		return &plainKillError{msg: "instance not found: worker"}
	}
	defer func() { killSessionViaDaemon = prev }()

	_, err := runCmdCaptureStdout(t, sessionsKillCmd, []string{"worker"})
	require.Error(t, err, "a clean (non-committed) failure must remain a hard failure with a non-zero exit")
	assert.True(t, strings.Contains(err.Error(), "instance not found"),
		"the daemon's clean failure message must be surfaced, got: %v", err)
}
