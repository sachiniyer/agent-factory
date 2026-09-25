package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// symlinkLaunchDirs sets up a real directory and a symlink to it, returning
// (linkDir, resolvedDir) where resolvedDir is filepath.EvalSymlinks(linkDir).
// It asserts the two yield distinct claudeProjectName values, which is the
// condition the symlink bug turns on: Claude files under the kernel-resolved
// path while af records the link.
func symlinkLaunchDirs(t *testing.T) (linkDir, resolvedDir string) {
	t.Helper()
	realDir := filepath.Join(t.TempDir(), "real")
	require.NoError(t, os.MkdirAll(realDir, 0o700))
	linkDir = filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(realDir, linkDir))
	resolved, err := filepath.EvalSymlinks(linkDir)
	require.NoError(t, err)
	require.NotEqual(t, linkDir, resolved, "symlink must resolve to a distinct path")
	require.NotEqual(t, claudeProjectName(linkDir), claudeProjectName(resolved),
		"link and resolved paths must yield distinct claude project names")
	return linkDir, resolved
}

// writeClaudeTranscript writes a direct conversation file under the project
// directory claudeProjectName(projectBase) inside configDir and returns its
// path. A zero modTime leaves the file's write time as-is.
func writeClaudeTranscript(t *testing.T, configDir, projectBase, id string, modTime time.Time) string {
	t.Helper()
	dir := filepath.Join(configDir, "projects", claudeProjectName(projectBase))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, id+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
	if !modTime.IsZero() {
		require.NoError(t, os.Chtimes(path, modTime, modTime))
	}
	return path
}

// TestInspectClaudeProjectConversationsSymlinkedLaunchDirFindsTranscriptUnderResolvedPath
// is the core regression for the symlink bug: Claude files transcripts under
// the kernel-resolved (EvalSymlinks) project name, but the launch command
// records the symlink. The inspector must follow the resolved path and find
// the recorded transcript instead of reporting it missing and starting fresh.
func TestInspectClaudeProjectConversationsSymlinkedLaunchDirFindsTranscriptUnderResolvedPath(t *testing.T) {
	configDir := t.TempDir()
	linkDir, resolvedDir := symlinkLaunchDirs(t)
	repoDir := t.TempDir()
	const id = "5299e00d-1111-4222-8333-f7045e07a242"
	writeClaudeTranscript(t, configDir, resolvedDir, id, time.Time{})

	state, err := InspectClaudeProjectConversations(
		"env -C "+linkDir+" CLAUDE_CONFIG_DIR="+configDir+" claude", repoDir,
		AgentConversationData{Agent: tmux.ProgramClaude, ID: id},
	)
	require.NoError(t, err)
	require.True(t, state.RecordedExists,
		"inspector must find the transcript filed under the resolved-path project name when the launch directory is a symlink")
	require.Equal(t, id, state.Resume.ID)
	require.Equal(t, ConversationCaptureClaudeTranscript, state.Resume.CaptureKind)
}

// TestInspectClaudeProjectConversationsSymlinkedLaunchDirFindsTranscriptUnderLinkPath
// confirms the raw (link) candidate is still scanned: a transcript filed under
// the symlink-path project name is found via the first candidate, so the
// dual-candidate expansion does not regress the as-recorded lookup.
func TestInspectClaudeProjectConversationsSymlinkedLaunchDirFindsTranscriptUnderLinkPath(t *testing.T) {
	configDir := t.TempDir()
	linkDir, _ := symlinkLaunchDirs(t)
	repoDir := t.TempDir()
	const id = "5299e00d-2222-4222-8333-f7045e07a242"
	writeClaudeTranscript(t, configDir, linkDir, id, time.Time{})

	state, err := InspectClaudeProjectConversations(
		"env -C "+linkDir+" CLAUDE_CONFIG_DIR="+configDir+" claude", repoDir,
		AgentConversationData{Agent: tmux.ProgramClaude, ID: id},
	)
	require.NoError(t, err)
	require.True(t, state.RecordedExists)
	require.Equal(t, id, state.Resume.ID)
}

