package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	stallCodexRolloutOpens(t, func(string) bool { return true })
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

// heldAliasStore is the #7370 review's layout B: a Codex store at home/sessions
// that the runtime reaches under a second spelling, alias, which only (device,
// inode) proves is the same directory. That is #7120 review F7's shape: on
// darwin a CODEX_HOME spelled in a case other than the one on disk, or a store
// reached through a second mount of it. A resolver that keeps the alias's
// spelling stands in for darwin's EvalSymlinks, which keeps the case it was
// given; every other path resolves for real.
type heldAliasStore struct {
	home, sessions, alias, workDir string
}

func newHeldAliasStore(t *testing.T) heldAliasStore {
	t.Helper()
	base := testguard.CanonicalTempDir(t)
	s := heldAliasStore{
		home:    filepath.Join(base, "home"),
		alias:   filepath.Join(base, "alias"),
		workDir: filepath.Join(base, "wt"),
	}
	s.sessions = filepath.Join(s.home, "sessions")
	require.NoError(t, os.MkdirAll(s.sessions, 0o755))
	require.NoError(t, os.MkdirAll(s.workDir, 0o755))
	require.NoError(t, os.Symlink(s.home, s.alias))
	t.Cleanup(fsprobe.SetEvalSymlinksForTest(func(path string) (string, error) {
		if path == s.alias || strings.HasPrefix(path, s.alias+string(filepath.Separator)) {
			if _, err := os.Stat(path); err != nil {
				return "", err
			}
			return path, nil
		}
		return filepath.EvalSymlinks(path)
	}))
	return s
}

// rollout writes a top-level rollout of the worktree for id and returns its
// path spelled under home, and spelled under the alias.
func (s heldAliasStore) rollout(t *testing.T, id string) (own, aliased string) {
	t.Helper()
	path := writeCodexRolloutFileWithCwd(t, s.home, "rollout-2026-11-11T13-50-27-"+id+".jsonl", s.workDir)
	rel, err := filepath.Rel(s.home, path)
	require.NoError(t, err)
	return path, filepath.Join(s.alias, rel)
}

// judge is HeldAgentConversation for a runtime that holds held, bound to this
// store and worktree, within the test's bound.
func (s heldAliasStore) judge(t *testing.T, held ...string) (AgentConversationData, error) {
	t.Helper()
	snap := heldTestInstance(t, held...).BindConversationCaptureRuntime(
		beginConversationCaptureAtCodexHomeAndWorkingDir(s.home, s.workDir))
	type answer struct {
		conv AgentConversationData
		err  error
	}
	got := withinBound(t, func() answer {
		conv, err := HeldAgentConversation(tmux.ProgramCodex, snap)
		return answer{conv, err}
	})
	return got.conv, got.err
}

// wedgeStatOf makes every stat of exactly path hold the test's flight clock
// out past the probe bound and then wait until the test ends, as one
// directory on a mount that lost that lookup does. Its ancestors and the
// paths under it answer. The bound is declared on the flight clock, never a
// shrunken probeTimeout: the wedged flight times out because the wedge
// spends a whole bound on it, and every other flight in the test answers
// whenever its work finishes, since nothing moves the clock while it runs
// (#7456). A bound shrunk for the whole test reaches flights the wedge does
// not mean to wedge — the unwedged ancestor stats and the header read the
// walk past the stuck ancestor still needs — whose work a loaded -race
// runner can stretch past it, reading a slow but answering lookup as a look
// that did not happen and leaving the rollout unclassified (#7593, #7904).
func wedgeStatOf(t *testing.T, path string) {
	t.Helper()
	release := make(chan struct{})
	clock := wedgeFlightClock(t)
	restoreStat := fsprobe.SetStatPathForTest(func(p string) (os.FileInfo, error) {
		if p == path {
			clock.Advance(wedgeHeldOut)
			<-release
		}
		return os.Stat(p)
	})
	t.Cleanup(func() {
		close(release)
		// Let any flight still blocked in the stat finish before the real
		// stat comes back.
		time.Sleep(50 * time.Millisecond)
		restoreStat()
	})
}

// requireHeldUndecided asserts the judge's could-not-look answer: no
// conversation, and errCodexCaptureUndecided, which the follow reads as
// Unchanged, capture's held fallback as nothing to record, and a repair's
// observation as unstamped.
func requireHeldUndecided(t *testing.T, conv AgentConversationData, err error, msg string) {
	t.Helper()
	require.False(t, conv.HasID(), "%s: named %q", msg, conv.ID)
	require.True(t, errors.Is(err, errCodexCaptureUndecided), "%s: the answer is undecided: %v", msg, err)
}

