package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

func TestHandoffAccountMissingTargetRefusesBeforeTeardown(t *testing.T) {
	for _, nonExecutable := range []bool{false, true} {
		t.Run(fmt.Sprint(nonExecutable), func(t *testing.T) {
			m, repo, inst, backend := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
			configureLimitAccountCandidate(t, m, "personal")
			target := filepath.Join(t.TempDir(), "claude")
			if nonExecutable {
				require.NoError(t, os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0600))
			}
			writeLimitAccountCandidates(t, "[program_overrides]\nclaude = \"claude\"\n")
			t.Setenv("PATH", filepath.Dir(target)+":/usr/bin:/bin")
			backend.onRespawn = func(i *session.Instance) { i.SetTmuxSession(tmux.NewTmuxSession(i.Title, i.AgentProgram())) }
			inst.Program = "codex"
			inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, "codex"))
			inst.ClearLimitReached()
			gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
			require.NoError(t, err)
			inst.SetGitWorktreeForTest(gw)
			_, err = m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, To: "claude", Account: "personal"})
			require.ErrorContains(t, err, "launch preflight")
			require.False(t, isMutationCommitted(err))
			require.Equal(t, "codex", inst.AgentProgram())
			require.Empty(t, inst.Handoffs())
			require.Nil(t, inst.ToInstanceData().PendingAccountSwap)
			_, respawns, prompts := backend.snapshot()
			require.Zero(t, respawns)
			require.Empty(t, prompts)
		})
	}
}

func TestHandoffAccountMissingTargetRefusesWhenCheckoutMarkerProbeTimesOut(t *testing.T) {
	m, repo, inst, backend := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	binDir := t.TempDir()
	gitShim := filepath.Join(binDir, "git")
	shim := fmt.Sprintf(`#!/bin/sh
if [ "$3" = "rev-parse" ] && [ "$4" = "--git-common-dir" ]; then
  exec /bin/sleep 5
fi
exec %q "$@"
`, realGit)
	require.NoError(t, os.WriteFile(gitShim, []byte(shim), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")
	backend.onRespawn = func(i *session.Instance) {
		i.SetTmuxSession(tmux.NewTmuxSession(i.Title, i.AgentProgram()))
	}
	inst.Program = "codex"
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, "codex"))
	inst.ClearLimitReached()
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)

	_, err = m.HandoffSession(HandoffSessionRequest{
		Title: inst.Title, RepoID: repo, To: "claude", Account: "personal",
	})
	require.ErrorContains(t, err, "launch preflight")
	require.False(t, isMutationCommitted(err))
	require.Equal(t, "codex", inst.AgentProgram())
	_, respawns, prompts := backend.snapshot()
	require.Zero(t, respawns)
	require.Empty(t, prompts)
}

func TestHandoffAccountRechecksChangedProgramOverrideUnderProjectLock(t *testing.T) {
	m, repoID, inst, _ := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	project, err := config.RegisterProject(inst.Path)
	require.NoError(t, err)
	_, err = config.SetProjectConfigValue(project.ID, "program_overrides.claude", filepath.Join(t.TempDir(), "missing-claude"))
	require.NoError(t, err)
	prepareHandoffTargetPreflight(t, inst)
	inst.Account = "work"
	inst.ClearLimitReached()

	precheckDone := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releasePrecheck := func() { releaseOnce.Do(func() { close(release) }) }
	m.accountSwapAfterManualPrecheckForTest = func() {
		close(precheckDone)
		<-release
	}
	t.Cleanup(func() {
		releasePrecheck()
		m.accountSwapAfterManualPrecheckForTest = nil
	})

	handoffDone := make(chan error, 1)
	go func() {
		_, err := m.HandoffSession(HandoffSessionRequest{
			Title: inst.Title, RepoID: repoID, Account: "personal",
		})
		handoffDone <- err
	}()
	select {
	case <-precheckDone:
	case <-time.After(5 * time.Second):
		t.Fatal("manual handoff did not reach the precheck-to-lock window")
	}
	_, err = config.SetProjectConfigValue(project.ID, "program_overrides.claude", "claude")
	require.NoError(t, err)
	releasePrecheck()
	select {
	case err := <-handoffDone:
		require.NoError(t, err, "the locked admission must re-evaluate the newly committed program override")
	case <-time.After(5 * time.Second):
		t.Fatal("manual handoff did not finish")
	}
}

