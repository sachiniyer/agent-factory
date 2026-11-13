package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #2616 regression suite. The root agent bypasses the general Lost-restore
// loop for its stronger always-ensure semantics — correctly — but that bypass
// reaps the record holding agent_conversation and creates a brand-new session,
// so the one always-on session (the target of every watch/monitor delivery)
// was the only one that came back with an empty context.

const priorRootConversationID = "64ea06ed-7206-7fc2-803b-f7045e07a242"

type codexReadyFakeBackend struct {
	*session.FakeBackend
}

func (codexReadyFakeBackend) Preview(*session.Instance) (string, error) {
	return "ready\n› ", nil
}

// attributableCodexFakeBackend is a codex root whose capture can attribute:
// the fake spawns no process, so Start hands the instance the held-set
// stand-in a launch's verified evidence would give it — a runtime that holds
// no rollout yet. Without it the root has no agent process to judge from, and
// its capture ends each attempt at once rather than polling (#7123).
type attributableCodexFakeBackend struct {
	codexReadyFakeBackend
}

func (b attributableCodexFakeBackend) Start(instance *session.Instance, firstTimeSetup bool) error {
	if err := b.codexReadyFakeBackend.Start(instance, firstTimeSetup); err != nil {
		return err
	}
	instance.SetCodexRuntimeHeldOverrideForTest(func() (map[string]struct{}, error) {
		return map[string]struct{}{}, nil
	})
	return nil
}

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

func writeRootClaudeTranscript(t *testing.T, configDir, repoPath, id string) string {
	t.Helper()
	projectName := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return '-'
	}, filepath.Clean(repoPath))
	projectDir := filepath.Join(configDir, "projects", projectName)
	require.NoError(t, os.MkdirAll(projectDir, 0o700))
	path := filepath.Join(projectDir, id+".jsonl")
	// A transcript records the directory its conversation was launched in, and
	// af counts it as this repo's by that cwd, not by its project name (#7208).
	record, err := json.Marshal(map[string]any{"type": "user", "cwd": repoPath})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(record, '\n'), 0o600))
	return path
}

// TestEnsureRootAgentsCarriesConversationAcrossTmuxVanish is the #2616 headline
// assertion: the conversation id the re-created root is launched on must be the
// id the vanished one had. Asserting only that a root exists again passes
// against the pre-fix daemon and proves nothing — the bug was never a missing
// root, it was a root that came back as somebody else.
func TestEnsureRootAgentsCarriesConversationAcrossTmuxVanish(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	claudeConfigDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeConfigDir)
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	writeRootClaudeTranscript(t, claudeConfigDir, repoPath, priorRootConversationID)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	require.NoError(t, err)
	manager.ensureRootAgentsAndWait()

	first := findRootInstance(t, manager, repoPath)
	require.NotNil(t, first, "root instance missing after first ensure")
	require.Len(t, *seen, 1)
	require.False(t, (*seen)[0].ResumeConversation.HasID(),
		"a first-ever root create has no prior conversation to carry")
	prior := seedRootConversation(t, first)

	// The #1104 outage class: tmux vanished under a healthy daemon.
	first.SetStatusForTest(session.Lost)
	manager.ensureRootAgentsAndWait()

	require.Len(t, *seen, 2, "the vanished root must be reaped and re-created")
	carried := (*seen)[1].ResumeConversation
	require.Equal(t, prior.ID, carried.ID,
		"the re-created root must resume the conversation the vanished one held, not mint a new id")
	require.Equal(t, prior.Agent, carried.Agent)
	require.NotNil(t, findRootInstance(t, manager, repoPath), "always-ensure: the root must exist again")
}

