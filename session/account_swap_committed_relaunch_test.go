package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/fsprobe"
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

// swapLayout is where a committed swap's session works: the repository, the
// worktree Claude runs in, and the flags that decide whether af owns that
// worktree exclusively (newerConversationIsEvidence).
type swapLayout struct {
	repo, worktree    string
	external, inPlace bool
}

// checkoutLayout is a worktree at the repository's own path: the user's
// checkout, whatever the record's flags say.
func checkoutLayout(t *testing.T) swapLayout {
	t.Helper()
	repo := initTempGitRepo(t)
	return swapLayout{repo: repo, worktree: repo}
}

// exclusiveLayout is a worktree af created for this session alone.
func exclusiveLayout(t *testing.T) swapLayout {
	t.Helper()
	return swapLayout{repo: initTempGitRepo(t), worktree: t.TempDir()}
}

// committedFreshClaudeSwap admits and commits a fresh Claude swap to "work",
// then drops the in-memory launch plan the way a restart does. It returns the
// session, the target account's Claude store, the worktree Claude runs in,
// and the committed conversation id.
func committedFreshClaudeSwap(t *testing.T) (*Instance, string, string, string) {
	t.Helper()
	return committedFreshClaudeSwapIn(t, checkoutLayout)
}

// committedFreshClaudeSwapIn is committedFreshClaudeSwap in the layout build
// creates.
func committedFreshClaudeSwapIn(t *testing.T, build func(*testing.T) swapLayout) (*Instance, string, string, string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: tmux.ProgramClaude}
	require.NoError(t, config.SaveConfig(cfg))
	target := registerAccount(t, tmux.ProgramClaude, "work")

	layout := build(t)
	inst := accountSwapTestInstanceWithProgram(tmux.ProgramClaude)
	inst.Path = layout.repo
	inst.inPlace = layout.inPlace
	gw, err := sessiongit.NewGitWorktreeFromStorage(layout.repo, layout.worktree, inst.Title, "main", "", layout.external, true)
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
// directory in the Claude store configDir, last written at modTime. Its
// record names worktree as the directory it was launched in, as Claude's
// does: ownership is judged by that cwd (#7208).
func fileClaudeTranscript(t *testing.T, configDir, worktree, id string, modTime time.Time) {
	t.Helper()
	setModTime(t, writeCarryFile(t, configDir, claudeTranscriptRel(worktree, id), claudeRecordLine(t, worktree)), modTime)
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

// In a worktree af created for this session alone, a newer conversation is
// evidence the user moved on from the committed one (a /clear in the
// replacement): resuming it would continue a thread the user left, and starting
// on its id never boots. The re-launch starts fresh on an id of its own, the
// record names that id, and the replacement is told why.
func TestCommittedSwapRelaunchStartsFreshWhenItsConversationIsStale(t *testing.T) {
	inst, target, worktree, committed := committedFreshClaudeSwapIn(t, exclusiveLayout)
	require.True(t, inst.newerConversationIsEvidence(worktree), "precondition: af owns this worktree exclusively")
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
	pending := inst.ToInstanceData().PendingAccountSwap
	require.Equal(t, match[1], pending.ConversationID,
		"the committed record must name the conversation the re-launch starts")
	require.Equal(t, staleCommittedConversationReason, pending.CarryFallback, "the record says why it starts fresh")
	require.Equal(t, staleCommittedConversationReason, inst.PendingAccountSwapConversation().CarryFailure,
		"the swap's answer and notice report why the replacement starts fresh")
}

// Anywhere af does not own exclusively, a newer conversation beside the
// committed one says nothing about this session: a sibling on the same account
// (the root, a --here row, an adopted worktree, claude in a scoped shell tab)
// files there too. The re-launch resumes the session's own conversation, as a
// restore would (#7216 review R1).
func TestCommittedSwapRelaunchResumesBesideANewerConversationItDoesNotOwn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		layout func(*testing.T) swapLayout
	}{
		{"checkout", checkoutLayout},
		{"in place", func(t *testing.T) swapLayout {
			l := checkoutLayout(t)
			l.inPlace = true
			return l
		}},
		{"adopted external worktree", func(t *testing.T) swapLayout {
			l := exclusiveLayout(t)
			l.external = true
			return l
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst, target, worktree, committed := committedFreshClaudeSwapIn(t, tc.layout)
			require.False(t, inst.newerConversationIsEvidence(worktree), "precondition: af does not own this worktree exclusively")
			fileClaudeTranscript(t, target, worktree, committed, time.Now().Add(-time.Hour))
			fileClaudeTranscript(t, target, worktree, "7c1d2e3f-4a5b-4c6d-8e7f-901234567890", time.Now().Add(-time.Minute))
			fallback := inst.ToInstanceData().PendingAccountSwap.CarryFallback

			plan := relaunchPlan(t, inst)
			require.NotRegexp(t, sessionIDArg, plan.program, plan.program)
			match := resumeArg.FindStringSubmatch(plan.program)
			require.NotNil(t, match, "a sibling's newer conversation must not abandon this session's own: %s", plan.program)
			require.Equal(t, committed, match[1])
			require.Equal(t, committed, plan.conversation.ID)
			pending := inst.ToInstanceData().PendingAccountSwap
			require.Equal(t, committed, pending.ConversationID)
			require.Equal(t, fallback, pending.CarryFallback, "a resumed conversation leaves the record's account of it alone")
		})
	}
}