// The precheck-to-lock window must re-resolve the account NAMESPACE, not only
// the command (#4430 review round 3): flipping `program_overrides.claude` from
// "claude" to "codex" between the advisory pass and locked admission changes
// which registry Selected consults. A namespace frozen at request time would
// admit "personal" against claude's registry while the committed launch runs
// codex — so the refusal must name codex, proving the locked pass re-resolved.
func TestHandoffAccountReresolvesAccountNamespaceUnderProjectLock(t *testing.T) {
	m, repoID, inst, _ := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal") // registered under claude only
	project, err := config.RegisterProject(inst.Path)
	require.NoError(t, err)
	_, err = config.SetProjectConfigValue(project.ID, "program_overrides.claude", "claude")
	require.NoError(t, err)
	prepareHandoffTargetPreflight(t, inst)
	// The flipped resolution needs a launchable codex so the ONLY refusal the
	// locked pass can produce is the namespace one.
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o700))
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	inst.ClearLimitReached()

	precheckDone := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releasePrecheck := func() { releaseOnce.Do(func() { close(release) }) }
	m.accountSwapAfterManualPrecheckForTest = func() {
		close(precheckDone)
		<-release
	}
	t.Cleanup(func() {
		releasePrecheck()
		m.accountSwapAfterManualPrecheckForTest = nil
	})

	handoffDone := make(chan error, 1)
	go func() {
		_, err := m.HandoffSession(HandoffSessionRequest{
			Title: inst.Title, RepoID: repoID, Account: "personal",
		})
		handoffDone <- err
	}()
	select {
	case <-precheckDone:
	case <-time.After(5 * time.Second):
		t.Fatal("manual handoff did not reach the precheck-to-lock window")
	}
	_, err = config.SetProjectConfigValue(project.ID, "program_overrides.claude", "codex")
	require.NoError(t, err)
	releasePrecheck()
	select {
	case err := <-handoffDone:
		require.Error(t, err,
			"the locked admission must consult the namespace the override NOW resolves to — "+
				"personal is registered under claude, not codex")
		require.Contains(t, err.Error(), "codex")
		require.False(t, isMutationCommitted(err))
	case <-time.After(5 * time.Second):
		t.Fatal("manual handoff did not finish")
	}
}

// An account-only handoff (no --to) keeps the recorded program. The pane runs
// codex through program_overrides.claude, and program_overrides.codex points
// elsewhere; the request's defaulted agent is that running IDENTITY, not an
// enum, so the account must resolve in codex's registry. Re-resolving the
// identity through codex's own override admitted a gemini launch instead
// (#4430 review). "work" exists only in gemini's registry here, so the old
// resolution passes the namespace check while the fixed one names codex.
func TestHandoffAccountOnlyResolvesTheRecordedProgramNamespace(t *testing.T) {
	m, repoID, inst, _ := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal") // registered under claude only
	project, err := config.RegisterProject(inst.Path)
	require.NoError(t, err)
	_, err = config.SetProjectConfigValue(project.ID, "program_overrides.claude", "codex")
	require.NoError(t, err)
	_, err = config.SetProjectConfigValue(project.ID, "program_overrides.codex", "gemini")
	require.NoError(t, err)
	home, err := config.GetConfigDir()
	require.NoError(t, err)
	_, err = agentaccount.Register(home, tmux.ProgramGemini, "work")
	require.NoError(t, err)
	prepareHandoffTargetPreflight(t, inst)
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, tmux.ProgramCodex))
	inst.ClearLimitReached()
	require.Equal(t, tmux.ProgramCodex, inst.CurrentAgentName(), "precondition: the pane runs codex")

	_, err = m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repoID, Account: "work"})
	require.Error(t, err)
	require.Contains(t, err.Error(), `account "work" is not registered for codex`,
		"an account-only request resolves in the running agent's registry")
	require.NotContains(t, err.Error(), tmux.ProgramGemini)
	require.False(t, isMutationCommitted(err))
	require.Equal(t, tmux.ProgramClaude, inst.AgentProgram())
}

// handoffRealPlanBackend keeps limitResumeBackend's recorded surface but runs
// the REAL LocalBackend.PrepareAgentSwap, so the daemon judges the command a
// production handoff actually froze. The plain fake freezes program=target —
// it never resolves program_overrides, which is exactly the indirection the
// cross-agent refusal reads: with the fake's plan, EffectiveAgent returns the
// enum and the refusal can never fire, no matter what the override says.
type handoffRealPlanBackend struct{ *limitResumeBackend }

