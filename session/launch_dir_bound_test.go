package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7272 (continuation of #7268): the Claude poll, restore, respawn and
// account-swap paths still read the launch dir unbounded after #7268 bounded
// its project spellings: the ownership check's launch-side ResolveForCompare,
// the attribution rule's resolutions, and the respawn's stats of the
// worktree. On a wedged mount (NFS, FUSE, a dead sshfs) each held its caller
// forever. Every one is now bounded, and one that does not answer within the
// bound is "could not look": never "not the same directory", never "nothing
// there", and never a reason to start on the id or start fresh.

// launchDirWedge returns the block a seam calls with the path it probes: one
// at or under root waits until the test ends, the way a launch dir on a wedged
// mount does, and any other path answers. It bounds af's probes at 50ms.
func launchDirWedge(t *testing.T, root string) func(string) {
	t.Helper()
	release := make(chan struct{})
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(50 * time.Millisecond)
	t.Cleanup(func() {
		close(release)
		// Let every flight still blocked in a seam finish before the bound
		// comes back.
		time.Sleep(50 * time.Millisecond)
		restoreTimeout()
	})
	return func(path string) {
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			<-release
		}
	}
}

// wedgeLaunchDirResolution wedges every symlink resolution of root or a path
// under it: fsprobe.EvalSymlinks and fsprobe.ResolveForCompare's walk alike.
func wedgeLaunchDirResolution(t *testing.T, root string) {
	t.Helper()
	wait := launchDirWedge(t, root)
	t.Cleanup(fsprobe.SetEvalSymlinksForTest(func(path string) (string, error) {
		wait(path)
		return filepath.EvalSymlinks(path)
	}))
}

// failLaunchDirResolution makes every symlink resolution of root or a path
// under it fail at once, as an unreadable ancestor does: the answer a timed-out
// resolution must give.
func failLaunchDirResolution(t *testing.T, root string) func() {
	t.Helper()
	return fsprobe.SetEvalSymlinksForTest(func(path string) (string, error) {
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return "", os.ErrPermission
		}
		return filepath.EvalSymlinks(path)
	})
}

// The ownership check's launch side resolves the launch dir. A resolution that
// does not answer within the bound decides exactly as one that failed: the
// launch dir's recorded spelling decides alone. That never needs the mount.
func TestClaudeTranscriptOwnershipDoesNotStallOnAWedgedLaunchDir(t *testing.T) {
	for _, tc := range []struct {
		name string
		// recorded is the directory the transcript records, relative to the
		// layout's base; it is never under the wedged spelling.
		recorded string
		// resolved is the answer when the launch dir resolves; recordedOnly
		// when only its recorded spelling can decide.
		resolved, recordedOnly claudeOwnership
	}{
		{
			name:         "the transcript records the launch dir's resolved spelling",
			recorded:     filepath.Join("real", "repo"),
			resolved:     claudeOwnedHere,
			recordedOnly: claudeOwnedHere,
		},
		{
			// link.repo and link/repo file under one project name, and both
			// reach real/repo: only the resolution proves they are one
			// directory. Without it the recorded spelling reads the collision
			// as another directory's, which only ever leaves the recorded
			// conversation the newest.
			name:         "a spelling that collides with the launch dir's and resolves to it",
			recorded:     "link.repo",
			resolved:     claudeOwnedHere,
			recordedOnly: claudeOwnedElsewhere,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := testguard.CanonicalTempDir(t)
			real := filepath.Join(base, "real")
			require.NoError(t, os.MkdirAll(filepath.Join(real, "repo"), 0o755))
			link := filepath.Join(base, "link")
			require.NoError(t, os.Symlink(real, link))
			require.NoError(t, os.Symlink(filepath.Join(real, "repo"), filepath.Join(base, "link.repo")))
			launchDir := filepath.Join(link, "repo")
			// Past 200 characters Claude suffixes a project name with a hash of
			// the whole path, and no two spellings collide.
			require.Equal(t, claudeProjectName(launchDir), claudeProjectName(filepath.Join(base, "link.repo")),
				"premise: link.repo files under the launch dir's project name (a temp root short enough to collide)")
			path := filepath.Join(t.TempDir(), newerConversationID+".jsonl")
			require.NoError(t, os.WriteFile(path, []byte(claudeRecordLine(t, filepath.Join(base, tc.recorded))), 0o600))
			transcript := claudeTranscript{dir: filepath.Dir(path), name: filepath.Base(path)}

			require.Equal(t, tc.resolved, claudeTranscriptOwnership(transcript, launchDir), "precondition: the answer with the launch dir resolved")
			restore := failLaunchDirResolution(t, link)
			require.Equal(t, tc.recordedOnly, claudeTranscriptOwnership(transcript, launchDir), "precondition: the answer when the launch dir cannot be resolved")
			restore()

			wedgeLaunchDirResolution(t, link)
			got := withinBound(t, func() claudeOwnership { return claudeTranscriptOwnership(transcript, launchDir) })
			require.Equal(t, tc.recordedOnly, got, "a resolution that did not answer decides as one that failed")
		})
	}
}