// TestReapDeadRootSnapshotsConversationOnlyAfterOwningTheOperation pins the
// ordering between a late async conversation capture and a root reap. A busy
// operation lock means capture/kill/archive may still change the record, so the
// reap must return no snapshot. Once it owns the lock and re-confirms identity,
// the conversation it returns is the exact record it is about to delete.
func TestReapDeadRootSnapshotsConversationOnlyAfterOwningTheOperation(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	require.NoError(t, err)
	manager.ensureRootAgentsAndWait()

	inst := findRootInstance(t, manager, repoPath)
	require.NotNil(t, inst)
	prior := seedRootConversation(t, inst)
	inst.SetStatusForTest(session.Lost)

	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	key := daemonInstanceKey(repo.ID, session.RootSessionTitle)
	opLock := manager.opLockFor(key)
	opLock.Lock()
	carried, reaped, err := manager.reapDeadRoot(repo.ID, inst, nil)
	opLock.Unlock()

	require.NoError(t, err)
	require.False(t, reaped)
	require.False(t, carried.conversation.HasID(),
		"a skipped reap must not snapshot state that another operation still owns")
	require.Empty(t, carried.tabs,
		"a skipped reap must not snapshot state that another operation still owns")

	carried, reaped, err = manager.reapDeadRoot(repo.ID, inst, nil)
	require.NoError(t, err)
	require.True(t, reaped)
	require.Equal(t, prior, carried.conversation,
		"the reap must snapshot the conversation under the same lock that fences deletion")
}

// TestEnsureRootAgentsDefersReapWhileConversationCaptureIsPolling closes the
// earlier edge than the operation-lock snapshot above: Codex discovery polls
// outside that lock, then takes it only to commit. Reaping while the poll is in
// flight deletes the instance that eventual commit is bound to and loses the
// newly discovered id forever. The root's capture can attribute — the case
// the fence exists for; one that cannot is below.
func TestEnsureRootAgentsDefersReapWhileConversationCaptureIsPolling(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	oldCaptureTimeout := conversationCaptureTimeout
	conversationCaptureTimeout = 500 * time.Millisecond
	t.Cleanup(func() { conversationCaptureTimeout = oldCaptureTimeout })
	var seen []session.InstanceOptions
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, _ string) (session.Backend, error) {
		seen = append(seen, opts)
		backend := session.NewFakeBackend()
		backend.CompleteStart()
		return attributableCodexFakeBackend{codexReadyFakeBackend{backend}}, nil
	})
	t.Cleanup(restore)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{
		Program: tmux.ProgramCodex,
	}))
	require.NoError(t, err)
	manager.ensureRootAgentsAndWait()

	first := findRootInstance(t, manager, repoPath)
	require.NotNil(t, first)
	manager.mu.Lock()
	pending := manager.pendingConversationCaptures[first]
	manager.mu.Unlock()
	require.Equal(t, 1, pending, "the create must register capture before publishing the root")
	first.SetStatusForTest(session.Lost)
	manager.ensureRootAgentsAndWait()

	require.Len(t, seen, 1,
		"the vanished root must stay recorded until its in-flight conversation discovery can commit")
	require.Same(t, first, findRootInstance(t, manager, repoPath))

	require.Eventually(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return manager.pendingConversationCaptures[first] == 0
	}, 2*time.Second, 20*time.Millisecond)

	manager.ensureRootAgentsAndWait()
	require.Len(t, seen, 2, "the root may be re-created once discovery settles")
}

// TestEnsureRootAgentsDoesNotDeferReapForACaptureThatCannotAttribute is the
// other side of that fence (#7123). A root with no verified agent process —
// its launch's pane query failed — can never commit a discovered id: every
// attribution judges from that process, and the active pane is no substitute.
// Its attempt ends at once rather than polling for an answer that cannot come
// (#7124 review F2), so there is nothing in flight for the reap to wait on,
// and a vanished root is re-created on the next pass instead of after the
// capture window.
func TestEnsureRootAgentsDoesNotDeferReapForACaptureThatCannotAttribute(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	t.Setenv("CODEX_HOME", t.TempDir())
	oldCaptureTimeout := conversationCaptureTimeout
	conversationCaptureTimeout = 5 * time.Second
	t.Cleanup(func() { conversationCaptureTimeout = oldCaptureTimeout })
	var seen []session.InstanceOptions
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, _ string) (session.Backend, error) {
		seen = append(seen, opts)
		backend := session.NewFakeBackend()
		backend.CompleteStart()
		return codexReadyFakeBackend{backend}, nil
	})
	t.Cleanup(restore)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{
		Program: tmux.ProgramCodex,
	}))
	require.NoError(t, err)
	manager.ensureRootAgentsAndWait()

	first := findRootInstance(t, manager, repoPath)
	require.NotNil(t, first)
	require.Eventually(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return manager.pendingConversationCaptures[first] == 0
	}, time.Second, 10*time.Millisecond,
		"an attempt with no agent process to judge from ends at once, well inside its 5s window")
	first.SetStatusForTest(session.Lost)
	manager.ensureRootAgentsAndWait()

	require.Len(t, seen, 2, "nothing is in flight, so the vanished root is re-created at once")
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
	claudeConfigDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeConfigDir)
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
	writeRootClaudeTranscript(t, claudeConfigDir, repoPath, priorRootConversationID)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	require.NoError(t, err)
	manager.ensureRootAgentsAndWait()
	first := findRootInstance(t, manager, repoPath)
	require.NotNil(t, first)
	seedRootConversation(t, first)

	first.SetStatusForTest(session.Lost)
	manager.ensureRootAgentsAndWait()

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