func (b *handoffRealPlanBackend) PrepareAgentSwap(i *session.Instance, target string) (session.AgentSwapPlan, error) {
	return (&session.LocalBackend{}).PrepareAgentSwap(i, target)
}

// The ordinary (no --account) handoff must judge account capability on the
// resolved command, not the target enum (#4430 review). `program_overrides.
// aider = "codex"` passes the enum check — aider has no account namespace — but
// the plan's frozen command launches Codex, which does. Dropping the recorded
// scope would start Codex with ambient credentials, so the handoff must refuse
// and name the resolution, before any pane is touched.
func TestHandoffScopedSessionRefusesCrossAgentProgramOverride(t *testing.T) {
	m, repo, inst, backend := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	inst.SetBackend(&handoffRealPlanBackend{backend})
	inst.Account = "work"
	inst.ClearLimitReached()
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o700))
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	writeLimitAccountCandidates(t, "[program_overrides]\naider = \"codex\"\n")

	_, err = m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, To: "aider"})
	require.ErrorContains(t, err, "resolves to codex")
	require.False(t, isMutationCommitted(err))
	require.Equal(t, "claude", inst.AgentProgram())
	account, _ := inst.AccountSelection()
	require.Equal(t, "work", account,
		"a refused handoff leaves the recorded scope untouched")
	require.Empty(t, inst.Handoffs())
	_, respawns, prompts := backend.snapshot()
	require.Zero(t, respawns)
	require.Empty(t, prompts)
}

// The inverse override direction of the test above (#4430 review):
// `program_overrides.codex = "aider"` makes a "codex" handoff launch Aider,
// which has no account namespace — the honest answer is the scope drop, not a
// --account refusal that could never name an Aider account. The capability
// check reads the frozen plan's EffectiveAgent, so admission must see the same
// aider the launch will.
func TestHandoffScopedSessionDescopesCrossAgentProgramOverride(t *testing.T) {
	m, repo, inst, backend := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	inst.SetBackend(&handoffRealPlanBackend{backend})
	inst.Account = "work"
	inst.ClearLimitReached()
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "aider"), []byte("#!/bin/sh\nexit 0\n"), 0o700))
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	writeLimitAccountCandidates(t, "[program_overrides]\ncodex = \"aider\"\n")

	resp, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, To: "codex"})
	require.NoError(t, err,
		"a target whose resolved command cannot carry a scope must take the descope, not a --account refusal")
	require.Equal(t, "work", resp.FromAccount)
	require.Empty(t, resp.ToAccount)
	account, _ := inst.AccountSelection()
	require.Empty(t, account, "the record dropped the scope — aider has no namespace for it")
	handoffs := inst.Handoffs()
	require.Len(t, handoffs, 1)
	require.Equal(t, "work", handoffs[0].FromAccount)
	_, _, prompts := backend.snapshot()
	require.Len(t, prompts, 1)
}

// Error precedence on the plan-failure path (#4430 review): PrepareAgentSwap
// resolves the command BEFORE preflight checks it, so a target that is BOTH
// unlaunchable AND scopable-resolved must still get the scope refusal —
// --account is the remedy the user can act on, while the preflight detail
// would send them to install an agent they were never going to reach. A
// non-scopable resolution leaves the preflight error to name the real blocker.
func TestHandoffScopedSessionScopeRefusalBeatsPreflightFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		override   string
		wantErr    string
		absentErr  string
		wantDetail string
	}{
		{name: "scopable resolution wins over preflight",
			override:   "aider = \"/nonexistent/codex\"",
			wantErr:    "--account",
			absentErr:  "preflight",
			wantDetail: "resolves to codex"},
		{name: "non-scopable resolution leaves preflight",
			override:  "aider = \"/nonexistent/aider\"",
			wantErr:   "preflight",
			absentErr: "--account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, repo, inst, backend := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
			inst.SetBackend(&handoffRealPlanBackend{backend})
			inst.Account = "work"
			inst.ClearLimitReached()
			gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
			require.NoError(t, err)
			inst.SetGitWorktreeForTest(gw)
			writeLimitAccountCandidates(t, "[program_overrides]\n"+tc.override+"\n")

			_, err = m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, To: "aider"})
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
			require.NotContains(t, err.Error(), tc.absentErr)
			if tc.wantDetail != "" {
				require.Contains(t, err.Error(), tc.wantDetail)
			}
			account, _ := inst.AccountSelection()
			require.Equal(t, "work", account, "a refused handoff never touches the scope")
		})
	}
}