// The attribution rule resolves the worktree, its repository and the launch
// dir. Any of them not answering within the bound is a directory af cannot
// prove, which is attributed nothing: a newer conversation there is no
// evidence the session moved on.
func TestNewerConversationEvidenceDoesNotStallOnAWedgedLaunchDir(t *testing.T) {
	for _, tc := range []struct {
		name string
		// launch is the launch dir relative to the worktree; wedged is the
		// path, relative to the worktree, that does not answer.
		launch, wedged string
	}{
		{name: "the worktree", launch: ".", wedged: "."},
		{name: "a launch dir inside it", launch: "sub", wedged: "sub"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workDir := filepath.Join(testguard.CanonicalTempDir(t), "wt")
			require.NoError(t, os.MkdirAll(filepath.Join(workDir, "sub"), 0o755))
			inst := claudeRestoreInstance(t, workDir, carryTestID)
			launchDir := filepath.Join(workDir, tc.launch)
			require.True(t, inst.newerConversationIsEvidence(launchDir), "precondition: af owns this launch dir exclusively")

			wedgeLaunchDirResolution(t, filepath.Join(workDir, tc.wedged))
			got := withinBound(t, func() bool { return inst.newerConversationIsEvidence(launchDir) })
			require.False(t, got, "a directory af could not resolve is attributed nothing")
		})
	}
}

// The restore's judge reaches both: a newer conversation in the listed project
// sends it through the ownership check, and a newer conversation owned here
// through the attribution rule. With the launch dir wedged it answers within
// the bound and resumes the recorded id, where with the launch dir answering it
// starts fresh: a look that did not happen is never a reason to.
func TestClaudeRestoreDoesNotStallOnAWedgedLaunchDirOwnership(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	workDir, recordedProject, _ := claudeRestoreWorktree(t)
	link := filepath.Dir(workDir)
	// Claude records the kernel's cwd, the resolved spelling, which answers;
	// the launch dir as af spells it is the one that does not.
	resolvedDir, err := filepath.EvalSymlinks(workDir)
	require.NoError(t, err)
	past := time.Now().Add(-time.Hour)
	setModTime(t, writeCarryFile(t, configDir, filepath.Join(recordedProject, carryTestID+".jsonl"), claudeRecordLine(t, resolvedDir)), past)
	setModTime(t, writeCarryFile(t, configDir, filepath.Join(recordedProject, newerConversationID+".jsonl"), claudeRecordLine(t, resolvedDir)), past.Add(time.Minute))
	inst := claudeRestoreInstance(t, workDir, carryTestID)

	_, ok, decline := prepareExactResumeConversation(inst, "claude")
	require.False(t, ok, "precondition: with the launch dir answering, the newer conversation is this worktree's")
	require.Equal(t, staleCarryReason, decline.reason)

	wedgeLaunchDirResolution(t, link)
	got := withinBound(t, func() exactResumeAnswer {
		program, ok, decline := prepareExactResumeConversation(inst, "claude")
		return exactResumeAnswer{program, ok, decline}
	})
	require.True(t, got.ok, "a launch dir af could not resolve proves nothing against the id: the restore resumes it")
	require.Equal(t, "claude --resume "+carryTestID, got.program)
	require.False(t, got.decline.startsOnRecorded, "a look that did not happen never starts on the id")
	require.False(t, got.decline.retiresRecord(inst.AgentConversation()), "nothing is retired on a look that did not happen")
}

// A committed account swap's re-launch judges its id by the same rule. Where
// a newer conversation would send it to a fresh id, a wedged launch dir leaves
// it resuming the committed conversation, within the bound.
func TestCommittedSwapRelaunchDoesNotStallOnAWedgedLaunchDir(t *testing.T) {
	inst, target, worktree, committed := committedFreshClaudeSwapIn(t, exclusiveLayout)
	elsewhere := t.TempDir()
	fileClaudeTranscript(t, target, worktree, committed, time.Now().Add(-time.Hour))
	// A newer conversation recording a directory that answers, so the
	// ownership check reaches the launch side, filed beside the committed one.
	setModTime(t, writeCarryFile(t, target, claudeTranscriptRel(worktree, newerConversationID), claudeRecordLine(t, elsewhere)), time.Now().Add(-time.Minute))
	fallback := inst.ToInstanceData().PendingAccountSwap.CarryFallback
	wedgeLaunchDirResolution(t, worktree)

	plan := withinBound(t, func() *accountSwapLaunchPlan { return relaunchPlan(t, inst) })
	require.NotRegexp(t, sessionIDArg, plan.program, "a look that did not happen never starts a conversation: %s", plan.program)
	match := resumeArg.FindStringSubmatch(plan.program)
	require.NotNil(t, match, "the re-launch resumes the committed conversation: %s", plan.program)
	require.Equal(t, committed, match[1])
	pending := inst.ToInstanceData().PendingAccountSwap
	require.Equal(t, committed, pending.ConversationID, "the record keeps the committed conversation")
	require.Equal(t, fallback, pending.CarryFallback, "a resumed conversation leaves the record's account of it alone")
}