// TestInspectClaudeProjectConversationsSymlinkedLaunchDirSubstitutesNewestUnderResolvedPath
// checks the restore-path fallback: when the recorded id is not on disk but a
// replacement transcript exists under the resolved-path project name, the
// inspector substitutes it rather than reporting an empty Resume (which would
// force a fresh root).
func TestInspectClaudeProjectConversationsSymlinkedLaunchDirSubstitutesNewestUnderResolvedPath(t *testing.T) {
	configDir := t.TempDir()
	linkDir, resolvedDir := symlinkLaunchDirs(t)
	repoDir := t.TempDir()
	const newest = "5299e00d-3333-4222-8333-f7045e07a242"
	writeClaudeTranscript(t, configDir, resolvedDir, newest, time.Time{})

	state, err := InspectClaudeProjectConversations(
		"env -C "+linkDir+" CLAUDE_CONFIG_DIR="+configDir+" claude", repoDir,
		AgentConversationData{Agent: tmux.ProgramClaude, ID: "11111111-1111-1111-1111-111111111111"},
	)
	require.NoError(t, err)
	require.False(t, state.RecordedExists, "the recorded id is not on disk")
	require.Equal(t, newest, state.Resume.ID,
		"the fallback must substitute the newest transcript found under the resolved-path project name")
}