// Claude may file the conversation under the kernel's spelling of a worktree
// reached through a symlinked parent (#7177). Reading only the recorded
// spelling would call the conversation unfiled and start on its id again.
func TestCommittedSwapRelaunchFindsItsConversationUnderEitherSpelling(t *testing.T) {
	var real string
	inst, target, worktree, committed := committedFreshClaudeSwapIn(t, func(t *testing.T) swapLayout {
		real = filepath.Join(testguard.CanonicalTempDir(t), "real", "repo")
		require.NoError(t, os.MkdirAll(real, 0o755))
		cmd := exec.Command("git", "init")
		cmd.Dir = real
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		link := filepath.Join(testguard.CanonicalTempDir(t), "link")
		require.NoError(t, os.Symlink(filepath.Dir(real), link))
		repo := filepath.Join(link, "repo")
		return swapLayout{repo: repo, worktree: repo}
	})
	require.NotEqual(t, real, worktree, "precondition: the worktree is recorded through the symlink")
	fileClaudeTranscript(t, target, real, committed, time.Now().Add(-time.Minute))

	plan := relaunchPlan(t, inst)
	require.NotRegexp(t, sessionIDArg, plan.program, plan.program)
	match := resumeArg.FindStringSubmatch(plan.program)
	require.NotNil(t, match, plan.program)
	require.Equal(t, committed, match[1])
}

// #7208: a newer conversation that names no directory af can read is no
// evidence the session moved on, even in a worktree af owns. The committed
// conversation is filed, so starting on its id again would never boot; the
// re-launch resumes it.
func TestCommittedSwapRelaunchResumesBesideAnUnattributableConversation(t *testing.T) {
	inst, target, worktree, committed := committedFreshClaudeSwapIn(t, exclusiveLayout)
	require.True(t, inst.newerConversationIsEvidence(worktree), "precondition: af owns this worktree exclusively")
	const unnamed = "7c1d2e3f-4a5b-4c6d-8e7f-901234567890"
	fileClaudeTranscript(t, target, worktree, committed, time.Now().Add(-time.Hour))
	setModTime(t, writeCarryFile(t, target, claudeTranscriptRel(worktree, unnamed), `{"type":"summary","summary":"s"}`+"\n"),
		time.Now().Add(-time.Minute))

	plan := relaunchPlan(t, inst)
	match := resumeArg.FindStringSubmatch(plan.program)
	require.NotNil(t, match, "a filed conversation is resumed: %s", plan.program)
	require.Equal(t, committed, match[1])
	require.NotRegexp(t, sessionIDArg, plan.program, "--session-id on a filed id never boots")
}

