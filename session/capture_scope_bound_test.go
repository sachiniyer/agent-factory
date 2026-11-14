package session

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7292 (split out of #7287 by its round-2 review, R6): the Codex capture arm
// and set-conversation's validation resolved the launch dir unbounded, on the
// automatic account swap, the agent swap and the operator-repair paths. Every
// one is now bounded. A swap or repair that hits a lookup that did not answer
// or failed refuses with what to do, before anything is torn down, and a
// capture scope armed on one decides nothing.

// failLaunchDirLookups makes every symlink resolution of root, or of a path
// under it, fail with EIO, as an erroring soft mount does.
func failLaunchDirLookups(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(fsprobe.SetEvalSymlinksForTest(func(path string) (string, error) {
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return "", &fs.PathError{Op: "lstat", Path: path, Err: syscall.EIO}
		}
		return filepath.EvalSymlinks(path)
	}))
}

// requireSwapRefusedOnTheWorktree asserts err is the swap paths' actionable
// WorktreeUnavailableError for a worktree lookup that did not answer
// (timedOut) or failed.
func requireSwapRefusedOnTheWorktree(t *testing.T, err error, timedOut bool) {
	t.Helper()
	var wtErr *WorktreeUnavailableError
	require.ErrorAs(t, err, &wtErr, "a worktree af could not look at refuses the swap: %v", err)
	require.Contains(t, err.Error(), "retry the swap", "the error says what to do")
	if timedOut {
		require.True(t, errors.Is(err, fsprobe.ErrProbeTimedOut), "the error names the bounded probe: %v", err)
		require.True(t, errors.Is(err, context.DeadlineExceeded), "and its deadline: %v", err)
	} else {
		require.True(t, errors.Is(err, syscall.EIO), "the error carries the failed lookup: %v", err)
	}
}

// The automatic Codex account swap arms conversation capture on the launch
// dir after its carry check, before the outgoing runtime stops. Nothing newer
// than the carried rollout means the carry never looks at the launch dir, so
// the arm is the first lookup that does. One that does not answer, or fails,
// refuses the swap's plan with what to do.
func TestAutoCodexAccountSwapRefusesOnAnUnresolvableLaunchDir(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fault    func(t *testing.T, root string)
		timedOut bool
	}{
		{"the lookup does not answer", wedgeLaunchDirResolution, true},
		{"the lookup fails", failLaunchDirLookups, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := codexCarryLayout(t, false)
			require.NoError(t, inst.ValidateAccountSwap("work"), "precondition: the swap plans")
			require.NoError(t, inst.accountSwapLaunch.conversationCapture.ScopeErr())

			tc.fault(t, inst.GetWorktreePath())
			requireSwapRefusedOnTheWorktree(t, withinBound(t, func() error { return inst.ValidateAccountSwap("work") }), tc.timedOut)
		})
	}
}

// An agent swap to Codex arms the same capture in its admission. Its
// preflight stat of the worktree answers here, so the arm's own resolution is
// what does not answer (a mount wedging in between), and the admission
// refuses with what to do.
func TestAgentSwapToCodexRefusesOnAnUnresolvableLaunchDir(t *testing.T) {
	h := newAgentSwapHarness(t)
	codex := filepath.Join(t.TempDir(), tmux.ProgramCodex)
	require.NoError(t, os.WriteFile(codex, []byte("#!/bin/sh\n"), 0o755))
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	cfg.ProgramOverrides[tmux.ProgramCodex] = codex
	require.NoError(t, config.SaveConfig(cfg))
	t.Setenv("CODEX_HOME", t.TempDir())
	plan, err := h.backend.PrepareAgentSwap(h.inst, tmux.ProgramCodex)
	require.NoError(t, err, "precondition: the swap to codex is admitted")
	require.NoError(t, plan.conversationCapture.ScopeErr())

	wedgeLaunchDirResolution(t, h.worktree)
	err = withinBound(t, func() error {
		_, err := h.backend.PrepareAgentSwap(h.inst, tmux.ProgramCodex)
		return err
	})
	requireSwapRefusedOnTheWorktree(t, err, true)
	require.False(t, h.killed.Load(), "the running agent is left alone")
	require.Empty(t, h.pty.cmds, "no replacement starts")
}