// The descope sibling fence must cover teardowns that already committed: a
// PendingTabCleanup handle is a tmux session whose kill was never confirmed,
// so its process may still run under the dropped account's environment while
// the live roster reports it gone (#4430 review). The handoff must refuse
// there exactly as validateAccountSwap does, and name the remedy — the
// cleanup sweep the next daemon start runs — because the removed tab cannot
// be closed again.
func TestHandoffScopedSessionDescopeRefusesPendingTabCleanup(t *testing.T) {
	m, repo, inst, backend := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	inst.SetBackend(&handoffRealPlanBackend{backend})
	inst.Account = "work"
	inst.ClearLimitReached()
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "aider"), []byte("#!/bin/sh\nexit 0\n"), 0o700))
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	writeLimitAccountCandidates(t, "[program_overrides]\ncodex = \"aider\"\n")
	inst.SetPendingTabCleanupForTest([]session.TabCleanupData{
		{TabID: "build", TmuxName: inst.Title + "__build"},
	})

	_, err = m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, To: "codex"})
	require.ErrorContains(t, err, "unconfirmed",
		"a pending teardown can still run under the dropped account — the descope must refuse it")
	require.ErrorContains(t, err, "restart af",
		"the refusal must name the remedy: the next start retries the cleanup sweep")
	account, _ := inst.AccountSelection()
	require.Equal(t, "work", account,
		"a refused handoff leaves the recorded scope untouched")
	require.Empty(t, inst.Handoffs())
	_, respawns, prompts := backend.snapshot()
	require.Zero(t, respawns)
	require.Empty(t, prompts)
}

func TestHandoffAccountHealthyDeliveryFailureDoesNotInventQuota(t *testing.T) {
	for _, live := range []session.Liveness{session.LiveRunning, session.LiveReady} {
		t.Run(fmt.Sprint(live), func(t *testing.T) {
			m, repo, inst, backend := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
			configureLimitAccountCandidate(t, m, "personal")
			inst.Account = "work"
			prepareHandoffTargetPreflight(t, inst)
			inst.ClearLimitReached()
			require.NoError(t, inst.Transition(session.ObserveLiveness(live)))
			backend.sendPromptErr = errors.New("delivery interrupted")
			resp, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
			require.ErrorContains(t, err, "delivery interrupted")
			require.True(t, isMutationCommitted(err))
			require.Equal(t, "claude", resp.From)
			require.Equal(t, "claude", resp.To)
			require.Equal(t, "work", resp.FromAccount)
			require.Equal(t, "personal", resp.ToAccount)
			require.NotEmpty(t, resp.HeadSHA)
			require.Equal(t, live, inst.GetLiveness())
			saved := persistedInstanceByTitle(t, repo, inst.Title)
			require.Equal(t, live, saved.Liveness)
			require.NotNil(t, saved.PendingAccountSwap)
			_, observations := session.AccountLimitEvidenceFromData(saved)
			for _, observation := range observations {
				require.NotEqual(t, "personal", observation.Account)
			}
			backend.sendPromptErr = nil
			m.cfg.LimitAutoResume = false
			m.ResumeLimitedSessions()
			require.NotNil(t, inst.ToInstanceData().PendingAccountSwap,
				"an unconfirmed delivery must wait for an explicit operator retry")
			require.NoError(t, m.resumeFromLimit(ResumeFromLimitRequest{Title: inst.Title, RepoID: repo}))
			require.Nil(t, inst.ToInstanceData().PendingAccountSwap)
		})
	}
}

