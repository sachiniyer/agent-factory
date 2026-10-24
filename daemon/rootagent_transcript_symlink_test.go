package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// symlinkEnvCDirs sets up a real directory and a symlink to it, returning
// (linkDir, resolvedDir). It asserts the two yield distinct claudeProjectName
// values, which is the condition the symlink bug turns on: Claude files under
// the kernel-resolved path while af records the link as the launch directory.
func symlinkEnvCDirs(t *testing.T) (linkDir, resolvedDir string) {
	t.Helper()
	realDir := filepath.Join(t.TempDir(), "real")
	require.NoError(t, os.MkdirAll(realDir, 0o700))
	linkDir = filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(realDir, linkDir))
	resolved, err := filepath.EvalSymlinks(linkDir)
	require.NoError(t, err)
	require.NotEqual(t, linkDir, resolved, "symlink must resolve to a distinct path")
	return linkDir, resolved
}

// symlinkedClaudeProgram builds the program_overrides command an operator would
// use to embed a symlinked env -C chdir in the root-agent program.
func symlinkedClaudeProgram(linkDir, configDir string) string {
	return "env -C " + linkDir + " CLAUDE_CONFIG_DIR=" + configDir + " claude"
}

// TestEnsureRootAgentsCarriesClaudeConversationAcrossTmuxVanishWithSymlinkedEnvC
// is the daemon-level regression for the symlink bug (G11): a root configured
// with a symlinked `env -C <link>` chdir files its Claude transcripts under the
// kernel-resolved project name, but af records the link as the launch directory.
// After a tmux vanish reaps the root, the restore path must find the transcript
// under the resolved-path project name and resume the recorded conversation,
// not start a fresh root. With the single-candidate inspector this fails:
// RecordedExists is false, no replacement exists, and skipRecordedResume clears
// the carry.
func TestEnsureRootAgentsCarriesClaudeConversationAcrossTmuxVanishWithSymlinkedEnvC(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	claudeConfigDir := t.TempDir()
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	linkDir, resolved := symlinkEnvCDirs(t)

	cfg := rootTestConfig(repoPath, config.RootAgentConfig{Program: tmux.ProgramClaude})
	cfg.ProgramOverrides = map[string]string{
		tmux.ProgramClaude: symlinkedClaudeProgram(linkDir, claudeConfigDir),
	}
	require.NoError(t, config.SaveConfig(cfg))

	// Claude files this transcript under the resolved-path project name, which
	// is what the OS getcwd reports after chdir into the symlink.
	writeRootClaudeTranscript(t, claudeConfigDir, resolved, priorRootConversationID)

	manager, err := NewManager(cfg)
	require.NoError(t, err)
	manager.ensureRootAgentsAndWait()

	first := findRootInstance(t, manager, repoPath)
	require.NotNil(t, first, "root instance missing after first ensure")
	require.Len(t, *seen, 1)
	require.False(t, (*seen)[0].ResumeConversation.HasID(),
		"a first-ever root create has no prior conversation to carry")
	prior := seedRootConversation(t, first)

	// tmux vanished under a healthy daemon — the #1104 outage class.
	first.SetStatusForTest(session.Lost)
	manager.ensureRootAgentsAndWait()

	require.Len(t, *seen, 2, "the vanished root must be reaped and re-created")
	carried := (*seen)[1].ResumeConversation
	require.Equal(t, prior.ID, carried.ID,
		"the re-created root must resume the conversation filed under the resolved-path project name, not start fresh")
	require.Equal(t, prior.Agent, carried.Agent)
	require.NotNil(t, findRootInstance(t, manager, repoPath), "always-ensure: the root must exist again")
}