// writeUsersClaudeConversation files the conversation the user's own claude
// keeps in the root's checkout: the same project directory and cwd as the
// root's, and newer than anything the root wrote.
func writeUsersClaudeConversation(t *testing.T, configDir, repoPath string) string {
	t.Helper()
	path := writeRootClaudeTranscript(t, configDir, repoPath, usersConversationID)
	newer := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(path, newer, newer))
	return path
}

const usersConversationID = "5299e00d-1111-4222-8333-f7045e07a242"

// TestEnsureRootAgentsCarriesTheRootsUnfiledConversationNotTheUsers is #7222:
// the root was launched --session-id R and nobody typed into it, so Claude
// never filed R, while the user's own claude in the same checkout filed U. The
// heal carries R, the root's own record; U shares the root's directory, which
// says nothing about whose it is, and the root must never resume the user's
// personal conversation.
func TestEnsureRootAgentsCarriesTheRootsUnfiledConversationNotTheUsers(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	claudeConfigDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeConfigDir)
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	writeUsersClaudeConversation(t, claudeConfigDir, repoPath)

	manager, warning := newManagerCapturingWarnings(t, rootTestConfig(repoPath, config.RootAgentConfig{}))
	manager.ensureRootAgentsAndWait()
	first := findRootInstance(t, manager, repoPath)
	require.NotNil(t, first)
	seedRootConversation(t, first)
	warning.Reset()

	first.SetStatusForTest(session.Lost)
	manager.ensureRootAgentsAndWait()

	require.Len(t, *seen, 2)
	require.Equal(t, priorRootConversationID, (*seen)[1].ResumeConversation.ID,
		"the heal carries the root's own record, which its first launch starts on")
	require.NotContains(t, warning.String(), usersConversationID)
}

// TestEnsureRootAgentsKeepsALiveRootsConversationWhenItsTranscriptVanishes:
// R's transcript is gone from under a live root (Claude's retention, or a
// hand), and the user's U is the newest conversation in the checkout. Claude
// 2.1 never continues a conversation under a new id, so U is not R's
// continuation: the live root's record stays R, however many poll intervals
// pass, and the next heal carries R (#7222).
func TestEnsureRootAgentsKeepsALiveRootsConversationWhenItsTranscriptVanishes(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	advance := withFrozenClock(t)
	claudeConfigDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeConfigDir)
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	require.NoError(t, err)
	manager.ensureRootAgentsAndWait()
	root := findRootInstance(t, manager, repoPath)
	require.NotNil(t, root)
	seedRootConversation(t, root)
	recorded := writeRootClaudeTranscript(t, claudeConfigDir, repoPath, priorRootConversationID)
	writeUsersClaudeConversation(t, claudeConfigDir, repoPath)

	require.NoError(t, os.Remove(recorded))
	for range 3 {
		manager.ensureRootAgentsAndWait()
		advance(time.Hour)
	}
	manager.ensureRootAgentsAndWait()

	require.Len(t, *seen, 1, "a live root is not restarted")
	require.Equal(t, priorRootConversationID, root.AgentConversation().ID,
		"the user's newer conversation is not the root's")
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	require.NotEqual(t, usersConversationID, persistedConversationID(t, repo.ID))

	root.SetStatusForTest(session.Lost)
	manager.ensureRootAgentsAndWait()
	require.Len(t, *seen, 2)
	require.Equal(t, priorRootConversationID, (*seen)[1].ResumeConversation.ID)
}

