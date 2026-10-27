package session

import (
	"errors"
	"io/fs"
	"os/exec"
	"syscall"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/require"
)

// errnoFailingCmdExec is a tmux mock executor whose every command fails with a
// fork/exec *fs.PathError carrying the supplied errno — the shape
// os.StartProcess surfaces from a refused clone (EAGAIN under RLIMIT_NPROC /
// cgroup pids.max, ENOMEM under a kernel OOM window, EMFILE / ENFILE on a full
// fd table). Every bounded tmux call this session makes (display-message for
// panePID, list-panes for capture, kill-session, and the has-session probe)
// returns the same errno-carrying fault, modelling the wedged-environment
// branch where no tmux command can spawn at all — exactly the producers #5296
// names for the environmental errno class.
func errnoFailingCmdExec(errno syscall.Errno) cmd_test.MockCmdExec {
	// *fs.PathError is what os/exec surfaces from a failed fork/exec, and its
	// Unwrap returns the wrapped errno; the daemon's classifier walks the
	// chain via errors.Is, so the errno must stay reachable through that
	// unwrap rather than as text in a formatted message.
	bare := &fs.PathError{Op: "fork/exec", Path: "tmux", Err: errno}
	return cmd_test.MockCmdExec{
		RunFunc:    func(*exec.Cmd) error { return bare },
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, bare },
	}
}

// TestCleanupRespawnedAccountTabsPreservesErrnoInChain pins the #5296 contract
// at the cleanup wrap commit c5af54d5 missed.
//
// The daemon's lost-restore classifier (daemon/lostrestore_environmental.go)
// resolves "environmental" through errors.Is over the wrapped chain — never
// message text — and a CloseAndWaitForPaneExit that fails with a fork/exec
// errno (EAGAIN, ENOMEM, ...) must keep that errno reachable after
// cleanupRespawnedAccountTabs wraps it, so the verdict is lostRestoreEnvErrno
// (unbounded environmental wait) rather than lostRestoreEnvNone (charged to the
// structural give-up budget, durable at lostRestoreMaxAttempts=6). Before the
// %v→%w fix the wrap flattened the cleanup error to prose, the errno survived
// only as text, and an exhausted environment (cgroup pids.max, fd table,
// memory) was misclassified as a session defect — the exact #5295 incident
// failure mode the classifier exists to prevent.
//
// The respawned-account-tab path is the narrowest branch in setupTabs:
// respawnedAccountTabs only accrues for account-scoped instances
// (account != ""), and the deferred cleanup only fires when a LATER tab fails
// — so non-account usage and ordinary two-tab restores never reach this wrap,
// and the bug stays hidden unless a test drives it directly. This test builds
// real *tmux.TmuxSessions through the same mock-cmdExec seam existing session
// tests use, drives the real cleanupRespawnedAccountTabs and the real
// finishRecoverTabFailure (with the agent pane closing cleanly, mirroring the
// production event where only the sibling tab's cleanup fails), and asserts
// the errno stays reachable through errors.Is at every layer the daemon's
// classifier consumes.
func TestCleanupRespawnedAccountTabsPreservesErrnoInChain(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	for _, tc := range []struct {
		name  string
		errno syscall.Errno
	}{
		{"EAGAIN", syscall.EAGAIN},
		{"ENOMEM", syscall.ENOMEM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failingExec := errnoFailingCmdExec(tc.errno)

			// The respawned account sibling whose cleanup fails with the
			// environmental errno. The mock executor fails every tmux command
			// with the supplied errno, so CloseAndWaitForPaneExit surfaces the
			// "could not establish the pane's complete process tree before
			// kill-session" error whose chain carries the errno through the
			// capture's list-panes fault — the cleanup error shape setupTabs
			// hands cleanupRespawnedAccountTabs in production.
			respawnedAccountTab := tmux.NewTmuxSessionFromSanitizedNameWithDeps(
				"af_errno_respawned_account_tab",
				tmux.ProgramClaude,
				persistPtyFactory{t: t, cmdExec: failingExec},
				failingExec,
			)

			// The primary restore error: a non-errno refusal of the
			// accountTabScopeUnknownError shape setupTabs returns when a
			// persisted account sibling's tmux probe does not answer — one of
			// the routine primary-without-errno shapes the classifier exists
			// to disambiguate. The errno, if any, must come from the cleanup
			// of the respawned account tab alone.
			primary := &accountTabScopeUnknownError{title: "errno-chain", tab: "respawned"}
			require.False(t, errors.Is(primary, tc.errno),
				"precondition: the primary restore error must not itself carry the errno, else the cleanup wrap is not the one under test")

			cleanupErr := cleanupRespawnedAccountTabs(primary, []*tmux.TmuxSession{respawnedAccountTab})
			require.Error(t, cleanupErr)
			require.True(t, errors.Is(cleanupErr, tc.errno),
				"cleanupRespawnedAccountTabs must keep the cleanup errno reachable through errors.Is "+
					"(the #5296 classifier reads errno only through the unwrap chain, never message text; "+
					"%%v flattened it to prose the classifier deliberately does not match): %v", cleanupErr)
			require.Contains(t, cleanupErr.Error(), "cleanup respawned account tab",
				"the cleanup-wrap diagnostic shape must stay present so observers read the failure origin")

			// finishRecoverTabFailure wraps with %%w (preserving the chain)
			// and runs CloseAndWaitForPaneExit on the agent session. Mirroring
			// the production event where only the sibling tab's cleanup fails,
			// the agent session closes cleanly — so the cleanup errno from
			// the respawned account tab is the only errno carrier in the final
			// error the daemon's classifier sees.
			agentExec := nameKeyedExec(map[string]bool{"af_errno_agent": true})
			agentSession := tmux.NewTmuxSessionFromSanitizedNameWithDeps(
				"af_errno_agent",
				tmux.ProgramClaude,
				persistPtyFactory{t: t, cmdExec: agentExec},
				agentExec,
			)

			final := finishRecoverTabFailure("errno-chain", false, tmux.RestoreRespawned, agentSession, cleanupErr)
			require.Error(t, final)
			require.True(t, errors.Is(final, tc.errno),
				"the daemon's classifier consumes the error finishRecoverTabFailure returns; "+
					"the cleanup errno must survive the recover-wrap to stay reachable through errors.Is "+
					"so the verdict is lostRestoreEnvErrno (unbounded wait) and not lostRestoreEnvNone "+
					"(structural give-up budget): %v", final)

			// Negative control: the errno's text present inside a plain string
			// error must NOT classify — pinning that the classifier genuinely
			// walks the unwrap chain rather than matching prose. Without this
			// property the original %v wrap would have read as environmental
			// from text, masking the bug.
			plainText := errors.New("cleanup respawned account tab: fork/exec tmux: " + tc.errno.Error())
			require.False(t, errors.Is(plainText, tc.errno),
				"the classifier must not match the errno from message text alone — the negative control for the unwrap-chain contract")
		})
	}
}
