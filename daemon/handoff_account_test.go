package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// newAutoResumeRootManager is newAutoResumeManager's fixture built on the
// reserved root title: a local backend, a live claude agent tab, and a parked
// usage limit — the #4395 scenario, where the root agent's account is exhausted
// and the only documented recovery used to be a kill that discarded its
// conversation.
func newAutoResumeRootManager(t *testing.T, alive bool, prompt string, resetAt time.Time) (*Manager, string, *session.Instance, *limitResumeBackend) {
	t.Helper()
	shimDir := t.TempDir()
	shim := filepath.Join(shimDir, tmux.ProgramClaude)
	if err := os.WriteFile(shim, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	manager, repoID, repoPath := newStatusTestManager(t)
	manager.cfg.LimitAutoResume = false
	backend := &limitResumeBackend{FakeBackend: session.NewFakeBackend(), alive: alive}
	inst := registerStarted(t, manager, repoID, repoPath, session.RootSessionTitle, backend, true, session.Running)
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, tmux.ProgramClaude))
	inst.Prompt = prompt
	inst.SetLimitReached(resetAt)
	return manager, repoID, inst, backend
}

// TestHandoffAccountMovesTheReservedRoot is the #4395 fix: `af sessions handoff
// root --account <other>` succeeds because an account-only handoff moves which
// identity the same agent authenticates as — root stays the reserved singleton
// on the same worktree and branch, with its agent program untouched.
func TestHandoffAccountMovesTheReservedRoot(t *testing.T) {
	for _, to := range []string{"", "claude"} {
		t.Run("to="+to, func(t *testing.T) {
			m, repo, inst, backend := newAutoResumeRootManager(t, true, "keep the fleet healthy", time.Now().Add(5*24*time.Hour))
			configureLimitAccountCandidate(t, m, "personal")
			inst.Account = "work"
			gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
			require.NoError(t, err)
			inst.SetGitWorktreeForTest(gw)
			head, err := exec.Command("git", "-C", inst.Path, "rev-parse", "HEAD").Output()
			require.NoError(t, err)

			resp, err := m.HandoffSession(HandoffSessionRequest{
				Title: session.RootSessionTitle, RepoID: repo, To: to, Account: "personal",
			})
			require.NoError(t, err)
			require.True(t, resp.OK)

			account, automatic := inst.AccountSelection()
			require.Equal(t, "personal", account)
			require.False(t, automatic)
			require.False(t, inst.LimitReached(), "the delivered account move clears the outgoing identity's wall")
			require.Equal(t, tmux.ProgramClaude, inst.AgentProgram(),
				"an account move must not change which agent root runs")
			require.Equal(t, inst.Path, inst.GetWorktreePath(),
				"an account move must not relocate root's worktree")
			_, respawns, prompts := backend.snapshot()
			require.Equal(t, 1, respawns)
			require.Len(t, prompts, 1)
			require.Contains(t, prompts[0], `Handed off from claude account "work" to claude account "personal".`)
			require.Equal(t, strings.TrimSpace(string(head)), resp.HeadSHA)
			after, err := exec.Command("git", "-C", inst.Path, "rev-parse", "HEAD").Output()
			require.NoError(t, err)
			require.Equal(t, head, after, "the branch tip must not move")
			saved := persistedInstanceByTitle(t, repo, inst.Title)
			require.True(t, session.IsReservedTitle(saved.Title))
			require.Len(t, saved.Tabs[0].Handoffs, 1)
			require.Equal(t, "work", saved.Tabs[0].Handoffs[0].FromAccount)
			require.Equal(t, "personal", saved.Tabs[0].Handoffs[0].ToAccount)
		})
	}
}

