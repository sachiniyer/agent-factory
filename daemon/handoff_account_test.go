package daemon

import (
	"encoding/json"
	"errors"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHandoffAccountMovesPinnedIdentity(t *testing.T) {
	for _, to := range []string{"", "claude"} {
		t.Run("to="+to, func(t *testing.T) {
			m, repo, inst, backend := newAutoResumeManager(t, "", true, "finish the migration", time.Now().Add(5*24*time.Hour))
			configureLimitAccountCandidate(t, m, "personal")
			inst.Account = "work"
			inst.ClearLimitReached()
			inst.SetLimitReached(time.Now().Add(5 * 24 * time.Hour))
			gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
			require.NoError(t, err)
			inst.SetGitWorktreeForTest(gw)
			head, err := exec.Command("git", "-C", inst.Path, "rev-parse", "HEAD").Output()
			require.NoError(t, err)
			m.cfg.LimitAutoResume = false
			var req HandoffSessionRequest
			require.NoError(t, json.Unmarshal([]byte(`{"account":"personal"}`), &req))
			req.Title, req.RepoID, req.To = inst.Title, repo, to
			resp, err := m.HandoffSession(req)
			require.NoError(t, err)
			require.True(t, resp.OK)
			account, automatic := inst.AccountSelection()
			require.Equal(t, "personal", account)
			require.False(t, automatic)
			require.False(t, inst.LimitReached())
			_, respawns, prompts := backend.snapshot()
			require.Equal(t, 1, respawns)
			require.Contains(t, prompts[0], "finish the migration")
			data := inst.ToInstanceData()
			require.Len(t, data.Tabs[0].Handoffs, 1)
			require.Equal(t, "work", data.Tabs[0].Handoffs[0].FromAccount)
			require.Equal(t, "personal", data.Tabs[0].Handoffs[0].ToAccount)
			require.Equal(t, strings.TrimSpace(string(head)), resp.HeadSHA)
			after, err := exec.Command("git", "-C", inst.Path, "rev-parse", "HEAD").Output()
			require.NoError(t, err)
			require.Equal(t, head, after)
			saved := persistedInstanceByTitle(t, repo, inst.Title)
			require.Len(t, saved.Tabs[0].Handoffs, 1)
			require.Equal(t, resp.HeadSHA, saved.Tabs[0].Handoffs[0].HeadSHA)
			require.Equal(t, "personal", saved.Tabs[0].Handoffs[0].ToAccount)
		})
	}
}

func TestHandoffAccountRefusesUnknownTarget(t *testing.T) {
	m, repo, inst, _ := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	var req HandoffSessionRequest
	require.NoError(t, json.Unmarshal([]byte(`{"account":"missing"}`), &req))
	req.Title, req.RepoID = inst.Title, repo
	_, err := m.HandoffSession(req)
	require.ErrorContains(t, err, `account "missing" is not registered for claude`)
}

func TestHandoffAccountRefusesWalledTarget(t *testing.T) {
	m, repo, inst, backend := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	inst.Account = "personal"
	inst.ClearLimitReached()
	inst.SetLimitReached(time.Now().Add(time.Hour))
	inst.Account = "work"
	_, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
	require.ErrorContains(t, err, `claude account "personal" is currently at its usage limit`)
	_, respawns, _ := backend.snapshot()
	require.Zero(t, respawns)
	require.Equal(t, "work", inst.Account)
}

func TestHandoffAccountCombinesNewAgentAndAccount(t *testing.T) {
	m, repo, inst, backend := newAutoResumeManager(t, "", true, "old brief", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	inst.Program = "codex"
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, "codex"))
	inst.Account = "work"
	inst.ClearLimitReached() // A manual handoff also works before a usage limit.
	backend.onRespawn = func(i *session.Instance) { i.SetTmuxSession(tmux.NewTmuxSession(i.Title, i.AgentProgram())) }
	resp, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, To: "claude", Account: "personal", Brief: "new brief"})
	require.NoError(t, err)
	require.Equal(t, "codex", resp.From)
	require.Equal(t, "claude", resp.To)
	require.Equal(t, "personal", inst.Account)
	require.Equal(t, "new brief", inst.GetPrompt())
}

func TestHandoffAccountRecoversPinnedDelivery(t *testing.T) {
	m, repo, inst, backend := newAutoResumeManager(t, "", true, "finish migration", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	inst.Account = "work"
	m.cfg.LimitAutoResume = false
	backend.sendPromptErr = errors.New("delivery interrupted")
	_, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
	require.ErrorContains(t, err, "delivery interrupted")
	saved := persistedInstanceByTitle(t, repo, inst.Title)
	require.True(t, saved.PendingAccountSwap.Manual)
	require.Contains(t, saved.PendingAccountSwap.Mission, "finish migration")
	backend.sendPromptErr = nil
	require.NoError(t, m.resumeFromLimit(ResumeFromLimitRequest{Title: inst.Title, RepoID: repo}))
	_, _, pending := inst.PendingAccountSwap()
	require.False(t, pending)
	name, automatic := inst.AccountSelection()
	require.Equal(t, "personal", name)
	require.False(t, automatic)
	require.Len(t, inst.ToInstanceData().Tabs[0].Handoffs, 1)
}

func TestHandoffAccountRecoversHealthyCheckpoint(t *testing.T) {
	m, repo, inst, backend := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	inst.Account = "work"
	inst.ClearLimitReached()
	m.cfg.LimitAutoResume = false
	require.NoError(t, inst.BeginManualAccountSwap())
	require.NoError(t, inst.ValidateManualAccountSwap("personal", "claude"))
	_, err := inst.SelectAccountForHandoff("work", "personal", "claude", session.HandoffReasonManual, "tip", "continue")
	require.NoError(t, err)
	require.NoError(t, m.persistSettlement(repo, daemonInstanceKey(repo, inst.Title), inst))
	inst.EndLimitResume()
	require.False(t, inst.LimitReached(), "a pending transaction is not quota evidence")
	m.ResumeLimitedSessions()
	_, respawns, prompts := backend.snapshot()
	require.Equal(t, 1, respawns)
	require.Len(t, prompts, 1)
	_, _, pending := inst.PendingAccountSwap()
	require.False(t, pending)
}

func TestHandoffAccountKeepsLimitInOutgoingAgentNamespace(t *testing.T) {
	m, repo, inst, backend := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	inst.Program = "codex"
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, "codex"))
	inst.Account = "work"
	inst.ClearLimitReached()
	inst.SetLimitReached(time.Now().Add(time.Hour))
	backend.onRespawn = func(i *session.Instance) { i.SetTmuxSession(tmux.NewTmuxSession(i.Title, i.AgentProgram())) }
	backend.sendPromptErr = errors.New("hold pending transaction")
	_, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, To: "claude", Account: "personal"})
	require.ErrorContains(t, err, "hold pending transaction")
	limited, err := m.limitedAccountsForSwap("claude", loadAccountLimitEvidenceForSwap)
	require.NoError(t, err)
	require.NotContains(t, limited, "work")
	limited, err = m.limitedAccountsForSwap("codex", loadAccountLimitEvidenceForSwap)
	require.NoError(t, err)
	require.Contains(t, limited, "work")
}