// #7596: a committed relaunch whose transcript-store verdict is blind never
// plans a command that cannot boot on the id the record says the first launch
// filed. A second --session-id on a filed id never boots; --resume exits only
// where nothing was filed. A store af can name but not list is judged again —
// a still-wedged listing's latched flight answers the retry at once, inside
// the bound the first judge already spent — and where the listing stays blind
// the record's say-so decides: ReplacementPanesStarted means the first launch
// ran on --session-id and filed the id, so --resume is the blind answer that
// boots, and the record and the notice name it.
func TestCommittedSwapRelaunchResumesBlindOnTheRecordsSaySo(t *testing.T) {
	for _, filed := range []bool{true, false} {
		name := "transcript filed"
		if !filed {
			name = "transcript absent"
		}
		t.Run(name, func(t *testing.T) {
			inst, target, worktree, committed := committedFreshClaudeSwapIn(t, exclusiveLayout)
			if filed {
				fileClaudeTranscript(t, target, worktree, committed, time.Now().Add(-time.Minute))
			}
			inst.SetAccountSwapReplacementPanesStartedForTest(true)
			wedgeStoreListing(t, target)

			plan := withinBound(t, func() *accountSwapLaunchPlan { return relaunchPlan(t, inst) })
			require.NotRegexp(t, sessionIDArg, plan.program,
				"--session-id never boots on the id the record says the first launch filed: %s", plan.program)
			match := resumeArg.FindStringSubmatch(plan.program)
			require.NotNil(t, match, "the record's say-so resumes the committed conversation: %s", plan.program)
			require.Equal(t, committed, match[1])
			require.Equal(t, committed, plan.conversation.ID)
			pending := inst.ToInstanceData().PendingAccountSwap
			require.Equal(t, committed, pending.ConversationID,
				"the record keeps the committed conversation the re-launch resumes")
			require.Equal(t, blindCommittedResumeReason, pending.CarryFallback,
				"the record says why the re-launch resumed without a listing's proof")
			require.Equal(t, blindCommittedResumeReason, inst.PendingAccountSwapConversation().CarryFailure,
				"the swap's answer and the notice report it")
		})
	}
}

// A nameable store whose listing did not answer is judged again inside the
// same look (#7596): a listing that answers on the retry is a look that
// happened, and the filed conversation resumes by its proof, not the record's
// say-so — so no blind reason reaches the record.
func TestCommittedSwapRelaunchJudgesAgainAfterTheListingAnswers(t *testing.T) {
	inst, target, worktree, committed := committedFreshClaudeSwapIn(t, exclusiveLayout)
	fileClaudeTranscript(t, target, worktree, committed, time.Now().Add(-time.Minute))
	inst.SetAccountSwapReplacementPanesStartedForTest(true)
	fallback := inst.ToInstanceData().PendingAccountSwap.CarryFallback
	var failed sync.Map // each project dir's first listing refuses, as a blip does
	t.Cleanup(fsprobe.SetReadDirForTest(func(dir string) ([]os.DirEntry, error) {
		if strings.HasPrefix(dir, target) {
			if _, seen := failed.LoadOrStore(dir, struct{}{}); !seen {
				return nil, os.ErrPermission
			}
		}
		return os.ReadDir(dir)
	}))

	plan := relaunchPlan(t, inst)
	require.NotRegexp(t, sessionIDArg, plan.program, plan.program)
	match := resumeArg.FindStringSubmatch(plan.program)
	require.NotNil(t, match, "the retried listing reads the filed transcript: %s", plan.program)
	require.Equal(t, committed, match[1])
	pending := inst.ToInstanceData().PendingAccountSwap
	require.Equal(t, committed, pending.ConversationID)
	require.Equal(t, fallback, pending.CarryFallback,
		"a listing that answered leaves the record's account of it alone: the resume is proved")
}

