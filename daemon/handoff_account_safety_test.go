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