// Layout B (#7370 review R1). The runtime holds this worktree's rollout under a
// spelling of the store that only (device, inode) proves is the store.
// Containment has three answers: in, out, and could-not-look. "Out" needs both
// the spelling and the identity to have looked: the store root's stat, and
// every ancestor's, with a proven absence counting as an answer. A stat on
// that path that did not answer leaves the rollout unclassified, so the judge
// is undecided. It used to read as "another store" and answer (∅, nil): the
// runtime holds nothing. A set-conversation repair then stamped held="" and the
// next follow read the pane's own thread as a move, undoing the repair.
func TestHeldCodexConversationUnderAnAliasedStoreNeedsTheIdentityToLook(t *testing.T) {
	t.Run("every lookup answers: the identity proves it is the store", func(t *testing.T) {
		s := newHeldAliasStore(t)
		_, aliased := s.rollout(t, heldTestOwnID)
		conv, err := s.judge(t, aliased)
		require.NoError(t, err)
		require.Equal(t, heldTestOwnID, conv.ID, "precondition: (device, inode) proves the aliased rollout is the store's")
	})
	t.Run("the store root's stat does not answer", func(t *testing.T) {
		s := newHeldAliasStore(t)
		_, aliased := s.rollout(t, heldTestOwnID)
		wedgeStatOf(t, s.sessions)
		conv, err := s.judge(t, aliased)
		requireHeldUndecided(t, conv, err, "a store root af could not stat proves nothing outside it")
		// The stuck stat keeps its latch while the resolve registry keeps
		// answering, so every check in that window takes this path too.
		conv, err = s.judge(t, aliased)
		requireHeldUndecided(t, conv, err, "a check while the root's stat is still stuck")
	})
	t.Run("the aliased store directory's stat does not answer", func(t *testing.T) {
		s := newHeldAliasStore(t)
		_, aliased := s.rollout(t, heldTestOwnID)
		// The ancestor whose identity is the store's: alias/sessions.
		wedgeStatOf(t, filepath.Join(s.alias, "sessions"))
		conv, err := s.judge(t, aliased)
		requireHeldUndecided(t, conv, err, "an ancestor af could not stat may be the store")
	})
	t.Run("a lower ancestor's stat does not answer", func(t *testing.T) {
		s := newHeldAliasStore(t)
		_, aliased := s.rollout(t, heldTestOwnID)
		// The day directory is not the store; the walk goes on past it and
		// finds the store above.
		wedgeStatOf(t, filepath.Dir(aliased))
		conv, err := s.judge(t, aliased)
		require.NoError(t, err)
		require.Equal(t, heldTestOwnID, conv.ID, "an ancestor above the stuck one proves containment")
	})
}

// Layout C (#7370 review R1): layout B plus a second top-level rollout of this
// worktree, held under the store's own spelling. With every lookup answering
// the runtime holds two candidates, which is ambiguous. When the aliased one's
// identity could not be looked up, the judge must not name the other from a
// partial view: a follow would move a record naming the aliased thread, and
// capture would record from half the evidence.
func TestHeldCodexConversationNeverNamesOneRolloutWhileAnotherIsUnclassified(t *testing.T) {
	layout := func(t *testing.T) (heldAliasStore, []string) {
		s := newHeldAliasStore(t)
		own, _ := s.rollout(t, heldTestOwnID)
		_, sibling := s.rollout(t, heldTestSiblingID)
		return s, []string{own, sibling}
	}
	t.Run("every lookup answers", func(t *testing.T) {
		s, held := layout(t)
		_, err := s.judge(t, held...)
		require.True(t, errors.Is(err, ErrCodexConversationAmbiguous), "precondition: two candidates are ambiguous: %v", err)
	})
	t.Run("the store root's stat does not answer", func(t *testing.T) {
		s, held := layout(t)
		wedgeStatOf(t, s.sessions)
		conv, err := s.judge(t, held...)
		requireHeldUndecided(t, conv, err, "one candidate named while the other is unclassified")
	})
	t.Run("the aliased store directory's stat does not answer", func(t *testing.T) {
		s, held := layout(t)
		wedgeStatOf(t, filepath.Join(s.alias, "sessions"))
		conv, err := s.judge(t, held...)
		requireHeldUndecided(t, conv, err, "one candidate named while the other is unclassified")
	})
}
