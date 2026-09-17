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
	require.NoError(t, m.resumeFromLimit(ResumeFromLimitRequest{Title: inst.Title, RepoID: repo}))
	_, _, pending := inst.PendingAccountSwap()
	require.False(t, pending)
	name, automatic := inst.AccountSelection()
	require.Equal(t, "personal", name)
	require.False(t, automatic)
	require.Len(t, inst.ToInstanceData().Tabs[0].Handoffs, 1)
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

// TestHandoffAccountRetryRoutesCrossAgentTargetBeforeRelaunch is the routing
// half of the pre-relaunch retry that #4401 never exercised. A committed
// cross-agent manual swap rewrites i.Program to the incoming agent, but until
// the replacement pane relaunches the live tmux session still names the
// outgoing agent. The shipped onRespawn hook forces the pane to update before
// the retry, so the live-pane and committed-record sources coincide and mask
// the race. This test commits the swap directly (the way respawnFresh failing
// before setLaunchProgram leaves it: i.Program rewritten, ledger entry and
// pending marker durable, ReplacementPanesStarted false, and the bound pane
// still reporting the outgoing agent), then retries with an explicit
// --to <target>: the gate must admit it AND the routing must send it to
// committedAccountSwap (the recorded transaction), not a fresh admission that
// overwrites the stored mission and headSHA.
func TestHandoffAccountRetryRoutesCrossAgentTargetBeforeRelaunch(t *testing.T) {
	m, repo, inst, backend := newAutoResumeManager(t, "", true, "finish migration", time.Now().Add(time.Hour))
	prepareHandoffTargetPreflight(t, inst)
	configureLimitAccountCandidate(t, m, "personal")
	inst.Program = "codex"
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, "codex"))
	inst.Account = "work"
	inst.ClearLimitReached()
	m.cfg.LimitAutoResume = false
	// Commit the cross-agent manual swap directly, the way the real bug state
	// persists it: the identity checkpoint landed (i.Program rewritten to the
	// incoming agent, ledger entry recorded, pending marker durable) but the
	// replacement pane never reached setLaunchProgram, so the bound tmux
	// session still reports the outgoing agent and ReplacementPanesStarted is
	// false. No respawn runs here, so SynchronizeAccountSwapRuntimeMetadata is
	// never reached for the commit — exactly the pre-relaunch window.
	require.NoError(t, inst.BeginManualAccountSwap())
	require.NoError(t, inst.ValidateManualAccountSwap("personal", "claude"))
	_, err := inst.SelectAccountForHandoff("work", "personal", "claude", session.HandoffReasonManual, "tip", "finish migration")
	require.NoError(t, err)
	require.NoError(t, m.persistSettlement(repo, daemonInstanceKey(repo, inst.Title), inst))
	require.True(t, inst.EndLimitResume())
	require.Equal(t, "claude", inst.AgentProgram(), "commit rewrites the durable record to the incoming agent")
	require.Equal(t, "codex", inst.CurrentAgentName(), "the live pane still reports the outgoing agent before relaunch")
	_, _, pending := inst.PendingAccountSwap()
	require.True(t, pending)
	recorded, ok := inst.LastHandoff()
	require.True(t, ok)
	require.Equal(t, "codex", recorded.From.Agent)
	require.Equal(t, "claude", recorded.To)
	require.NotEmpty(t, recorded.HeadSHA, "the committed transaction must record its attribution boundary")

	// The retry's recovery path relaunches the replacement pane; onRespawn
	// stands in for setLaunchProgram landing during that retry, not before the
	// routing decision below — at the gate and the routing branch the pane is
	// still the outgoing "codex".
	backend.onRespawn = func(i *session.Instance) { i.SetTmuxSession(tmux.NewTmuxSession(i.Title, i.AgentProgram())) }

	// The retry names the committed target explicitly against a stale live
	// pane. Without the gate fix it is refused as a "committed account swap";
	// with only the gate fix it slips past routing into a fresh admission that
	// appends a second ledger entry and overwrites the stored mission.
	resp, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, To: "claude", Account: "personal"})
	require.NoError(t, err, "retry of the committed swap's own target must pass the gate and route to committedAccountSwap")
	require.True(t, resp.OK)
	require.Equal(t, "codex", resp.From, "a committed retry reports the recorded outgoing agent, not the stale live pane")
	require.Equal(t, "claude", resp.To)
	require.Equal(t, "work", resp.FromAccount)
	require.Equal(t, "personal", resp.ToAccount)
	require.Equal(t, recorded.HeadSHA, resp.HeadSHA, "the committed retry preserves the recorded attribution boundary")
	require.Len(t, inst.ToInstanceData().Tabs[0].Handoffs, 1,
		"the retry must finish the recorded transaction, not append a fresh admission that overwrites the stored mission")
	_, _, prompts := backend.snapshot()
	require.Len(t, prompts, 1)
	require.Contains(t, prompts[0], "finish migration")
	_, _, pending = inst.PendingAccountSwap()
	require.False(t, pending, "the retried transaction must retire its durable marker")
}