// TestEnsureRootAgentsFallsBackFreshNotToTheUsersConversation: the carried
// create on R fails, and by then R's transcript is gone and the user's U is in
// the checkout. The always-on retry is a fresh agent, never U (#7222).
func TestEnsureRootAgentsFallsBackFreshNotToTheUsersConversation(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	claudeConfigDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeConfigDir)
	repoPath := setupControlRepo(t)
	recorded := writeRootClaudeTranscript(t, claudeConfigDir, repoPath, priorRootConversationID)

	var seen []session.InstanceOptions
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, _ string) (session.Backend, error) {
		seen = append(seen, opts)
		fake := session.NewFakeBackend()
		fake.CompleteStart()
		if opts.ResumeConversation.ID == priorRootConversationID {
			require.NoError(t, os.Remove(recorded))
			writeUsersClaudeConversation(t, claudeConfigDir, repoPath)
			return unresumableCarryBackend{readyFakeBackend{fake}}, nil
		}
		return readyFakeBackend{fake}, nil
	})
	t.Cleanup(restore)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	require.NoError(t, err)
	manager.ensureRootAgentsAndWait()
	first := findRootInstance(t, manager, repoPath)
	require.NotNil(t, first)
	seedRootConversation(t, first)

	first.SetStatusForTest(session.Lost)
	manager.ensureRootAgentsAndWait()

	require.Len(t, seen, 3)
	require.Equal(t, priorRootConversationID, seen[1].ResumeConversation.ID)
	require.False(t, seen[2].ResumeConversation.HasID(),
		"the retry after a failed carry is a fresh agent, not the user's conversation")
	require.NotNil(t, findRootInstance(t, manager, repoPath))
}

// plannedCreateBackend is a ready fake whose start makes and commits the
// conversation plan a real local create makes, so a heal's note and log line
// classify the launch af would actually make, not a stand-in for it.
type plannedCreateBackend struct {
	readyFakeBackend
}

func (b plannedCreateBackend) Start(instance *session.Instance, firstTimeSetup bool) error {
	// In the real launch's order: binding the agent's tmux session is what
	// materializes the agent tab, and only then is the planned conversation
	// committed to it. A commit with no agent tab yet records nothing.
	instance.SetTmuxSession(tmux.NewTmuxSession(instance.Title, tmux.ProgramClaude))
	if err := instance.CommitCreateConversationForTest(tmux.ProgramClaude); err != nil {
		return err
	}
	return b.readyFakeBackend.Start(instance, firstTimeSetup)
}

// TestEnsureRootAgentsReportsAHealThatStartsOnItsRecordAsFreshContext is #7226
// review R1, end to end: a heal whose launch starts Claude on R, because no
// store holds R's transcript any more, comes back with none of R's history, so
// the row's note and the log both say "fresh context". A heal that resumes R
// says it resumed.
func TestEnsureRootAgentsReportsAHealThatStartsOnItsRecordAsFreshContext(t *testing.T) {
	for _, tc := range []struct {
		name        string
		filed       bool
		want        session.RootRecreateContext
		wantInfo    string
		wantWarning string
	}{
		{
			name:        "R no store holds",
			want:        session.RootRecreateContextFresh,
			wantWarning: "could not resume its prior claude conversation " + priorRootConversationID + ", because no transcript store holds it any more; it started a new conversation on that id and starts with a fresh context",
		},
		{
			name:     "R a store holds",
			filed:    true,
			want:     session.RootRecreateContextNone,
			wantInfo: "resumed its prior claude conversation " + priorRootConversationID,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			// Every store the judge counts is this test's own: CLAUDE_CONFIG_DIR,
			// and $HOME/.claude, which the daemon package's shared home would
			// otherwise make whatever an earlier test left there.
			t.Setenv("HOME", t.TempDir())
			claudeConfigDir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", claudeConfigDir)
			restore := session.SetBackendFactoryForTest(func(session.InstanceOptions, string) (session.Backend, error) {
				fake := session.NewFakeBackend()
				fake.CompleteStart()
				return plannedCreateBackend{readyFakeBackend{fake}}, nil
			})
			t.Cleanup(restore)
			repoPath := setupControlRepo(t)
			if tc.filed {
				writeRootClaudeTranscript(t, claudeConfigDir, repoPath, priorRootConversationID)
			}

			manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
			require.NoError(t, err)
			manager.ensureRootAgentsAndWait()
			first := findRootInstance(t, manager, repoPath)
			require.NotNil(t, first)
			seedRootConversation(t, first)
			info, warning := captureInfo(t), captureWarnings(t)

			first.SetStatusForTest(session.Lost)
			manager.ensureRootAgentsAndWait()

			root := findRootInstance(t, manager, repoPath)
			require.NotNil(t, root)
			require.NotSame(t, first, root, "the heal replaced the record")
			require.NotEmpty(t, root.GetWorktreePath(),
				"the plan judged the carried id from the checkout, as a real create does, not blind")
			require.Equal(t, priorRootConversationID, root.AgentConversation().ID, "the root keeps its record either way")
			require.Equal(t, tc.want, root.RootRecreateContext())
			if tc.wantWarning != "" {
				require.Contains(t, warning.String(), tc.wantWarning)
				require.NotContains(t, info.String(), "resumed its prior")
			} else {
				require.Contains(t, info.String(), tc.wantInfo)
				require.NotContains(t, warning.String(), "fresh context")
			}
		})
	}
}