// lostInstanceInWedgeableWorktree is a Lost instance ready for Recover, with a
// mock tmux, and its worktree path.
func lostInstanceInWedgeableWorktree(t *testing.T, agentName string, newSessions *int) (*Instance, string) {
	t.Helper()
	log.Initialize(false)
	t.Cleanup(log.Close)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	exec := countingExec(map[string]bool{}, newSessions)
	inst := lostInstanceForRecover(t, agentName, agentName+tmuxTabSeparator+shellTabName, exec)
	return inst, inst.GetWorktreePath()
}

// requireBoundedWorktreeRefusal asserts Recover refused within the bound with
// the could-not-look shape: a WorktreeUnavailableError carrying the
// ErrProbeTimedOut + context.DeadlineExceeded pair the lost-restore loop
// retries as a bounded environmental wait, before any spawn, teardown or
// rebuild.
func requireBoundedWorktreeRefusal(t *testing.T, inst *Instance, newSessions *int) {
	t.Helper()
	err := withinBound(t, inst.Recover)
	var wtErr *WorktreeUnavailableError
	require.ErrorAs(t, err, &wtErr, "a worktree af could not look at refuses this attempt: %v", err)
	require.True(t, errors.Is(err, fsprobe.ErrProbeTimedOut), "the refusal names the bounded probe: %v", err)
	require.True(t, errors.Is(err, context.DeadlineExceeded), "and its deadline: %v", err)
	require.Zero(t, *newSessions, "no agent starts in a worktree af could not look at")
	require.False(t, inst.RebuildTeardownPending(), "a look that did not happen is never a missing worktree to rebuild")
	require.Equal(t, Lost, inst.GetStatus())
}

// The respawn checks its worktree is a directory before anything else touches
// it. On a wedged mount that check is a look that did not happen, never a
// missing worktree to rebuild.
func TestRecoverDoesNotStallOnAWedgedWorktree(t *testing.T) {
	var newSessions int
	inst, workDir := lostInstanceInWedgeableWorktree(t, "af_recover_wedged", &newSessions)
	wait := launchDirWedge(t, workDir)
	t.Cleanup(fsprobe.SetStatPathForTest(func(path string) (os.FileInfo, error) {
		wait(path)
		return os.Stat(path)
	}))

	requireBoundedWorktreeRefusal(t, inst, &newSessions)
}

// A row recorded worktree-missing first asks whether anything is at its path.
// On a wedged mount that is neither something there nor nothing there.
func TestRecoverOfAMissingWorktreeDoesNotStallOnAWedgedPath(t *testing.T) {
	var newSessions int
	inst, workDir := lostInstanceInWedgeableWorktree(t, "af_recover_missing_wedged", &newSessions)
	inst.SetWorktreeMissing(WorktreeMissingDeletedReason(workDir))
	wait := launchDirWedge(t, workDir)
	t.Cleanup(fsprobe.SetLstatForTest(func(path string) (os.FileInfo, error) {
		wait(path)
		return os.Lstat(path)
	}))

	requireBoundedWorktreeRefusal(t, inst, &newSessions)
	missing, _ := inst.WorktreeMissing()
	require.True(t, missing, "the record still says the worktree is missing")
}

// The account-swap carry asks the attribution rule too, and its safe answer is
// the opposite of the restore's. A directory af could not resolve counts
// against the carry, so a newer conversation refuses it rather than af carry a
// conversation the user may have moved on from (a /clear). Here the repository
// sits on the wedged mount, so af cannot tell whether the worktree is the
// user's own checkout.
func TestClaudeCarryRefusesANewerConversationWhenAttributionCannotLook(t *testing.T) {
	home := carryAmbientProviderHomes(t)
	inst, recorded, _ := symlinkedClaudeCarryInstance(t)
	recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
	source := filepath.Join(home, ".claude")
	cwd := inst.GetWorktreePath()
	past := time.Now().Add(-time.Hour)
	setModTime(t, writeCarryFile(t, source, filepath.Join(recorded, carryTestID+".jsonl"), claudeRecordLine(t, cwd)), past)
	setModTime(t, writeCarryFile(t, source, filepath.Join(recorded, newerConversationID+".jsonl"), claudeRecordLine(t, cwd)), past.Add(time.Minute))
	require.Equal(t, carryRefusalCode(staleCarryReason), inst.AccountSwapCarryRefusal(),
		"precondition: with every directory answering, the newer conversation refuses the carry")

	wedgeLaunchDirResolution(t, inst.Path)
	require.False(t, withinBound(t, func() bool { return inst.newerConversationIsEvidence(cwd) }),
		"the restore's answer: a directory af cannot prove is attributed nothing, so it resumes")
	require.Equal(t, carryRefusalCode(staleCarryReason), withinBound(t, inst.AccountSwapCarryRefusal),
		"the carry's answer: require_carry's check-ahead defers the move")
	require.NoError(t, withinBound(t, func() error { return inst.ValidateAccountSwap("work") }))
	if carry := inst.accountSwapLaunch.carry; carry != nil {
		t.Fatalf("a look that did not happen carried %s past a newer conversation", carry.transcript)
	}
	require.Equal(t, staleCarryReason, inst.accountSwapLaunch.carryFallback, "the swap states why it starts fresh")
}
