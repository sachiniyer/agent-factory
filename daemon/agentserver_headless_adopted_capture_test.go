package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #5947 adopted-workspace restore suite. An adopted launch that comes up on
// a fresh Codex conversation (the host's store refuses the recorded id — the
// everyday case for an account-scoped sandbox whose archive deleted and the
// restore recreated CODEX_HOME empty) must discover and record that fresh id the
// same way the ordinary launch path does, so the daemon's row names the
// conversation the restore actually began instead of an abandoned one.
//
// The capture machinery these tests drive is the real thing — a held-override
// fixture stands in for the process walk the way it does for the daemon-loop
// capture tests — but the adopted launch itself is a package-level seam
// (launchAdopted, mirroring restoreSurvivingTabs), because a real adopted launch
// respawns through *LocalBackend + tmux, which a unit test does not spin up. The
// seam fake plays the spawn: it writes the rollout a fresh conversation mints and
// reports whether the launch came up on the recorded id (resumed) or nothing
// (fresh), exactly as the production launchAdopted reads off the instance.

// adoptedFreshID is the id of the fresh Codex conversation a restore-adopt's
// respawn mints when it cannot resume the recorded one. A valid UUID — the
// capture extracts the id from the rollout filename with a UUID regex, so a
// made-up id that is not one would be extracted as empty. Distinct from every
// recorded id these fixtures use so a recording that names it is unambiguous.
const adoptedFreshID = "01a1cc99-99b0-7099-9909-0dec0dec0dec"

// adoptedRecordedID is the archived row's recorded Codex id, which this host's
// store refuses (the account-scoped archive deleted its rollout).
const adoptedRecordedID = "019f386f-7206-7fc2-803b-f7045e07a242"

// adoptedCodexFixture stands up an in-sandbox agent-server over an adopted Codex
// workspace: a tracked Codex instance whose CODEX_HOME is a brand-new empty store
// (so PinRecordedConversationForLaunch refuses the recorded id), a worktree the
// fresh rollout's session_meta is correlated to, and a held-override that makes
// capture's process walk attribute every rollout in that store. It returns the
// server, the fresh-store root, and the worktree directory the rollout's cwd
// must name.
func adoptedCodexFixture(t *testing.T) (hs *headlessServer, codexHome, workDir string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	codexHome = t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	repoPath := setupControlRepo(t)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "codex-worker",
		Path:    repoPath,
		Program: tmux.ProgramCodex,
	})
	require.NoError(t, err)
	inst.SetTmuxSession(tmux.NewTmuxSession("codex-worker", tmux.ProgramCodex))
	// Attribution needs a runtime holding the rollout open; the fixture's pane
	// is a name-only handle, so the test hands the walk its store — exactly as
	// the daemon-loop capture fixtures do.
	inst.SetCodexRuntimeHeldOverrideForTest(heldAllCodexRollouts(codexHome))
	gw, err := git.NewGitWorktreeFromStorage(repoPath, repoPath, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	inst.SetStatusForTest(session.Running)
	workDir = inst.GetWorktreePath()
	require.NotEmpty(t, workDir, "fixture: the Codex capture correlates against the worktree path")
	hs = &headlessServer{
		adopted: true,
		as:      &launchStub{},
		inst:    inst,
		program: tmux.ProgramCodex,
		title:   inst.Title,
	}
	return hs, codexHome, workDir
}

// overrideLaunchAdopted swaps the adopted-launch seam for the test's lifetime.
// The fake plays the respawn: it writes the rollout a fresh conversation mints
// (fresh == true), and reports the conversation the launch came up on through
// resp — nil for a fresh start, or the resumed id when it resumed the recorded
// one — exactly as production launchAdopted reads off hs.inst.AgentConversation().
func overrideLaunchAdopted(t *testing.T, fresh bool, mintRollout bool, codexHome, workDir string) {
	t.Helper()
	orig := launchAdopted
	t.Cleanup(func() { launchAdopted = orig })
	launchAdopted = func(hs *headlessServer, req agentLifecycleRequest, resp *agentOKResponse) error {
		// The respawn a real adopted launch runs. A fresh launch mints a rollout
		// the capture must name; a resumed one does not.
		if mintRollout {
			writeDaemonCodexRolloutFileWithCwd(t, codexHome,
				"rollout-2026-11-11T13-49-03-"+adoptedFreshID+".jsonl", workDir)
		}
		resp.OK = true
		if !fresh && req.Conversation != nil && req.Conversation.HasID() {
			// The resumed launch came up on the recorded id, exactly as
			// production's respawn records it on the agent tab.
			resumed := *req.Conversation
			require.True(t, hs.inst.SetAgentConversation(resumed),
				"fixture: the resumed launch must record the conversation on the agent tab")
			resp.Conversation = &resumed
		}
		return nil
	}
}