func TestHandoffAccountFinalSettlementFailureIsReportedAndNotRedelivered(t *testing.T) {
	m, repo, inst, backend := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	inst.ClearLimitReached()
	previous := testHookPersistInstanceData
	defer func() { testHookPersistInstanceData = previous }()
	fail := true
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		if fail && data.Title == inst.Title && data.Account == "personal" && data.PendingAccountSwap == nil {
			return errors.New("completion disk unavailable")
		}
		return nil
	}
	_, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
	require.ErrorContains(t, err, "pending settlement")
	require.Nil(t, inst.ToInstanceData().PendingAccountSwap)
	require.NotNil(t, persistedInstanceByTitle(t, repo, inst.Title).PendingAccountSwap)
	require.Contains(t, m.settleOwed, stableSessionKey(repo, inst))
	fail = false
	m.FlushOwedSettlements()
	m.ResumeLimitedSessions()
	require.Nil(t, persistedInstanceByTitle(t, repo, inst.Title).PendingAccountSwap)
	_, _, prompts := backend.snapshot()
	require.Len(t, prompts, 1)
}

func TestControlHandoffAccountSettlementFailureUsesCommittedEnvelope(t *testing.T) {
	m, repo, inst, backend := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	inst.Account = "work"
	inst.ClearLimitReached()
	prepareHandoffTargetPreflight(t, inst)
	previous := testHookPersistInstanceData
	defer func() { testHookPersistInstanceData = previous }()
	fail := true
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		if fail && data.Title == inst.Title && data.Account == "personal" && data.PendingAccountSwap == nil {
			return errors.New("completion disk unavailable")
		}
		return nil
	}
	var resp HandoffSessionResponse
	cs := &controlServer{manager: m}
	require.NoError(t, cs.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"}, &resp))
	require.True(t, resp.OK)
	require.Equal(t, "claude", resp.From)
	require.Equal(t, "claude", resp.To)
	require.Equal(t, "work", resp.FromAccount)
	require.Equal(t, "personal", resp.ToAccount)
	require.NotEmpty(t, resp.HeadSHA)
	require.Equal(t, apiproto.ErrorCodeMutationCommitted, resp.MutationOutcome.Code)
	require.Contains(t, resp.MutationOutcome.Warning, "pending settlement")
	_, _, prompts := backend.snapshot()
	require.Len(t, prompts, 1)
}

func TestControlRetryHandoffSettlementFailureUsesCommittedEnvelope(t *testing.T) {
	m, repo, inst, backend := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	inst.Account = "work"
	inst.ClearLimitReached()
	prepareHandoffTargetPreflight(t, inst)
	backend.sendPromptErr = errors.New("delivery reply lost")
	_, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
	require.Error(t, err)
	require.True(t, inst.PendingManualAccountSwapDeliveryUnconfirmed())
	backend.sendPromptErr = nil

	previous := testHookPersistInstanceData
	defer func() { testHookPersistInstanceData = previous }()
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		if data.Title == inst.Title && data.Account == "personal" && data.PendingAccountSwap == nil {
			return errors.New("completion disk unavailable")
		}
		return nil
	}

	var resp ResumeFromLimitResponse
	err = (&controlServer{manager: m}).ResumeFromLimit(
		ResumeFromLimitRequest{Title: inst.Title, RepoID: repo}, &resp,
	)
	require.NoError(t, err, "a delivered retry must answer through the response envelope")
	require.True(t, resp.OK)
	require.Equal(t, apiproto.ErrorCodeMutationCommitted, resp.MutationOutcome.Code)
	require.Contains(t, resp.MutationOutcome.Warning, "pending settlement")
}

func TestControlRetryHandoffDeliveryFailureIsNotCommitted(t *testing.T) {
	m, repo, inst, backend := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	inst.Account = "work"
	inst.ClearLimitReached()
	prepareHandoffTargetPreflight(t, inst)
	backend.sendPromptErr = errors.New("delivery reply lost")
	_, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
	require.Error(t, err)
	require.NotNil(t, inst.ToInstanceData().PendingAccountSwap)

	backend.sendPromptErr = errors.New("retry delivery failed")
	var resp ResumeFromLimitResponse
	err = (&controlServer{manager: m}).ResumeFromLimit(
		ResumeFromLimitRequest{Title: inst.Title, RepoID: repo}, &resp,
	)
	require.ErrorContains(t, err, "retry delivery failed")
	require.False(t, isMutationCommitted(err),
		"a retry that did not deliver the mission must remain a failed retry")
	require.False(t, resp.OK)
	require.Empty(t, resp.MutationOutcome.Code)
	require.NotNil(t, inst.ToInstanceData().PendingAccountSwap)
}

func prepareHandoffTargetPreflight(t *testing.T, inst *session.Instance) {
	t.Helper()
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0700))
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
}