// A record that shows the first launch never ran describes no filing (#7596):
// starting on the committed id boots, exactly as the first launch would have —
// the one case a blind verdict may still start on it.
func TestCommittedSwapRelaunchStartsOnItsIDWhenNoFirstLaunchRan(t *testing.T) {
	inst, target, _, committed := committedFreshClaudeSwapIn(t, exclusiveLayout)
	require.False(t, inst.ToInstanceData().PendingAccountSwap.ReplacementPanesStarted,
		"precondition: the committed record shows the panes never started")
	wedgeStoreListing(t, target)

	plan := withinBound(t, func() *accountSwapLaunchPlan { return relaunchPlan(t, inst) })
	require.NotRegexp(t, resumeArg, plan.program,
		"nothing is filed where nothing launched: %s", plan.program)
	match := sessionIDArg.FindStringSubmatch(plan.program)
	require.NotNil(t, match, "as the first launch did: %s", plan.program)
	require.Equal(t, committed, match[1])
	require.NotEqual(t, blindCommittedResumeReason,
		inst.ToInstanceData().PendingAccountSwap.CarryFallback,
		"a start on the committed id is no blind resume")
}

// The started-panes proof binds the id the panes started on, not the record's
// current one (#7596 review): a committed id reminted for staleness after the
// first launch has never itself launched — a later blind retry must not
// --resume it, for nothing of it is filed; it starts on its id, where a real
// first launch would file it.
func TestCommittedSwapRelaunchStartsOnAnIDRemintedAfterTheLaunch(t *testing.T) {
	inst, target, worktree, committed := committedFreshClaudeSwapIn(t, exclusiveLayout)
	inst.SetAccountSwapReplacementPanesStartedForTest(true)
	fileClaudeTranscript(t, target, worktree, committed, time.Now().Add(-time.Hour))
	fileClaudeTranscript(t, target, worktree, newerConversationID, time.Now().Add(-time.Minute))

	first := relaunchPlan(t, inst)
	reminted := sessionIDArg.FindStringSubmatch(first.program)
	require.NotNil(t, reminted, "a stale conversation remints: %s", first.program)
	require.NotEqual(t, committed, reminted[1])
	pending := inst.ToInstanceData().PendingAccountSwap
	require.Equal(t, reminted[1], pending.ConversationID)
	require.Equal(t, committed, pending.LaunchedConversationID,
		"precondition: the launch proof still names the id the panes started on")

	wedgeStoreListing(t, target)
	plan := withinBound(t, func() *accountSwapLaunchPlan { return relaunchPlan(t, inst) })
	require.NotRegexp(t, resumeArg, plan.program,
		"nothing of the reminted id was ever filed: --resume exits at launch (%s)", plan.program)
	match := sessionIDArg.FindStringSubmatch(plan.program)
	require.NotNil(t, match, "as a first launch of it does: %s", plan.program)
	require.Equal(t, reminted[1], match[1])
	require.NotEqual(t, blindCommittedResumeReason,
		inst.ToInstanceData().PendingAccountSwap.CarryFallback,
		"a start on a never-launched id is no blind resume")
}

// A store af cannot even name says nothing about the id: the re-launch starts
// on it, as the first launch did (#7596). The committed record's target is
// registered while the record lives, so the seam is reached the way a
// deregistered target leaves it — the record names an account the registry no
// longer holds.
func TestCommittedSwapRelaunchStartsOnItsIDWhenTheStoreCannotBeNamed(t *testing.T) {
	inst, _, worktree, committed := committedFreshClaudeSwap(t)
	inst.Account = "ghost"
	inst.pendingAccountSwap.To = "ghost"
	inst.SetAccountSwapReplacementPanesStartedForTest(true)

	launch, conversation, reminted, blindResumed := inst.committedConversationLaunch("claude", worktree, "ghost", committed)
	require.False(t, reminted)
	require.False(t, blindResumed, "a store af cannot name is no blind resume: nothing is known of the id")
	require.Equal(t, committed, conversation.ID)
	match := sessionIDArg.FindStringSubmatch(launch)
	require.NotNil(t, match, "as the first launch did: %s", launch)
	require.Equal(t, committed, match[1])
}