// TestHandoffAccountRefusesToMoveTheReservedRootToAnotherAgent is the boundary
// #4395 does not relax: --to a different agent changes what root IS, so it is
// refused even when --account accompanies it — the request must leave the
// session, its account, and its ledger untouched.
func TestHandoffAccountRefusesToMoveTheReservedRootToAnotherAgent(t *testing.T) {
	m, repo, inst, backend := newAutoResumeRootManager(t, true, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	inst.Account = "work"
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)

	_, err = m.HandoffSession(HandoffSessionRequest{
		Title: session.RootSessionTitle, RepoID: repo, To: tmux.ProgramCodex, Account: "personal",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "root agent")
	require.Equal(t, "work", inst.Account)
	require.Equal(t, tmux.ProgramClaude, inst.AgentProgram())
	require.Empty(t, inst.Handoffs())
	_, respawns, prompts := backend.snapshot()
	require.Zero(t, respawns)
	require.Empty(t, prompts)
}

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
	prepareHandoffTargetPreflight(t, inst)
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
	_, _, prompts := backend.snapshot()
	require.Len(t, prompts, 1)
	require.Contains(t, prompts[0], `Handed off from codex account "work" to claude account "personal".`)
}

func TestHandoffAccountPromptNamesAmbientAndPinnedIdentities(t *testing.T) {
	for _, tc := range []struct {
		name        string
		fromAccount string
		want        string
	}{
		{name: "ambient", want: `Handed off from the ambient claude identity to claude account "personal".`},
		{name: "pinned", fromAccount: "work", want: `Handed off from claude account "work" to claude account "personal".`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, repo, inst, backend := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
			configureLimitAccountCandidate(t, m, "personal")
			inst.Account = tc.fromAccount
			inst.ClearLimitReached()

			_, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
			require.NoError(t, err)
			_, _, prompts := backend.snapshot()
			require.Len(t, prompts, 1)
			require.Contains(t, prompts[0], tc.want)
			require.NotContains(t, prompts[0], `account ""`)
		})
	}
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
	_, err = m.resumeFromLimitOutcome(ResumeFromLimitRequest{Title: inst.Title, RepoID: repo})
	require.NoError(t, err)
	_, _, pending := inst.PendingAccountSwap()
	require.False(t, pending)
	name, automatic := inst.AccountSelection()
	require.Equal(t, "personal", name)
	require.False(t, automatic)
	require.Len(t, inst.ToInstanceData().Tabs[0].Handoffs, 1)
}

