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
	_, err = m.resumeFromLimitOutcome(ResumeFromLimitRequest{Title: inst.Title, RepoID: repo})
	require.NoError(t, err)
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
	require.NoError(t, inst.ValidateManualAccountSwap("personal", "claude", false))
	_, err := inst.SelectAccountForHandoff("work", "personal", "claude", "claude", false, session.HandoffReasonManual, "tip", "continue")
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

// TestHandoffAccountDoesNotRefuseAcrossSiblingResolvedDivergence is the
// manual-handoff surface of the limitedAccountsForSwap live-wall mis-key. A
// same-agent account-only handoff resolves its namespace from the subject's
// RUNNING agent (handoff_account.go:189), then asks limitedAccountsForSwap to
// skip accounts currently at their limit in that namespace. When a SIBLING's
// configured enum differs from its resolved runtime agent (a per-session
// respawn under program_overrides.claude = "codex"), the buggy live-wall loop
// matched the sibling's CONFIGURED enum and added the sibling's codex-namespaced
// wall into the subject's claude namespace — so the handoff was refused with a
// message naming the wrong namespace and a claude account that was not, in
// fact, limited: "claude account \"work\" is currently at its usage limit".
//
// With the fix the live-wall loop keys on the resolved agent the wall was filed
// under, so the sibling's codex wall does not leak and the handoff proceeds
// past the limit check. The subject halts at mission delivery (sendPromptErr),
// the same seam TestHandoffAccountKeepsLimitInOutgoingAgentNamespace uses, so
// the assertion is that the wrong-namespace limit refusal is gone and the
// handoff reached execution. The same-namespace companion proves a genuine
// claude-namespaced wall on "work" is still refused, so the success above is
// the keying fix and not a broken refusal path.
func TestHandoffAccountDoesNotRefuseAcrossSiblingResolvedDivergence(t *testing.T) {
	runHandoffAcrossSiblingDivergence(t, tmux.ProgramCodex, false)
}

// TestHandoffAccountStillRefusesSameNamespaceSiblingWall is the anti-vacuity
// companion: when the sibling's configured AND resolved agent both match the
// subject's claude namespace, its live wall on "work" MUST still refuse the
// handoff. This passes both before and after the fix — it proves the refusal
// machinery is wired, so the divergent test's success is the keying fix and not
// an unrelated ineligibility.
func TestHandoffAccountStillRefusesSameNamespaceSiblingWall(t *testing.T) {
	runHandoffAcrossSiblingDivergence(t, tmux.ProgramClaude, true)
}

// runHandoffAcrossSiblingDivergence builds a claude subject C (configured and
// running claude, at an ambient usage-limit wall) and a sibling B whose stored
// Program is claude but whose pane runs siblingAgent, then attempts a
// same-agent account-only handoff to the claude account "work". When
// siblingAgent is codex (configured != resolved) the handoff must succeed past
// the limit check (the sibling's codex wall must not leak into claude's
// namespace); when siblingAgent is claude (configured == resolved) the genuine
// claude-namespaced wall must still refuse it. expectRefusal selects the
// expected outcome.
func runHandoffAcrossSiblingDivergence(t *testing.T, siblingAgent string, expectRefusal bool) {
	t.Helper()
	m, repo, inst, backend := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	prepareHandoffTargetPreflight(t, inst)
	// "work" is the handoff target; register it for claude so Selected passes.
	configureLimitAccountCandidate(t, m, "work")
	// The subject's own wall stays ambient (filed by newAutoResumeManager with
	// an empty account), so it neither limits "work" nor turns the request into
	// a self-rejection. Run on "personal" so from != "work" and the request is
	// a real swap rather than a no-op.
	inst.Account = "personal"
	// Keep the subject's runtime pinned to claude, matching a pre-override
	// session that has not been individually respawned.
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, tmux.ProgramClaude))
	inst.ClearLimitReached()
	inst.SetLimitReached(time.Now().Add(time.Hour))

	// Sibling B: stored Program=claude (the configured enum the buggy loop
	// keyed on), pane runs siblingAgent, account-scoped to "work".
	bBackend := &limitResumeBackend{FakeBackend: session.NewFakeBackend(), alive: true}
	b := registerStarted(t, m, repo, inst.Path, "drifted", bBackend, true, session.Running)
	b.Program = tmux.ProgramClaude
	b.Account = "work"
	b.SetTmuxSession(tmux.NewTmuxSession(b.Title, siblingAgent))
	b.SetLimitReached(time.Now().Add(time.Hour))
	// registerStarted's seedDiskInstance overwrote the repo's on-disk instance
	// file with b's entry alone; re-append the subject so HandoffSession's
	// findSession refresh (which drops memory-only rows not on disk) still finds
	// it. refreshDaemonInstances preserves existing in-memory instances
	// wholesale, so the subject's live wall survives the refresh intact.
	require.NoError(t, appendInstanceData(repo, inst.ToInstanceData()))

	backend.onRespawn = func(i *session.Instance) {
		i.SetTmuxSession(tmux.NewTmuxSession(i.Title, i.AgentProgram()))
	}
	backend.sendPromptErr = errors.New("delivery interrupted")

	_, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "work"})
	if expectRefusal {
		// A genuine claude-namespaced wall on "work" must be refused, naming
		// the claude namespace and the work account.
		require.ErrorContains(t, err, `claude account "work" is currently at its usage limit`)
		_, respawns, _ := backend.snapshot()
		require.Zero(t, respawns, "a refused handoff must not respawn the pane")
		return
	}
	// The sibling's wall lives under siblingAgent (codex), not claude, so the
	// claude-namespace scan must not see "work": the wrong-namespace refusal is
	// gone and the handoff reaches execution, halting at mission delivery.
	require.NotContains(t, err.Error(), "is currently at its usage limit",
		"a sibling wall filed under codex must not refuse the claude handoff naming the wrong namespace")
	require.ErrorContains(t, err, "delivery interrupted",
		"the handoff must pass the limit check and reach mission delivery")
	_, respawns, _ := backend.snapshot()
	require.Equal(t, 1, respawns, "admission passed; the pane must have been respawned before delivery")
}