// TestReportRootConversationCarryDistinguishesTheThreeOutcomes: whichever way
// the carry goes, the log must SAY which. A fresh agent is a legitimate outcome
// when the prior conversation cannot be recovered — but if it reads the same as
// a successful carry-over, #2616 just becomes invisible again in a new form.
//
// The verdict comes from session.ClassifyRootRecreateContext, the same call that
// decides the note on the row (#2629), so these cases double as the log half of
// that classification — including the launched agent that separates a repointed
// root from a command that picked its own conversation.
func TestReportRootConversationCarryDistinguishesTheThreeOutcomes(t *testing.T) {
	prior := session.AgentConversationData{Agent: tmux.ProgramClaude, ID: priorRootConversationID}

	tests := []struct {
		name        string
		carried     session.AgentConversationData
		created     *session.AgentConversationData
		launched    string
		startedOn   bool
		wantInfo    string
		wantWarning string
	}{
		{
			name:        "nothing recorded to carry",
			carried:     session.AgentConversationData{},
			created:     nil,
			launched:    tmux.ProgramClaude,
			wantWarning: "had no recorded conversation to carry",
		},
		{
			name:     "carried",
			carried:  prior,
			created:  &session.AgentConversationData{Agent: tmux.ProgramClaude, ID: priorRootConversationID},
			launched: tmux.ProgramClaude,
			wantInfo: "resumed its prior claude conversation " + priorRootConversationID,
		},
		{
			name:        "resolved command pins its own conversation",
			carried:     prior,
			created:     nil,
			launched:    tmux.ProgramClaude,
			wantWarning: "context continuity is unknown",
		},
		{
			// A repointed root_agents program: the launch runs codex, so the claude
			// conversation is provably unresumable. Naming BOTH agents is what makes
			// the cause diagnosable from the log alone.
			name:        "the root now runs a different agent",
			carried:     prior,
			created:     nil,
			launched:    tmux.ProgramCodex,
			wantWarning: "now runs codex, so its prior claude conversation " + priorRootConversationID + " cannot be resumed",
		},
		{
			// No store held the carried transcript, so the launch started a new
			// conversation on its id (#7226 review R1): the same id, no history.
			name:        "a new conversation on the carried id",
			carried:     prior,
			created:     &session.AgentConversationData{Agent: tmux.ProgramClaude, ID: priorRootConversationID},
			launched:    tmux.ProgramClaude,
			startedOn:   true,
			wantWarning: "could not resume its prior claude conversation " + priorRootConversationID + ", because no transcript store holds it any more; it started a new conversation on that id and starts with a fresh context",
		},
		{
			name:        "recorded but not resumed",
			carried:     prior,
			created:     &session.AgentConversationData{Agent: tmux.ProgramClaude, ID: "5299e00d-1111-4222-8333-f7045e07a242"},
			launched:    tmux.ProgramClaude,
			wantWarning: "did not come up on its prior claude conversation " + priorRootConversationID,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			info, warning := captureInfo(t), captureWarnings(t)

			outcome := session.ClassifyRootRecreateContext(tc.carried, tc.created, tc.launched, tc.startedOn)
			reportRootConversationCarry("/repo", outcome, tc.carried, tc.created, tc.launched)

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
