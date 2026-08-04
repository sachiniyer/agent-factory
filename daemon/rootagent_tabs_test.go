package daemon

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// The #2628 regression suite, the second half of the #2616 audit. The root
// agent's heal replaces its record rather than re-spawning into it, and a fresh
// create comes up with only its agent tab (#1100) — so a root that had a
// terminal, a process tab, a dev-server web tab, or an editor lost all of them
// to a tmux outage, and the record listing them was deleted in the same pass.

// seedRootTabs gives the manager's live root the tab roster a real one
// accumulates. The fake create backend spawns nothing, so the tabs are the
// tmux-less test shape; what the heal carries is the persisted ROSTER.
func seedRootTabs(t *testing.T, inst *session.Instance) {
	t.Helper()
	require.Len(t, inst.GetTabs(), 1, "seed expects the agent tab and nothing else")
	inst.AddTabForTest("shell", session.TabKindShell)
	inst.AddWebTabForTest("web", "http://localhost:5173/")
}

// TestEnsureRootAgentsCarriesTabsAcrossTmuxVanish is the #2628 headline
// assertion: the re-created root must be asked to rebuild the tabs the vanished
// one had. Asserting only that a root exists again passes against the pre-fix
// daemon — the bug was never a missing root, it was a root that came back with
// a one-row tab strip.
func TestEnsureRootAgentsCarriesTabsAcrossTmuxVanish(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	require.NoError(t, err)
	manager.EnsureRootAgents()

	first := findRootInstance(t, manager, repoPath)
	require.NotNil(t, first, "root instance missing after first ensure")
	require.Len(t, *seen, 1)
	require.Empty(t, (*seen)[0].RestoreTabs, "a first-ever root create has no roster to rebuild")
	seedRootConversation(t, first)
	seedRootTabs(t, first)

	// The #1104 outage class: tmux vanished under a healthy daemon.
	first.SetStatusForTest(session.Lost)
	manager.EnsureRootAgents()

	require.Len(t, *seen, 2, "the vanished root must be reaped and re-created")
	carried := (*seen)[1].RestoreTabs
	require.Len(t, carried, 3, "the whole roster rides across, agent tab included")
	require.Equal(t, session.TabKindAgent, carried[0].Kind)
	require.Equal(t, "shell", carried[1].Name)
	require.Equal(t, session.TabKindShell, carried[1].Kind)
	require.Equal(t, session.TabKindWeb, carried[2].Kind)
	require.Equal(t, "http://localhost:5173/", carried[2].URL,
		"a web tab is only its target; carrying the row without the URL carries nothing")
}

// TestEnsureRootAgentsKeepsTheTabCarryWhenTheConversationCannotResume: the two
// carries are independent. A conversation the provider can no longer resume
// retries the create without it — and that retry must still rebuild the tabs,
// or recovering from one loss would silently cause the other.
func TestEnsureRootAgentsKeepsTheTabCarryWhenTheConversationCannotResume(t *testing.T) {
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
	seedRootTabs(t, first)

	first.SetStatusForTest(session.Lost)
	manager.EnsureRootAgents()

	require.Len(t, seen, 3, "the failed carried create must be retried without the conversation")
	require.False(t, seen[2].ResumeConversation.HasID())
	require.Len(t, seen[2].RestoreTabs, 3,
		"dropping the unresumable conversation must not drop the tabs with it")
}

// TestReportRootTabCarryReportsWhatCameBack: the same rule as the conversation
// carry (#2616). A tab that could not be re-spawned is dropped best-effort, so
// a heal that came back short must not read like a clean one — and a root that
// never had extra tabs must not log about tabs at all.
func TestReportRootTabCarryReportsWhatCameBack(t *testing.T) {
	agent := session.TabData{Name: "agent", Kind: session.TabKindAgent}
	shell := session.TabData{Name: "shell", Kind: session.TabKindShell}
	web := session.TabData{Name: "web", Kind: session.TabKindWeb}

	tests := []struct {
		name        string
		carried     []session.TabData
		created     []session.TabData
		wantInfo    string
		wantWarning string
	}{
		{
			name:    "no extra tabs to restore",
			carried: []session.TabData{agent},
			created: []session.TabData{agent},
		},
		{
			name:     "every tab came back",
			carried:  []session.TabData{agent, shell, web},
			created:  []session.TabData{agent, shell, web},
			wantInfo: "restored its 2 non-agent tabs",
		},
		{
			name:        "a tab could not be restored",
			carried:     []session.TabData{agent, shell, web},
			created:     []session.TabData{agent, web},
			wantWarning: "brought back 1 of its 2 non-agent tabs",
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

			reportRootTabCarry("/repo", tc.carried, tc.created)

			if tc.wantInfo == "" && tc.wantWarning == "" {
				require.Empty(t, strings.TrimSpace(info.String()))
				require.Empty(t, strings.TrimSpace(warning.String()))
				return
			}
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