// TestInspectClaudeProjectConversationsSymlinkedLaunchDirPicksNewestAcrossBothCandidates
// verifies the fallback newest-transcript selection spans both candidate
// project directories: an older transcript under the link-path project name
// must not outrank a newer one under the resolved-path project name.
func TestInspectClaudeProjectConversationsSymlinkedLaunchDirPicksNewestAcrossBothCandidates(t *testing.T) {
	configDir := t.TempDir()
	linkDir, resolvedDir := symlinkLaunchDirs(t)
	repoDir := t.TempDir()
	const older = "5299e00d-4444-4222-8333-f7045e07a242"
	const newer = "5299e00d-5555-4222-8333-f7045e07a242"
	oldTime := time.Now().Add(-time.Hour)
	writeClaudeTranscript(t, configDir, linkDir, older, oldTime)
	writeClaudeTranscript(t, configDir, resolvedDir, newer, oldTime.Add(time.Minute))

	state, err := InspectClaudeProjectConversations(
		"env -C "+linkDir+" CLAUDE_CONFIG_DIR="+configDir+" claude", repoDir,
		AgentConversationData{Agent: tmux.ProgramClaude, ID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"},
	)
	require.NoError(t, err)
	require.False(t, state.RecordedExists)
	require.Equal(t, newer, state.Resume.ID,
		"the fallback must choose the newest transcript across both the link-path and resolved-path project directories")
}

// TestInspectClaudeProjectConversationsSymlinkedLaunchDirHasNoResumeWhenNoTranscriptAnywhere
// confirms both candidate directories being absent still yields an empty
// Resume (no error), so the restore path keeps its documented fresh-start
// behavior when no transcript exists under either spelling.
func TestInspectClaudeProjectConversationsSymlinkedLaunchDirHasNoResumeWhenNoTranscriptAnywhere(t *testing.T) {
	configDir := t.TempDir()
	linkDir, _ := symlinkLaunchDirs(t)
	repoDir := t.TempDir()

	state, err := InspectClaudeProjectConversations(
		"env -C "+linkDir+" CLAUDE_CONFIG_DIR="+configDir+" claude", repoDir,
		AgentConversationData{Agent: tmux.ProgramClaude, ID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"},
	)
	require.NoError(t, err)
	require.False(t, state.RecordedExists)
	require.False(t, state.Resume.HasID(),
		"with no transcript under either candidate, Resume must be empty so the restore path starts fresh")
}

// TestInspectClaudeProjectConversationsSymlinkedLaunchDirRecordedIdBeatsNewerFallbackAcrossCandidates
// pins the cross-directory precedence: a recorded id found under either
// candidate is preserved even when a newer transcript exists under the other
// candidate, so a newer same-project transcript is not mistaken for evidence
// about the recorded root process.
func TestInspectClaudeProjectConversationsSymlinkedLaunchDirRecordedIdBeatsNewerFallbackAcrossCandidates(t *testing.T) {
	configDir := t.TempDir()
	linkDir, resolvedDir := symlinkLaunchDirs(t)
	repoDir := t.TempDir()
	const recordedID = "5299e00d-6666-4222-8333-f7045e07a242"
	const newerID = "5299e00d-7777-4222-8333-f7045e07a242"
	oldTime := time.Now().Add(-time.Hour)
	writeClaudeTranscript(t, configDir, resolvedDir, recordedID, oldTime)
	writeClaudeTranscript(t, configDir, linkDir, newerID, oldTime.Add(time.Hour))

	state, err := InspectClaudeProjectConversations(
		"env -C "+linkDir+" CLAUDE_CONFIG_DIR="+configDir+" claude", repoDir,
		AgentConversationData{Agent: tmux.ProgramClaude, ID: recordedID},
	)
	require.NoError(t, err)
	require.True(t, state.RecordedExists)
	require.Equal(t, recordedID, state.Resume.ID,
		"a recorded id found under either candidate must be preserved even if a newer transcript exists under the other candidate")
}

// TestInspectClaudeProjectConversationsSymlinkedLaunchDirLaunchDirEvalFailureStaysSingleCandidate
// guards the EvalSymlinks error path: when the env -C target does not exist on
// disk (so EvalSymlinks fails), the inspector falls back to the single recorded
// candidate and behaves as before, rather than erroring or silently dropping
// the lookup.
func TestInspectClaudeProjectConversationsSymlinkedLaunchDirLaunchDirEvalFailureStaysSingleCandidate(t *testing.T) {
	configDir := t.TempDir()
	repoDir := t.TempDir()
	// A launch directory that does not exist on disk. EvalSymlinks fails on it,
	// so only the single recorded candidate is scanned.
	missingLaunchDir := filepath.Join(t.TempDir(), "absent")
	const id = "5299e00d-8888-4222-8333-f7045e07a242"
	writeClaudeTranscript(t, configDir, missingLaunchDir, id, time.Time{})

	state, err := InspectClaudeProjectConversations(
		"env -C "+missingLaunchDir+" CLAUDE_CONFIG_DIR="+configDir+" claude", repoDir,
		AgentConversationData{Agent: tmux.ProgramClaude, ID: id},
	)
	require.NoError(t, err)
	require.True(t, state.RecordedExists,
		"when EvalSymlinks of the launch directory fails, the single recorded candidate must still be scanned")
	require.Equal(t, id, state.Resume.ID)
}

// TestInspectClaudeProjectConversationsSymlinkedAncestorFindsTranscriptUnderRecordedPath
// simulates the macOS /var -> /private/var temp-dir layout, where the launch
// directory's ANCESTOR is a symlink so filepath.EvalSymlinks rewrites the whole
// path even though the launch directory itself is not a link. The existing
// TestInspectClaudeProjectConversationsUsesEffectiveLaunchDirectory writes the
// transcript under the as-recorded (unresolved) project name; the resolved-path
// candidate's project directory does not exist. This test pins that the
// dual-candidate expansion does not regress that case: the first candidate is
// still scanned, the non-existent second candidate adds nothing, and the
// transcript is found exactly as before the fix.
func TestInspectClaudeProjectConversationsSymlinkedAncestorFindsTranscriptUnderRecordedPath(t *testing.T) {
	configDir := t.TempDir()
	repoDir := t.TempDir()
	// Build an ancestor-symlink tree: <root>/private/var/folders/actual is the
	// real directory; <root>/var is a symlink to <root>/private/var. The launch
	// directory <root>/var/folders/actual therefore has EvalSymlinks different
	// from itself, even though its final component is not a link.
	root := t.TempDir()
	realAncestor := filepath.Join(root, "private", "var")
	require.NoError(t, os.MkdirAll(filepath.Join(realAncestor, "folders"), 0o700))
	linkAncestor := filepath.Join(root, "var")
	require.NoError(t, os.Symlink(realAncestor, linkAncestor))
	launchDir := filepath.Join(linkAncestor, "folders", "actual")
	require.NoError(t, os.MkdirAll(launchDir, 0o700))
	resolved, err := filepath.EvalSymlinks(launchDir)
	require.NoError(t, err)
	require.NotEqual(t, launchDir, resolved,
		"the ancestor symlink must make EvalSymlinks rewrite the path, as macOS /var does")

	const id = "5299e00d-9999-4222-8333-f7045e07a242"
	// Transcript filed under the as-recorded (link-side) project name, mirroring
	// how TestInspectClaudeProjectConversationsUsesEffectiveLaunchDirectory sets
	// up its evidence. The resolved-path project dir is never populated.
	writeClaudeTranscript(t, configDir, launchDir, id, time.Time{})

	state, err := InspectClaudeProjectConversations(
		"env -C "+launchDir+" CLAUDE_CONFIG_DIR="+configDir+" claude", repoDir,
		AgentConversationData{Agent: tmux.ProgramClaude, ID: id},
	)
	require.NoError(t, err)
	require.True(t, state.RecordedExists,
		"the first (as-recorded) candidate must still find the transcript when an ancestor symlink makes the resolved candidate differ")
	require.Equal(t, id, state.Resume.ID)
}
