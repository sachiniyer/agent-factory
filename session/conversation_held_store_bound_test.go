package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7279 (continuation of #7287): the held-rollout judge correlates each held
// rollout against the snapshot's Codex session store (CODEX_HOME/sessions) by
// resolving the store root, each held rollout path, the (device, inode)
// identities, and a fresh rollout's header — every one a store-touching syscall
// on the daemon's poll/follow path and the set-conversation RPC handler. A
// CODEX_HOME on a mount that never answers (NFS, FUSE, a dead sshfs) wedged each
// unbounded lookup forever, stalling the per-session capture loop, the
// operator's set-conversation repair, and a clean daemon shutdown. Every one is
// now bounded (fsprobe, #7279): a lookup that does not answer within the bound
// is "could not look", mapped to undecided, never another directory's — never a
// named conversation.

// wedgeCodexRolloutOpenForTest makes every bounded header read of a rollout
// block until the test ends, the way a wedged CODEX_HOME does, and bounds af's
// reads at 50ms. readCodexRolloutFirstLine routes the open through fsprobe's
// per-path flight (ReadFileWithOpen), so a blocked open surfaces as the same
// bounded non-answer a blocked read does, never a hang.
func wedgeCodexRolloutOpenForTest(t *testing.T) {
	t.Helper()
	release := make(chan struct{})
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(50 * time.Millisecond)
	old := openCodexRollout
	openCodexRollout = func(string) (*os.File, error) {
		<-release
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() {
		close(release)
		// Let any flight still blocked in the open finish before the real
		// open comes back.
		time.Sleep(50 * time.Millisecond)
		openCodexRollout = old
		restoreTimeout()
	})
}

// heldStoreBoundSnap is the held-rollout judge bound to a runtime that holds one
// rollout of workDir in a Codex store at codexHome: the laminated shape the
// wedge tests below probe.
func heldStoreBoundSnap(t *testing.T, codexHome, workDir, id string) (ConversationCaptureSnapshot, string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(workDir, 0o755))
	own := writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-11-11T13-50-27-"+id+".jsonl", workDir)
	snap := heldTestInstance(t, own).BindConversationCaptureRuntime(
		beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir))
	return snap, own
}

// The store root is the mount the runtime holds its rollout files open on. A
// CODEX_HOME whose symlink resolution never answers used to block the judge's
// unbounded pathutil.ResolveForCompare(sessions) forever, so the follow/capture
// loop for one recorded session wedged for good. The store root now resolves
// inside fsprobe's bound: one that does not answer is undecided, never another
// directory's — never a named conversation. Before the fix, fsprobe's
// SetEvalSymlinksForTest seam could not reach the store root (pathutil calls
// filepath.EvalSymlinks directly, not the seam), so the judge named the
// rollout through the wedge; after, the seam wedges it and the answer is
// undecided within the bound.
func TestHeldCodexConversationDoesNotStallOnAWedgedStoreResolve(t *testing.T) {
	codexHome := t.TempDir()
	workDir := filepath.Join(testguard.CanonicalTempDir(t), "wt")
	snap, _ := heldStoreBoundSnap(t, codexHome, workDir, heldTestOwnID)
	conv, err := HeldAgentConversation(tmux.ProgramCodex, snap)
	require.NoError(t, err)
	require.Equal(t, heldTestOwnID, conv.ID, "precondition: with the store answering, the held rollout is this launch dir's")

	// A store-wide symlink wedge — root, held path, and every ancestor — the
	// way a CODEX_HOME that has whole-mount_STOPped answering looks.
	wedgeLaunchDirResolution(t, filepath.Join(codexHome, "sessions"))
	type answer struct {
		conv AgentConversationData
		err  error
	}
	got := withinBound(t, func() answer {
		conv, err := HeldAgentConversation(tmux.ProgramCodex, snap)
		return answer{conv, err}
	})
	require.False(t, got.conv.HasID(), "a store af could not resolve names no conversation")
	require.True(t, errors.Is(got.err, errCodexCaptureUndecided), "the answer is undecided, never another directory's: %v", got.err)
}

// A held rollout path is on the same store mount, so its resolution is bounded
// too. The judge's containment check is spelling then (device, inode); when a
// held path's spelling does not answer and the store root's identity could not
// be statted either (the same wedged store), the rollout is unclassified — never
// confidently "another store", which a look that did not happen must never be.
// Before the fix, neither lookup went through the fsprobe seam, so both
// answered (on a real temp dir) and the judge named the rollout; after, both
// are bounded and the answer is undecided within the bound.
func TestHeldCodexConversationDoesNotStallOnAWedgedHeldPathResolve(t *testing.T) {
	codexHome := t.TempDir()
	workDir := filepath.Join(testguard.CanonicalTempDir(t), "wt")
	snap, own := heldStoreBoundSnap(t, codexHome, workDir, heldTestOwnID)
	conv, err := HeldAgentConversation(tmux.ProgramCodex, snap)
	require.NoError(t, err)
	require.Equal(t, heldTestOwnID, conv.ID, "precondition: with the store answering, the held rollout is this launch dir's")

	// Wedge the held rollout's symlink resolution (its day directory and below,
	// so the store root — its parent — still resolves), and wedge the store
	// root's identity stat (the whole store), so the (device, inode) fallback
	// cannot prove containment either.
	wedgeLaunchDirResolution(t, filepath.Dir(own))
	wedgeWorktreeStat(t, filepath.Join(codexHome, "sessions"))
	type answer struct {
		conv AgentConversationData
		err  error
	}
	got := withinBound(t, func() answer {
		conv, err := HeldAgentConversation(tmux.ProgramCodex, snap)
		return answer{conv, err}
	})
	require.False(t, got.conv.HasID(), "a held path af could not resolve and whose store af could not stat names no conversation")
	require.True(t, errors.Is(got.err, errCodexCaptureUndecided), "the answer is undecided, never another directory's: %v", got.err)
}

// A rollout's session_meta header is read on the follow/capture loop's every
// rearm (a rollout this daemon has not classified yet) and on the
// set-conversation RPC handler. The open and the read run inside one fsprobe
// per-path bound now, so a wedged CODEX_HOME whose open never answers surfaces
// as an unclassifiable header within the bound, never a hung capture loop.
// Before the fix, readCodexRolloutFirstLine called os.OpenFile directly — no
// bound — and there was no seam to wedge; here the bounded flight is the seam,
// and a blocked open is the bounded non-answer.
func TestHeldCodexConversationDoesNotStallOnAWedgedStoreHeaderRead(t *testing.T) {
	codexHome := t.TempDir()
	workDir := filepath.Join(testguard.CanonicalTempDir(t), "wt")
	snap, _ := heldStoreBoundSnap(t, codexHome, workDir, heldTestOwnID)

	// Wedge only the rollout header read: the store root, the held path and
	// the identity stats all answer (the judge reaches the classification),
	// while a rollout af has not classified yet is read for the first time
	// against an open that never returns.
	wedgeCodexRolloutOpenForTest(t)
	type answer struct {
		conv AgentConversationData
		err  error
	}
	got := withinBound(t, func() answer {
		conv, err := HeldAgentConversation(tmux.ProgramCodex, snap)
		return answer{conv, err}
	})
	require.False(t, got.conv.HasID(), "a header af could not read names no conversation")
	require.True(t, errors.Is(got.err, errCodexCaptureUndecided), "the answer is undecided, never another directory's: %v", got.err)
}