// adoptedLaunchRequest is the restore-adopt's launch RPC: not a first-time
// setup, carrying the recorded id this host's store will refuse.
func adoptedLaunchRequest() agentLifecycleRequest {
	conv := session.AgentConversationData{Agent: tmux.ProgramCodex, ID: adoptedRecordedID}
	return agentLifecycleRequest{FirstTimeSetup: false, Conversation: &conv}
}

// TestAdoptedCodexLaunchCapturesTheFreshConversationID is the #5947 headline: a
// restore-adopt whose host refuses the recorded id starts a fresh Codex
// conversation, and the agent-server's launch must discover and record that
// fresh conversation's id — the way the ordinary launch path already does — so
// the daemon's row names the conversation the restore actually began. Before
// the fix the adopted branch returned before any capture was armed, the fresh
// id was never discovered, and the row kept the abandoned recorded id.
func TestAdoptedCodexLaunchCapturesTheFreshConversationID(t *testing.T) {
	hs, codexHome, workDir := adoptedCodexFixture(t)
	overrideLaunchAdopted(t, true /* fresh */, true /* mint the rollout */, codexHome, workDir)

	var resp agentOKResponse
	require.NoError(t, hs.Launch(adoptedLaunchRequest(), &resp))

	require.True(t, resp.OK, "the adopted launch must succeed")
	require.Nil(t, resp.Conversation,
		"a fresh launch reports no conversation synchronously; the capture discovers it in the background")

	// The capture the fix arms must record the fresh conversation the respawn
	// minted, so the daemon and the row learn the id the restore came up on.
	// Before the fix the adopted branch returned before any capture was armed,
	// so this stayed empty and the row kept the abandoned recorded id.
	require.Eventually(t, func() bool {
		return hs.inst.AgentConversation().ID == adoptedFreshID
	}, 5*time.Second, 10*time.Millisecond,
		"the adopted launch must capture and record the fresh Codex conversation id")
}

// TestAdoptedCodexLaunchSkipsCaptureWhenTheConversationResumed is the no-regression
// half: a restore-adopt that DID resume the recorded id must NOT arm the fresh
// capture — the row already names the right conversation, and a capture would
// race it. Even when a fresh rollout exists in the store, the resumed launch
// leaves it unrecorded.
func TestAdoptedCodexLaunchSkipsCaptureWhenTheConversationResumed(t *testing.T) {
	hs, codexHome, workDir := adoptedCodexFixture(t)
	// The fake resumes the recorded id AND mints a fresh rollout anyway, so a
	// wrongly-armed capture would record the fresh id over the resumed one.
	overrideLaunchAdopted(t, false /* resumed */, true /* mint a rollout all the same */, codexHome, workDir)

	var resp agentOKResponse
	require.NoError(t, hs.Launch(adoptedLaunchRequest(), &resp))

	require.True(t, resp.OK)
	require.NotNil(t, resp.Conversation, "a resumed launch reports the conversation it came up on")
	require.Equal(t, adoptedRecordedID, resp.Conversation.ID,
		"the resumed id is the one the daemon records")
	require.Equal(t, adoptedRecordedID, hs.inst.AgentConversation().ID,
		"the resumed launch recorded the resumed id on the agent tab")

	// Give a wrongly-armed capture every chance to overwrite the record, then
	// confirm it never did: the resumed id stays, and the fresh rollout the
	// fake minted is ignored.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		require.Equal(t, adoptedRecordedID, hs.inst.AgentConversation().ID,
			"a resumed adopted launch must not arm the fresh-conversation capture")
		require.False(t, hs.inst.AgentConversation().ID == adoptedFreshID,
			"the capture must not record the minted fresh rollout over the resumed id")
		time.Sleep(25 * time.Millisecond)
	}
}