// set-conversation proves a rollout is the session's by its launch dir and
// the cwd the rollout records. Either lookup not answering, or failing, is no
// proof either way: the repair is refused with what to do, never as "a
// different session's", within the bound.
func TestSetConversationRefusesOnAnUnresolvableLaunchDir(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault func(t *testing.T, root string)
	}{
		{"the lookup does not answer", wedgeLaunchDirResolution},
		{"the lookup fails", failLaunchDirLookups},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setConversationConfig(t)
			codexHome := t.TempDir()
			t.Setenv("CODEX_HOME", codexHome)
			workDir := filepath.Join(testguard.CanonicalTempDir(t), "wt")
			require.NoError(t, os.MkdirAll(workDir, 0o755))
			writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-07-06T10-17-35-"+setConversationID+".jsonl", workDir)
			inst := setConversationInstance(t, workDir)
			_, err := ValidateSettableConversation(inst, setConversationID)
			require.NoError(t, err, "precondition: the repair is the session's")

			tc.fault(t, workDir)
			err = withinBound(t, func() error {
				_, err := ValidateSettableConversation(inst, setConversationID)
				return err
			})
			require.Error(t, err)
			require.Contains(t, err.Error(), "could not resolve", "the refusal says af could not look")
			require.Contains(t, err.Error(), "then retry", "and what to do")
			require.NotContains(t, err.Error(), "different session", "never that the rollout is another session's")
		})
	}
}

// symlinkedCodexLaunchDir is a launch dir reached through a symlinked parent:
// the link (which the tests wedge), the launch dir as spelled through it, and
// the kernel's spelling, which a rollout records and which still answers.
func symlinkedCodexLaunchDir(t *testing.T) (link, launchDir, kernel string) {
	t.Helper()
	base := testguard.CanonicalTempDir(t)
	kernel = filepath.Join(base, "real", "wt")
	require.NoError(t, os.MkdirAll(kernel, 0o755))
	link = filepath.Join(base, "link")
	require.NoError(t, os.Symlink(filepath.Dir(kernel), link))
	return link, filepath.Join(link, "wt"), kernel
}

// The follow check and set-conversation's held observation arm the session's
// capture scope. A launch dir that does not resolve leaves the scope deciding
// nothing. Here the runtime holds a rollout recording the kernel's spelling,
// which answers. A scope that resolved the launch dir names it; one that could
// not is undecided, never a named conversation.
func TestSessionCodexCaptureScopeDecidesNothingOnAnUnresolvableLaunchDir(t *testing.T) {
	setConversationConfig(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	link, launchDir, kernel := symlinkedCodexLaunchDir(t)
	own := writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-11-11T13-50-27-"+heldTestOwnID+".jsonl", kernel)
	inst := setConversationInstance(t, launchDir)
	held := heldTestInstance(t, own)
	scope := inst.SessionCodexCaptureScope()
	require.NoError(t, scope.ScopeErr())
	conv, err := HeldAgentConversation(tmux.ProgramCodex, held.BindConversationCaptureRuntime(scope))
	require.NoError(t, err)
	require.Equal(t, heldTestOwnID, conv.ID, "precondition: the held rollout is the session's")

	wedgeLaunchDirResolution(t, link)
	scope = withinBound(t, inst.SessionCodexCaptureScope)
	require.True(t, errors.Is(scope.ScopeErr(), fsprobe.ErrProbeTimedOut), "the scope says why: %v", scope.ScopeErr())
	conv, err = HeldAgentConversation(tmux.ProgramCodex, held.BindConversationCaptureRuntime(scope))
	require.False(t, conv.HasID(), "an unresolved scope names no conversation")
	require.True(t, errors.Is(err, errCodexCaptureUndecided), "the answer is undecided: %v", err)
}

// A capture armed on a launch dir that did not resolve decides nothing. Here
// the replacement's runtime holds a new rollout recording the kernel's
// spelling, which answers. A capture that resolved the launch dir would adopt
// it; one that could not reports the channel unavailable and adopts nothing.
func TestCaptureArmedOnAnUnresolvableLaunchDirDecidesNothing(t *testing.T) {
	codexHome := t.TempDir()
	link, launchDir, kernel := symlinkedCodexLaunchDir(t)
	wedgeLaunchDirResolution(t, link)
	snap := withinBound(t, func() ConversationCaptureSnapshot {
		return beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, launchDir)
	})
	require.True(t, errors.Is(snap.ScopeErr(), fsprobe.ErrProbeTimedOut), "the snapshot says why: %v", snap.ScopeErr())
	own := writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-11-11T13-50-27-"+heldTestOwnID+".jsonl", kernel)
	bound := heldTestInstance(t, own).BindConversationCaptureRuntime(snap)

	conv, err := CaptureAgentConversation(tmux.ProgramCodex, bound, 200*time.Millisecond)
	require.False(t, conv.HasID(), "an unresolved scope adopts nothing")
	require.True(t, errors.Is(err, ErrCodexCaptureChannelUnavailable), "and says it could not look: %v", err)
}