// TestEnsureRootAgentsSubstitutesNewestClaudeTranscriptUnderSymlinkedEnvC is the
// daemon-level regression for the substitution path (G11 fallback): when the
// recorded id is not on disk but a newer transcript exists under the
// resolved-path project name, the restore path must substitute it rather than
// starting a fresh root.
func TestEnsureRootAgentsSubstitutesNewestClaudeTranscriptUnderSymlinkedEnvC(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	claudeConfigDir := t.TempDir()
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	linkDir, resolved := symlinkEnvCDirs(t)

	cfg := rootTestConfig(repoPath, config.RootAgentConfig{Program: tmux.ProgramClaude})
	cfg.ProgramOverrides = map[string]string{
		tmux.ProgramClaude: symlinkedClaudeProgram(linkDir, claudeConfigDir),
	}
	require.NoError(t, config.SaveConfig(cfg))

	const newestConversationID = "5299e00d-1111-4222-8333-f7045e07a242"
	writeRootClaudeTranscript(t, claudeConfigDir, resolved, newestConversationID)

	manager, err := NewManager(cfg)
	require.NoError(t, err)
	manager.ensureRootAgentsAndWait()
	first := findRootInstance(t, manager, repoPath)
	require.NotNil(t, first)
	prior := seedRootConversation(t, first)

	first.SetStatusForTest(session.Lost)
	manager.ensureRootAgentsAndWait()

	require.GreaterOrEqual(t, len(*seen), 2)
	carried := (*seen)[len(*seen)-1].ResumeConversation
	require.True(t, carried.HasID(),
		"a recoverable on-disk transcript under the resolved-path project name must be tried before a fresh root")
	require.Equal(t, newestConversationID, carried.ID,
		"the replacement root must resume the newest existing transcript filed under the resolved path")
	require.NotEqual(t, prior.ID, carried.ID, "the recorded transcript is not on disk")
}

// TestRefreshRootClaudeConversationReplacesMissingTranscriptUnderSymlinkedEnvC
// is the daemon-level regression for the live poller (G10): a root configured
// with a symlinked `env -C <link>` must have its transcript verification
// functional. Before the fix the poller looked only under the symlink-path
// project name (which never has transcripts), so it could neither confirm the
// recorded id nor substitute a replacement — verification was silently disabled
// for the root's entire lifetime. After the fix the poller finds transcripts
// under the resolved-path project name and substitutes the newest when the
// recorded one disappears.
func TestRefreshRootClaudeConversationReplacesMissingTranscriptUnderSymlinkedEnvC(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	advance := withFrozenClock(t)
	claudeConfigDir := t.TempDir()
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	linkDir, resolved := symlinkEnvCDirs(t)
	program := symlinkedClaudeProgram(linkDir, claudeConfigDir)

	cfg := rootTestConfig(repoPath, config.RootAgentConfig{Program: tmux.ProgramClaude})
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: program}
	require.NoError(t, config.SaveConfig(cfg))

	manager, err := NewManager(cfg)
	require.NoError(t, err)
	manager.ensureRootAgentsAndWait()
	root := findRootInstance(t, manager, repoPath)
	require.NotNil(t, root)
	seedRootConversation(t, root)
	// The live pane runs the env -C command, so the poller inspects that.
	root.SetTmuxSession(tmux.NewTmuxSession(session.RootSessionTitle, program))

	// Recorded transcript plus a newer replacement, both under the resolved
	// path's project name (where Claude files them).
	oldPath := writeRootClaudeTranscript(t, claudeConfigDir, resolved, priorRootConversationID)
	const newestConversationID = "5299e00d-1111-4222-8333-f7045e07a242"
	newPath := writeRootClaudeTranscript(t, claudeConfigDir, resolved, newestConversationID)
	oldTime := time.Now().Add(-time.Minute)
	require.NoError(t, os.Chtimes(oldPath, oldTime, oldTime))
	require.NoError(t, os.Chtimes(newPath, oldTime.Add(time.Second), oldTime.Add(time.Second)))

	manager.ensureRootAgentsAndWait()
	require.Len(t, *seen, 1, "refreshing conversation metadata must not restart a healthy root")
	require.Equal(t, priorRootConversationID, root.AgentConversation().ID,
		"a still-valid root conversation must not be replaced by another process's newer transcript")

	// The recorded transcript disappears — the poller must substitute the newest.
	require.NoError(t, os.Remove(oldPath))
	manager.ensureRootAgentsAndWait()
	require.Equal(t, priorRootConversationID, root.AgentConversation().ID,
		"the ensure loop must not rescan project transcripts on every tick before the inspection interval")
	advance(time.Hour)
	manager.ensureRootAgentsAndWait()

	require.Equal(t, newestConversationID, root.AgentConversation().ID,
		"the poller must substitute the newest transcript filed under the resolved-path project name when the recorded one disappears")
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	require.Equal(t, newestConversationID, persistedConversationID(t, repo.ID),
		"the substituted id must be durably persisted")
}
