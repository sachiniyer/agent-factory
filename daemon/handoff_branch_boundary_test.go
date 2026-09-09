package daemon

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// The outgoing process commits after admission, immediately before teardown.
// A second commit at replacement launch must belong to the incoming identity.
type committingHandoffBackend struct {
	*handoffBackend
	beforeStop func()
	afterStart func()
}

func (b *committingHandoffBackend) SwapAgent(i *session.Instance, plan session.AgentSwapPlan) error {
	b.beforeStop()
	if err := b.handoffBackend.SwapAgent(i, plan); err != nil {
		return err
	}
	b.afterStart()
	return nil
}

func handoffBoundaryGit(t *testing.T, path string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", path}, args...)...).CombinedOutput()
	require.NoError(t, err, "%s", out)
	return strings.TrimSpace(string(out))
}
func handoffBoundaryWorktree(t *testing.T, inst *session.Instance) {
	t.Helper()
	base := handoffBoundaryGit(t, inst.Path, "rev-parse", "HEAD")
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", base, false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
}
func handoffBoundaryCommit(t *testing.T, inst *session.Instance, message string) string {
	t.Helper()
	handoffBoundaryGit(t, inst.Path, "-c", "user.name=Handoff Test", "-c", "user.email=handoff@example.com", "commit", "--allow-empty", "-m", message)
	return handoffBoundaryGit(t, inst.Path, "rev-parse", "HEAD")
}
func assertHandoffBoundary(t *testing.T, repo string, inst *session.Instance, resp HandoffSessionResponse, tip, mission, outgoing string) {
	t.Helper()
	require.Equal(t, tip, resp.HeadSHA, "the outgoing commit must be inside the handoff boundary")
	saved := persistedInstanceByTitle(t, repo, inst.Title)
	require.Len(t, saved.Tabs[0].Handoffs, 1)
	require.Equal(t, tip, saved.Tabs[0].Handoffs[0].HeadSHA)
	require.Equal(t, outgoing, saved.Tabs[0].Handoffs[0].From.Agent)
	require.Contains(t, mission, "1 commit, 0 uncommitted files", "the mission must include the final outgoing commit, but not replacement work")
	require.Contains(t, mission, "It was being done by "+outgoing)
}

func TestHandoffBranchBoundaryAfterStop(t *testing.T) {
	m, repo, path := newStatusTestManager(t)
	backend := &committingHandoffBackend{handoffBackend: &handoffBackend{FakeBackend: &session.FakeBackend{}}}
	inst := registerHandoffSubject(t, m, repo, path, "boundary", backend)
	handoffBoundaryWorktree(t, inst)
	var tip string
	backend.beforeStop = func() { tip = handoffBoundaryCommit(t, inst, "outgoing final commit") }
	backend.afterStart = func() { handoffBoundaryCommit(t, inst, "incoming first commit") }
	resp, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, To: "gemini"})
	require.NoError(t, err)
	require.Len(t, backend.sentPrompts, 1)
	assertHandoffBoundary(t, repo, inst, resp, tip, backend.sentPrompts[0], "claude")
}

func TestHandoffAccountBranchBoundaryAfterStop(t *testing.T) {
	m, repo, inst, backend := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	prepareHandoffTargetPreflight(t, inst)
	handoffBoundaryWorktree(t, inst)
	configureLimitAccountCandidate(t, m, "personal")
	inst.Program = "codex"
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, "codex"))
	inst.Account = "work"
	inst.ClearLimitReached()
	var tip string
	old := loadAccountLimitEvidenceForSwap
	t.Cleanup(func() { loadAccountLimitEvidenceForSwap = old })
	loadAccountLimitEvidenceForSwap = func() ([]session.AccountLimitObservationData, error) {
		evidence, err := old()
		if tip == "" {
			tip = handoffBoundaryCommit(t, inst, "outgoing final commit")
		}
		return evidence, err
	}
	backend.onRespawn = func(i *session.Instance) {
		saved := persistedInstanceByTitle(t, repo, i.Title)
		require.Equal(t, tip, saved.Tabs[0].Handoffs[0].HeadSHA)
		require.NotNil(t, saved.PendingAccountSwap)
		require.Contains(t, saved.PendingAccountSwap.Mission, "1 commit, 0 uncommitted files")
		handoffBoundaryCommit(t, i, "incoming first commit")
		i.SetTmuxSession(tmux.NewTmuxSession(i.Title, i.AgentProgram()))
	}
	resp, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, To: "claude", Account: "personal"})
	require.NoError(t, err)
	_, _, prompts := backend.snapshot()
	require.Len(t, prompts, 1)
	assertHandoffBoundary(t, repo, inst, resp, tip, prompts[0], "codex")
}