// danglingCodexLaunchDir is #7295's layout: the launch dir is a symlink to the
// kernel's directory, which the returned func removes, leaving the link
// dangling.
func danglingCodexLaunchDir(t *testing.T) (launchDir, kernel string, dangle func()) {
	t.Helper()
	root := testguard.CanonicalTempDir(t)
	kernel = filepath.Join(root, "store", "repo")
	require.NoError(t, os.MkdirAll(kernel, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "wt"), 0o755))
	launchDir = filepath.Join(root, "wt", "repo")
	require.NoError(t, os.Symlink(kernel, launchDir))
	return launchDir, kernel, func() { require.NoError(t, os.RemoveAll(kernel)) }
}

// Independent review of #7297, R1, the Codex sibling of #7290 item 1. The
// operator-repair provenance reason covers a lookup that failed as well as one
// that did not answer. A dangling launch dir and an EIO are both lookups that
// failed, with no timeout anywhere: the decline is unverified, and its reason
// claims no timeout.
func TestOperatorRepairProvenanceClaimsNoTimeoutWhenALookupFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault func(t *testing.T, launchDir string, dangle func())
	}{
		{"a dangling launch dir", func(t *testing.T, _ string, dangle func()) { dangle() }},
		{"an EIO", func(t *testing.T, launchDir string, _ func()) { failLaunchDirLookups(t, launchDir) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			launchDir, kernel, dangle := danglingCodexLaunchDir(t)
			path := writeCodexRolloutFileWithCwd(t, t.TempDir(), codexBoundRolloutName(codexBoundRecordedID), kernel)
			require.Equal(t, resumeDecline{}, operatorRepairProvenance(path, launchDir), "precondition: the repair is this session's")

			tc.fault(t, launchDir, dangle)
			got := operatorRepairProvenance(path, launchDir)
			require.True(t, got.unverified, "a lookup that failed is unverified, never another directory: %+v", got)
			require.Equal(t, operatorRepairUnresolvedReason, got.reason)
			for _, claim := range []string{"in time", "timed out", "did not answer"} {
				require.NotContains(t, got.reason, claim, "nothing timed out")
			}
		})
	}
}

// #7297's two Codex fixes on a dangling launch dir, pinned. The staleness
// judge, which the restore guard and the carry share, reads Unresolved, not
// Current: Current would carry an older rollout past a newer one of this
// worktree. Operator-repair provenance reads unverified, not "another
// directory", which would retire the repair.
func TestCodexStalenessOnADanglingLaunchDir(t *testing.T) {
	launchDir, kernel, dangle := danglingCodexLaunchDir(t)
	home := t.TempDir()
	recorded := writeCodexRolloutFileWithCwd(t, home, codexBoundRolloutName(codexBoundRecordedID), kernel)
	newer := writeCodexRolloutFileWithCwd(t, home, codexBoundRolloutName(codexBoundNewerID), kernel)
	now := time.Now()
	setModTime(t, recorded, now.Add(-2*time.Hour))
	setModTime(t, newer, now.Add(-time.Hour))
	files := map[string]struct{}{recorded: {}, newer: {}}
	require.Equal(t, codexRolloutStale, checkCodexRecordedStaleness(recorded, files, boundedLaunchDir(launchDir), operatorRepairWindow{}),
		"precondition: the newer rollout is this worktree's")

	dangle()
	require.Equal(t, codexRolloutUnresolved, checkCodexRecordedStaleness(recorded, files, boundedLaunchDir(launchDir), operatorRepairWindow{}),
		"a dangling launch dir correlates nothing: unresolved, never current")
	got := operatorRepairProvenance(recorded, launchDir)
	require.True(t, got.unverified, "the repair is unverified, never another directory's: %+v", got)
	require.NotContains(t, got.reason, "another directory")
}
