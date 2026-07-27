package daemon

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #2616 regression suite. The root agent bypasses the general Lost-restore
// loop for its stronger always-ensure semantics — correctly — but that bypass
// reaps the record holding agent_conversation and creates a brand-new session,
// so the one always-on session (the target of every watch/monitor delivery)
// was the only one that came back with an empty context.

const priorRootConversationID = "64ea06ed-7206-7fc2-803b-f7045e07a242"

// seedRootConversation gives the manager's live root the recorded conversation
// a real one carries, the way a claude root gets one from the --session-id
// injected at first launch. The fake create backend spawns no program and so
// records none of its own.
func seedRootConversation(t *testing.T, inst *session.Instance) session.AgentConversationData {
	t.Helper()
	inst.SetTmuxSession(tmux.NewTmuxSession(session.RootSessionTitle, tmux.ProgramClaude))
	conv := session.AgentConversationData{
		Agent:       tmux.ProgramClaude,
		ID:          priorRootConversationID,
		CapturedAt:  time.Date(2026, 7, 26, 21, 35, 39, 0, time.UTC),
		CaptureKind: session.ConversationCaptureInjected,
	}
	require.True(t, inst.SetAgentConversation(conv))
	return conv
}

// TestEnsureRootAgentsCarriesConversationAcrossTmuxVanish is the #2616 headline
// assertion: the conversation id the re-created root is launched on must be the
// id the vanished one had. Asserting only that a root exists again passes
// against the pre-fix daemon and proves nothing — the bug was never a missing
// root, it was a root that came back as somebody else.
func TestEnsureRootAgentsCarriesConversationAcrossTmuxVanish(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	require.NoError(t, err)
	manager.EnsureRootAgents()

	first := findRootInstance(t, manager, repoPath)
	require.NotNil(t, first, "root instance missing after first ensure")
	require.Len(t, *seen, 1)
	require.False(t, (*seen)[0].ResumeConversation.HasID(),
		"a first-ever root create has no prior conversation to carry")
	prior := seedRootConversation(t, first)

	// The #1104 outage class: tmux vanished under a healthy daemon.
	first.SetStatusForTest(session.Lost)
	manager.EnsureRootAgents()

	require.Len(t, *seen, 2, "the vanished root must be reaped and re-created")
	carried := (*seen)[1].ResumeConversation
	require.Equal(t, prior.ID, carried.ID,
		"the re-created root must resume the conversation the vanished one held, not mint a new id")
	require.Equal(t, prior.Agent, carried.Agent)
	require.NotNil(t, findRootInstance(t, manager, repoPath), "always-ensure: the root must exist again")
}

// unresumableCarryBackend fails exactly the create that carries a conversation
// forward, standing in for `claude --resume <id>` exiting at startup because
// the provider no longer has that conversation.
type unresumableCarryBackend struct {
	readyFakeBackend
}

func (unresumableCarryBackend) Start(*session.Instance, bool) error {
	return errors.New("simulated: the agent no longer has that conversation")
}

// TestEnsureRootAgentsFallsBackToAFreshAgentWhenTheCarriedCreateFails: the
// always-on guarantee outranks continuity. A recorded conversation the provider
// can no longer resume must not be able to keep the root DOWN — that would turn
// #2616 ("the root came back without its history") into something strictly
// worse ("the root never came back"), on every tick, forever.
func TestEnsureRootAgentsFallsBackToAFreshAgentWhenTheCarriedCreateFails(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	var seen []session.InstanceOptions
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, _ string) (session.Backend, error) {
		seen = append(seen, opts)
		fake := session.NewFakeBackend()
		fake.CompleteStart()
		if opts.ResumeConversation.HasID() {
			return unresumableCarryBackend{readyFakeBackend{fake}}, nil
		}
		return readyFakeBackend{fake}, nil
	})
	t.Cleanup(restore)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	require.NoError(t, err)
	manager.EnsureRootAgents()
	first := findRootInstance(t, manager, repoPath)
	require.NotNil(t, first)
	seedRootConversation(t, first)

	first.SetStatusForTest(session.Lost)
	manager.EnsureRootAgents()

	require.Len(t, seen, 3, "the failed carried create must be retried without the carry, in the same pass")
	require.Equal(t, priorRootConversationID, seen[1].ResumeConversation.ID)
	require.False(t, seen[2].ResumeConversation.HasID(),
		"the fallback create must start a fresh agent rather than the conversation that just failed")
	require.NotNil(t, findRootInstance(t, manager, repoPath),
		"always-ensure: an unresumable conversation must not leave the root down")

	manager.mu.Lock()
	st := manager.rootEnsureStates[repoPath]
	manager.mu.Unlock()
	require.NotNil(t, st)
	require.Zero(t, st.consecutiveFailures, "a root that came back is not a failed ensure")
}

// TestReportRootConversationCarryDistinguishesTheThreeOutcomes: whichever way
// the carry goes, the log must SAY which. A fresh agent is a legitimate outcome
// when the prior conversation cannot be recovered — but if it reads the same as
// a successful carry-over, #2616 just becomes invisible again in a new form.
func TestReportRootConversationCarryDistinguishesTheThreeOutcomes(t *testing.T) {
	prior := session.AgentConversationData{Agent: tmux.ProgramClaude, ID: priorRootConversationID}

	tests := []struct {
		name        string
		carried     session.AgentConversationData
		created     *session.AgentConversationData
		wantInfo    string
		wantWarning string
	}{
		{
			name:        "nothing recorded to carry",
			carried:     session.AgentConversationData{},
			created:     nil,
			wantWarning: "had no recorded conversation to carry",
		},
		{
			name:     "carried",
			carried:  prior,
			created:  &session.AgentConversationData{Agent: tmux.ProgramClaude, ID: priorRootConversationID},
			wantInfo: "resumed its prior claude conversation " + priorRootConversationID,
		},
		{
			name:        "recorded but not resumed",
			carried:     prior,
			created:     &session.AgentConversationData{Agent: tmux.ProgramClaude, ID: "5299e00d-1111-4222-8333-f7045e07a242"},
			wantWarning: "did not come up on its prior claude conversation " + priorRootConversationID,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var info, warning bytes.Buffer
			prevInfo, prevWarning := log.InfoLog.Writer(), log.WarningLog.Writer()
			log.InfoLog.SetOutput(&info)
			log.WarningLog.SetOutput(&warning)
			t.Cleanup(func() {
				log.InfoLog.SetOutput(prevInfo)
				log.WarningLog.SetOutput(prevWarning)
			})

			reportRootConversationCarry("/repo", tc.carried, tc.created)

			if tc.wantInfo != "" {
				require.Contains(t, info.String(), tc.wantInfo)
				require.Empty(t, strings.TrimSpace(warning.String()))
				return
			}
			require.Contains(t, warning.String(), tc.wantWarning)
			require.Empty(t, strings.TrimSpace(info.String()))
		})
	}
}