// TestAdoptedNonCodexLaunchDoesNotArmCodexCapture pins that the capture is a
// Codex-only concern: an adopted Claude launch (or any non-Codex agent) arms no
// Codex capture even when it comes up fresh, exactly as the ordinary path's
// `else if codex` gate already does for Claude.
func TestAdoptedNonCodexLaunchDoesNotArmCodexCapture(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "claude-worker",
		Path:    repoPath,
		Program: "claude",
	})
	require.NoError(t, err)
	inst.SetTmuxSession(tmux.NewTmuxSession("claude-worker", "claude"))
	// A Codex store with a Codex rollout that a wrongly-armed capture would
	// attribute; none of it is this Claude session's, so it must stay unrecorded.
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	writeDaemonCodexRolloutFile(t, codexHome, "rollout-2026-11-11T13-49-03-"+adoptedFreshID+".jsonl")
	inst.SetCodexRuntimeHeldOverrideForTest(heldAllCodexRollouts(codexHome))

	hs := &headlessServer{adopted: true, as: &launchStub{}, inst: inst, program: "claude", title: inst.Title}
	orig := launchAdopted
	t.Cleanup(func() { launchAdopted = orig })
	launchAdopted = func(_ *headlessServer, _ agentLifecycleRequest, resp *agentOKResponse) error {
		resp.OK = true // a fresh Claude launch; no Codex conversation to report
		return nil
	}

	var resp agentOKResponse
	require.NoError(t, hs.Launch(adoptedLaunchRequest(), &resp))
	require.True(t, resp.OK)
	require.Nil(t, resp.Conversation, "a fresh non-Codex launch reports no conversation")

	// No Codex capture was armed, so the Codex store's rollout never lands on a
	// Claude session's record.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		require.False(t, inst.AgentConversation().HasID(),
			"a non-Codex adopted launch must not arm the Codex capture")
		time.Sleep(25 * time.Millisecond)
	}
}

// TestOrdinaryLaunchPathStillCapturesAFreshCodexConversation is the no-regression
// guard for the ordinary (non-adopted) path: the snapshot arm moved above the
// adopted branch is shared with it, and the ordinary path's post-spawn capture
// must still record a fresh Codex conversation exactly as before.
func TestOrdinaryLaunchPathStillCapturesAFreshCodexConversation(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	repoPath := setupControlRepo(t)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "codex-worker",
		Path:    repoPath,
		Program: tmux.ProgramCodex,
	})
	require.NoError(t, err)
	inst.SetTmuxSession(tmux.NewTmuxSession("codex-worker", tmux.ProgramCodex))
	inst.SetCodexRuntimeHeldOverrideForTest(heldAllCodexRollouts(codexHome))
	gw, err := git.NewGitWorktreeFromStorage(repoPath, repoPath, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	inst.SetStatusForTest(session.Running)
	// The ordinary path reaches hs.as.Launch; the adopted seam is not used here,
	// so leave the production launchAdopted in place.
	stub := &launchStub{}
	hs := &headlessServer{adopted: false, as: stub, inst: inst, program: tmux.ProgramCodex, title: inst.Title}

	var resp agentOKResponse
	require.NoError(t, hs.Launch(agentLifecycleRequest{FirstTimeSetup: false}, &resp))
	require.True(t, resp.OK)
	require.Nil(t, resp.Conversation,
		"the ordinary launch reported no recorded conversation; the capture discovers it")
	require.Equal(t, 1, stub.launched, "the ordinary path must still drive hs.as.Launch")

	// A rollout minted after the launch (the fresh conversation the spawn just
	// started) is new since the pre-spawn baseline, so the capture records it.
	workDir := inst.GetWorktreePath()
	writeDaemonCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T13-49-03-"+adoptedFreshID+".jsonl", workDir)

	require.Eventually(t, func() bool {
		return inst.AgentConversation().ID == adoptedFreshID
	}, 5*time.Second, 10*time.Millisecond,
		"the ordinary launch path must still capture and record the fresh Codex conversation")
}