// TestResumeLimitedSessionsFinishesCommittedRootAccountSwap is the #4395
// settlement guarantee on the reserved title: once the handoff path checkpoints
// root's new identity, its durable mission belongs to the scheduler — the
// reserved-title refusal protects root's lifecycle from the scheduler, not a
// transaction the scheduler is designated to finish. A bare limit park on root
// stays refused either way.
func TestResumeLimitedSessionsFinishesCommittedRootAccountSwap(t *testing.T) {
	m, repo, inst, backend := newAutoResumeRootManager(t, true, "keep the fleet healthy", time.Now().Add(5*24*time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	inst.Account = "work"
	inst.ClearLimitReached()
	m.cfg.LimitAutoResume = false
	require.NoError(t, inst.BeginManualAccountSwap())
	require.NoError(t, inst.ValidateManualAccountSwap("personal", "claude"))
	_, err := inst.SelectAccountForHandoff("work", "personal", "claude", session.HandoffReasonManual, "tip", "keep the fleet healthy")
	require.NoError(t, err)
	require.NoError(t, m.persistSettlement(repo, daemonInstanceKey(repo, inst.Title), inst))
	inst.EndLimitResume()

	m.ResumeLimitedSessions()

	_, respawns, prompts := backend.snapshot()
	require.Equal(t, 1, respawns)
	require.Len(t, prompts, 1)
	require.Contains(t, prompts[0], "keep the fleet healthy")
	_, _, pending := inst.PendingAccountSwap()
	require.False(t, pending, "the scheduler must settle the committed transaction, not leave it pending")
}

// TestHandoffAccountRetriesCommittedSwapToSameTarget is the #4393 escape
// hatch: a handoff that committed its account move but never delivered its
// mission leaves pending_account_swap fencing the row — and the refusal's own
// remedy, retrying that swap, was itself a refused lifecycle action. A handoff
// naming the swap's committed account must route to the same committed
// recovery the limit-retry door takes: re-deliver the stored mission and clear
// the marker.
func TestHandoffAccountRetriesCommittedSwapToSameTarget(t *testing.T) {
	m, repo, inst, backend := newAutoResumeManager(t, "", true, "finish migration", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	inst.Account = "work"
	m.cfg.LimitAutoResume = false
	backend.sendPromptErr = errors.New("delivery interrupted")
	_, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
	require.ErrorContains(t, err, "delivery interrupted")
	from, to, pending := inst.PendingAccountSwap()
	require.True(t, pending)
	require.Equal(t, "work", from)
	require.Equal(t, "personal", to)

	// Any OTHER handoff is still a different transaction the committed swap
	// owns: a different account or a different agent stays fenced.
	_, err = m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, To: "codex", Account: "personal"})
	require.ErrorContains(t, err, "committed account swap")

	// Naming the swap's own committed target IS the advertised retry. It
	// re-delivers the recorded mission rather than admitting a fresh swap.
	backend.sendPromptErr = nil
	resp, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
	require.NoError(t, err)
	require.True(t, resp.OK)
	require.Equal(t, "work", resp.FromAccount)
	require.Equal(t, "personal", resp.ToAccount)
	_, _, pending = inst.PendingAccountSwap()
	require.False(t, pending, "the retried transaction must retire its durable marker")
	_, _, prompts := backend.snapshot()
	require.Len(t, prompts, 1)
	require.Contains(t, prompts[0], "finish migration")
	require.Contains(t, prompts[0], `claude account "personal"`)
	require.Len(t, inst.ToInstanceData().Tabs[0].Handoffs, 1,
		"the retry must finish the recorded transaction, not append a second handoff")
}

// TestHandoffAccountRetryReportsRecordedAgentBoundary is the cross-agent half
// of the committed-retry path: once a codex→claude swap's identity checkpoint
// lands, the live agent IS the incoming one, so a retry that re-derives "from"
// from CurrentAgentName reports claude→claude. The response must come from the
// durable ledger entry — the only place the recorded transition and its head
// attribution boundary still exist.
func TestHandoffAccountRetryReportsRecordedAgentBoundary(t *testing.T) {
	m, repo, inst, backend := newAutoResumeManager(t, "", true, "finish migration", time.Now().Add(time.Hour))
	prepareHandoffTargetPreflight(t, inst)
	configureLimitAccountCandidate(t, m, "personal")
	inst.Program = "codex"
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, "codex"))
	inst.Account = "work"
	inst.ClearLimitReached()
	m.cfg.LimitAutoResume = false
	backend.onRespawn = func(i *session.Instance) { i.SetTmuxSession(tmux.NewTmuxSession(i.Title, i.AgentProgram())) }
	backend.sendPromptErr = errors.New("delivery interrupted")
	_, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, To: "claude", Account: "personal"})
	require.ErrorContains(t, err, "delivery interrupted")
	_, _, pending := inst.PendingAccountSwap()
	require.True(t, pending)
	recorded, ok := inst.LastHandoff()
	require.True(t, ok)
	require.Equal(t, "codex", recorded.From.Agent)
	require.Equal(t, "claude", recorded.To)
	require.NotEmpty(t, recorded.HeadSHA, "the committed transaction must record its attribution boundary")

	backend.sendPromptErr = nil
	resp, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, To: "claude", Account: "personal"})
	require.NoError(t, err)
	require.True(t, resp.OK)
	require.Equal(t, "codex", resp.From, "a committed retry reports the recorded outgoing agent, not the live incoming one")
	require.Equal(t, "claude", resp.To)
	require.Equal(t, "work", resp.FromAccount)
	require.Equal(t, "personal", resp.ToAccount)
	require.Equal(t, recorded.HeadSHA, resp.HeadSHA)
	_, _, pending = inst.PendingAccountSwap()
	require.False(t, pending)
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
	prepareHandoffTargetPreflight(t, inst)
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
