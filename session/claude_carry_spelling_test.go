package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Claude may file a worktree's conversations under either spelling of its path:
// as recorded, and as the kernel resolves it through a symlinked parent
// (claudeProjectCandidates). Every check that reads the worktree's
// conversations must read both, or a conversation filed under the other one is
// invisible to it (#7177).

// symlinkedClaudeCarryInstance is carrySwapInstance for Claude in a worktree
// reached through a symlinked parent. It returns the instance and the project
// directories, relative to a Claude config dir, of the worktree's two
// spellings: as recorded first, then as resolved.
func symlinkedClaudeCarryInstance(t *testing.T) (*Instance, string, string) {
	t.Helper()
	inst, _ := carrySwapInstance(t, tmux.ProgramClaude)
	real := filepath.Join(testguard.CanonicalTempDir(t), "real")
	require.NoError(t, os.MkdirAll(filepath.Join(real, "repo"), 0o755))
	link := filepath.Join(testguard.CanonicalTempDir(t), "link")
	require.NoError(t, os.Symlink(real, link))
	inst.Path = filepath.Join(link, "repo")
	cmd := exec.Command("git", "init")
	cmd.Dir = inst.Path
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)

	projects := claudeProjectCandidates(inst.GetWorktreePath())
	require.Len(t, projects, 2, "the worktree must have two project-directory spellings")
	require.Equal(t, filepath.Join("projects", claudeProjectName(filepath.Join(link, "repo"))), projects[0])
	require.Equal(t, filepath.Join("projects", claudeProjectName(filepath.Join(real, "repo"))), projects[1])
	return inst, projects[0], projects[1]
}

func TestClaudeCarryStalenessReadsEveryProjectSpelling(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	// file writes a transcript for id under project at modTime.
	file := func(t *testing.T, source, project, id string, at time.Time) {
		t.Helper()
		setModTime(t, writeCarryFile(t, source, filepath.Join(project, id+".jsonl"), "{}\n"), at)
	}
	for _, tc := range []struct {
		name   string
		layout func(t *testing.T, source, recorded, resolved string)
		// refusal is the fallback the swap states; "" means it carries.
		refusal string
		why     string
	}{
		{
			name: "a newer conversation under the resolved spelling",
			layout: func(t *testing.T, source, recorded, resolved string) {
				file(t, source, recorded, carryTestID, past)
				file(t, source, resolved, newerConversationID, past.Add(time.Minute))
			},
			refusal: staleCarryReason,
			why:     "a /clear filed under the other spelling is still a newer conversation in this worktree",
		},
		{
			name: "a newer conversation under the recorded spelling",
			layout: func(t *testing.T, source, recorded, resolved string) {
				file(t, source, resolved, carryTestID, past)
				file(t, source, recorded, newerConversationID, past.Add(time.Minute))
			},
			refusal: staleCarryReason,
			why:     "the spelling the recorded transcript was found under is not the only one Claude writes",
		},
		{
			name: "a newer conversation under the same spelling",
			layout: func(t *testing.T, source, recorded, resolved string) {
				file(t, source, resolved, carryTestID, past)
				file(t, source, resolved, newerConversationID, past.Add(time.Minute))
			},
			refusal: staleCarryReason,
			why:     "the single-spelling /clear is still refused",
		},
		{
			name: "no newer conversation under either spelling",
			layout: func(t *testing.T, source, recorded, resolved string) {
				file(t, source, recorded, carryTestID, past.Add(time.Minute))
				file(t, source, resolved, newerConversationID, past)
			},
			why: "an older conversation under the other spelling does not block the carry",
		},
		{
			name: "a newer copy of the recorded conversation under the other spelling",
			layout: func(t *testing.T, source, recorded, resolved string) {
				file(t, source, recorded, carryTestID, past)
				file(t, source, resolved, carryTestID, past.Add(time.Minute))
			},
			refusal: carryTranscriptCopiesReason,
			why:     "carrying the older copy would resume the conversation without its latest turns",
		},
		{
			name: "an older copy of the recorded conversation under the other spelling",
			layout: func(t *testing.T, source, recorded, resolved string) {
				file(t, source, recorded, carryTestID, past.Add(time.Minute))
				file(t, source, resolved, carryTestID, past)
			},
			why: "the copy the carry takes is the newest one",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := carryAmbientProviderHomes(t)
			inst, recorded, resolved := symlinkedClaudeCarryInstance(t)
			recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
			tc.layout(t, filepath.Join(home, ".claude"), recorded, resolved)

			// require_carry's check-ahead runs the same plan without a target.
			if tc.refusal == "" {
				require.Empty(t, inst.AccountSwapCarryRefusal(), tc.why)
			} else {
				require.Equal(t, carryRefusalCode(tc.refusal), inst.AccountSwapCarryRefusal(), tc.why)
			}

			require.NoError(t, inst.ValidateAccountSwap("work"))
			if tc.refusal == "" {
				require.NotNil(t, inst.accountSwapLaunch.carry, tc.why)
				require.Empty(t, inst.accountSwapLaunch.carryFallback, tc.why)
				return
			}
			require.Nil(t, inst.accountSwapLaunch.carry, tc.why)
			require.Equal(t, tc.refusal, inst.accountSwapLaunch.carryFallback, tc.why)
		})
	}
}

// The root agent's restore and a headless workspace's resume check read the
// same store, and must find a conversation under either spelling too.
func TestInspectClaudeProjectConversationsReadsEveryProjectSpelling(t *testing.T) {
	configDir := t.TempDir()
	real := filepath.Join(testguard.CanonicalTempDir(t), "real")
	require.NoError(t, os.MkdirAll(real, 0o700))
	link := filepath.Join(testguard.CanonicalTempDir(t), "link")
	require.NoError(t, os.Symlink(real, link))
	projects := claudeProjectCandidates(link)
	require.Len(t, projects, 2)
	recordedProject, resolvedProject := projects[0], projects[1]

	const recordedID = "64ea06ed-ed87-4b12-bc2d-2a2102a81ba5"
	const otherID = "5299e00d-1111-4222-8333-f7045e07a242"
	const newestID = "64ea06ed-ed87-4b12-bc2d-2a2102a81ba6"
	past := time.Now().Add(-time.Hour)
	write := func(project, id string, at time.Time) string {
		t.Helper()
		path := writeCarryFile(t, configDir, filepath.Join(project, id+".jsonl"), "{}\n")
		setModTime(t, path, at)
		return path
	}
	inspect := func() ClaudeProjectConversationState {
		t.Helper()
		state, err := InspectClaudeProjectConversations("CLAUDE_CONFIG_DIR="+configDir+" claude", link,
			AgentConversationData{Agent: tmux.ProgramClaude, ID: recordedID})
		require.NoError(t, err)
		return state
	}

	recorded := write(resolvedProject, recordedID, past)
	write(recordedProject, otherID, past.Add(-time.Minute))
	state := inspect()
	require.True(t, state.RecordedExists,
		"a recorded conversation filed under the resolved spelling still exists; replacing it resumes another one")
	require.Equal(t, recordedID, state.Resume.ID)

	require.NoError(t, os.Remove(recorded))
	write(resolvedProject, newestID, past)
	state = inspect()
	require.False(t, state.RecordedExists)
	require.Equal(t, newestID, state.Resume.ID,
		"the newest conversation in the worktree is filed under the resolved spelling")
}
