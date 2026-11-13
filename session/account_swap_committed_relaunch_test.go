package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7214: a fresh (not carried) Claude account swap commits its replacement's
// conversation id P durably before the first launch, which starts Claude with
// --session-id P under the target account's store. When that replacement is
// then gone before the swap's pending state clears, the retry re-plans the
// replacement from the committed record. Once Claude has filed P's transcript
// in that store, a second --session-id P never boots ("Error: Session ID P is
// already in use.") and every retry re-plans the same id, so the row wedges.
// A re-launch must never start a new conversation on an id a transcript in the
// target account's store already holds: it resumes P, which is this session's
// own conversation, unless the account swap's staleness rule says the user has
// since moved on from it.

// committedFreshClaudeSwap admits and commits a fresh Claude swap to "work",
// then drops the in-memory launch plan the way a restart does. It returns the
// session, the target account's Claude store, the worktree Claude runs in,
// and the committed conversation id.
func committedFreshClaudeSwap(t *testing.T) (*Instance, string, string, string) {
	t.Helper()
	return committedFreshClaudeSwapIn(t, initTempGitRepo)
}

// committedFreshClaudeSwapIn is committedFreshClaudeSwap in the repository
// repo creates.
func committedFreshClaudeSwapIn(t *testing.T, repo func(*testing.T) string) (*Instance, string, string, string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: tmux.ProgramClaude}
	require.NoError(t, config.SaveConfig(cfg))
	target := registerAccount(t, tmux.ProgramClaude, "work")

	inst := accountSwapTestInstanceWithProgram(tmux.ProgramClaude)
	inst.Path = repo(t)
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)

	require.NoError(t, inst.ValidateAccountSwap("work"))
	_, err = inst.SelectAccountAutomatically("", "work")
	require.NoError(t, err)
	pending := inst.ToInstanceData().PendingAccountSwap
	require.NotNil(t, pending)
	require.NotEmpty(t, pending.ConversationID, "precondition: a fresh swap commits its replacement's conversation id")
	require.Empty(t, pending.CarriedConversationID, "precondition: the swap is fresh, not carried")

	// The restart's view: only the durable record survives.
	inst.accountSwapLaunch = nil
	return inst, target, inst.GetWorktreePath(), pending.ConversationID
}

// fileClaudeTranscript files conversation id under worktree's project
// directory in the Claude store configDir, last written at modTime.
func fileClaudeTranscript(t *testing.T, configDir, worktree, id string, modTime time.Time) {
	t.Helper()
	setModTime(t, writeCarryFile(t, configDir, claudeTranscriptRel(worktree, id), "{}\n"), modTime)
}

var resumeArg = regexp.MustCompile(`--resume[ =](\S+)`)

// relaunchPlan re-plans the gone replacement and returns the plan it recorded.
func relaunchPlan(t *testing.T, inst *Instance) *accountSwapLaunchPlan {
	t.Helper()
	require.NoError(t, inst.ValidateAccountSwap("work"), "the committed retry re-plans its replacement")
	require.NotNil(t, inst.accountSwapLaunch)
	return cloneAccountSwapLaunchPlan(inst.accountSwapLaunch)
}

func TestCommittedSwapRelaunchResumesItsConversationOnceATranscriptHoldsIt(t *testing.T) {
	inst, target, worktree, committed := committedFreshClaudeSwap(t)
	// The replacement launched and Claude filed the committed conversation.
	fileClaudeTranscript(t, target, worktree, committed, time.Now().Add(-time.Minute))

	plan := relaunchPlan(t, inst)
	require.NotRegexp(t, sessionIDArg, plan.program,
		"a re-launch must not start a new conversation on an id the target store already holds")
	match := resumeArg.FindStringSubmatch(plan.program)
	require.NotNil(t, match, "the re-launch resumes the session's own conversation: %s", plan.program)
	require.Equal(t, committed, match[1])
	require.Equal(t, committed, plan.conversation.ID, "the record names the conversation that runs")
	require.Equal(t, tmux.ProgramClaude, plan.conversation.Agent)
	require.Equal(t, committed, inst.ToInstanceData().PendingAccountSwap.ConversationID)
}

// Control: while nothing is filed under the committed id, the re-launch starts
// on it exactly as the first launch did.
func TestCommittedSwapRelaunchStartsOnItsIDWhileNoTranscriptHoldsIt(t *testing.T) {
	inst, _, _, committed := committedFreshClaudeSwap(t)

	plan := relaunchPlan(t, inst)
	require.NotRegexp(t, resumeArg, plan.program, "nothing to resume: %s", plan.program)
	match := sessionIDArg.FindStringSubmatch(plan.program)
	require.NotNil(t, match, plan.program)
	require.Equal(t, committed, match[1])
	require.Equal(t, committed, plan.conversation.ID)
	require.Equal(t, committed, inst.ToInstanceData().PendingAccountSwap.ConversationID)
}

// A newer conversation in the worktree means the user moved on from the
// committed one (a /clear in the replacement): resuming it would continue a
// thread the user left, and starting on its id never boots. The re-launch
// starts fresh on an id of its own, and the record names that id.
func TestCommittedSwapRelaunchStartsFreshWhenItsConversationIsStale(t *testing.T) {
	inst, target, worktree, committed := committedFreshClaudeSwap(t)
	const newer = "7c1d2e3f-4a5b-4c6d-8e7f-901234567890"
	fileClaudeTranscript(t, target, worktree, committed, time.Now().Add(-time.Hour))
	fileClaudeTranscript(t, target, worktree, newer, time.Now().Add(-time.Minute))

	plan := relaunchPlan(t, inst)
	require.NotRegexp(t, resumeArg, plan.program, "a stale conversation is not resumed: %s", plan.program)
	match := sessionIDArg.FindStringSubmatch(plan.program)
	require.NotNil(t, match, "the re-launch starts a new conversation: %s", plan.program)
	require.NotEqual(t, committed, match[1], "a new conversation must not re-use an id a transcript already holds")
	require.NotEqual(t, newer, match[1])
	require.Equal(t, match[1], plan.conversation.ID)
	require.Equal(t, match[1], inst.ToInstanceData().PendingAccountSwap.ConversationID,
		"the committed record must name the conversation the re-launch starts")
}

// Claude may file the conversation under the kernel's spelling of a worktree
// reached through a symlinked parent (#7177). Reading only the recorded
// spelling would call the conversation unfiled and start on its id again.
func TestCommittedSwapRelaunchFindsItsConversationUnderEitherSpelling(t *testing.T) {
	var real string
	inst, target, worktree, committed := committedFreshClaudeSwapIn(t, func(t *testing.T) string {
		real = filepath.Join(testguard.CanonicalTempDir(t), "real", "repo")
		require.NoError(t, os.MkdirAll(real, 0o755))
		cmd := exec.Command("git", "init")
		cmd.Dir = real
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		link := filepath.Join(testguard.CanonicalTempDir(t), "link")
		require.NoError(t, os.Symlink(filepath.Dir(real), link))
		return filepath.Join(link, "repo")
	})
	require.NotEqual(t, real, worktree, "precondition: the worktree is recorded through the symlink")
	fileClaudeTranscript(t, target, real, committed, time.Now().Add(-time.Minute))

	plan := relaunchPlan(t, inst)
	require.NotRegexp(t, sessionIDArg, plan.program, plan.program)
	match := resumeArg.FindStringSubmatch(plan.program)
	require.NotNil(t, match, plan.program)
	require.Equal(t, committed, match[1])
}
