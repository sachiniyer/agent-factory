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
	inst, recorded, resolved, _ := symlinkedClaudeCarryInstanceNamed(t, "repo")
	return inst, recorded, resolved
}

// symlinkedClaudeCarryInstanceNamed is symlinkedClaudeCarryInstance for a
// worktree whose last path element is leaf. It also returns the real
// directory the symlinked parent points at.
func symlinkedClaudeCarryInstanceNamed(t *testing.T, leaf string) (*Instance, string, string, string) {
	t.Helper()
	inst, _ := carrySwapInstance(t, tmux.ProgramClaude)
	real := filepath.Join(testguard.CanonicalTempDir(t), "real")
	require.NoError(t, os.MkdirAll(filepath.Join(real, leaf), 0o755))
	link := filepath.Join(testguard.CanonicalTempDir(t), "link")
	require.NoError(t, os.Symlink(real, link))
	worktree := filepath.Join(link, leaf)
	cmd := exec.Command("git", "init")
	cmd.Dir = worktree
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	// A worktree af created for the session, apart from the repository's own
	// checkout, so a newer conversation in it is evidence (#7227).
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, worktree, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)

	projects := claudeProjectCandidates(inst.GetWorktreePath())
	require.Len(t, projects, 2, "the worktree must have two project-directory spellings")
	require.Equal(t, filepath.Join("projects", claudeProjectName(filepath.Join(link, leaf))), projects[0])
	require.Equal(t, filepath.Join("projects", claudeProjectName(filepath.Join(real, leaf))), projects[1])
	return inst, projects[0], projects[1], real
}

func TestClaudeCarryStalenessReadsEveryProjectSpelling(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	// cwd is the worktree each subtest's conversations were launched in.
	var cwd string
	// file writes a transcript for id under project at modTime.
	file := func(t *testing.T, source, project, id string, at time.Time) {
		t.Helper()
		setModTime(t, writeCarryFile(t, source, filepath.Join(project, id+".jsonl"), claudeRecordLine(t, cwd)), at)
	}
	for _, tc := range []struct {
		name   string
		layout func(t *testing.T, source, recorded, resolved string)
		// refusal is the fallback the swap states; "" means it carries.
		refusal string
		// from is the spelling whose copy a carry takes: "recorded" or
		// "resolved"; "" leaves it unchecked.
		from string
		why  string
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
			from: "recorded",
			why:  "an older conversation under the other spelling does not block the carry",
		},
		{
			name: "a newer copy of the recorded conversation under the other spelling",
			layout: func(t *testing.T, source, recorded, resolved string) {
				file(t, source, recorded, carryTestID, past)
				file(t, source, resolved, carryTestID, past.Add(time.Minute))
			},
			from: "resolved",
			why:  "the carry takes the copy Claude is appending to, not the first spelling that holds one (#7207)",
		},
		{
			name: "an older copy of the recorded conversation under the other spelling",
			layout: func(t *testing.T, source, recorded, resolved string) {
				file(t, source, recorded, carryTestID, past.Add(time.Minute))
				file(t, source, resolved, carryTestID, past)
			},
			from: "recorded",
			why:  "the copy the carry takes is the newest one",
		},
		{
			name: "a conversation newer than the older copy but older than the newest copy",
			layout: func(t *testing.T, source, recorded, resolved string) {
				file(t, source, recorded, carryTestID, past)
				file(t, source, recorded, newerConversationID, past.Add(time.Minute))
				file(t, source, resolved, carryTestID, past.Add(2*time.Minute))
			},
			from: "resolved",
			why:  "the conversation is judged by its newest copy, so the user did not move on (#7207)",
		},
		{
			name: "a conversation newer than every copy",
			layout: func(t *testing.T, source, recorded, resolved string) {
				file(t, source, recorded, carryTestID, past)
				file(t, source, resolved, carryTestID, past.Add(time.Minute))
				file(t, source, recorded, newerConversationID, past.Add(2*time.Minute))
			},
			refusal: staleCarryReason,
			why:     "a /clear after the newest copy is still a newer conversation",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := carryAmbientProviderHomes(t)
			inst, recorded, resolved := symlinkedClaudeCarryInstance(t)
			cwd = inst.GetWorktreePath()
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
				if project := map[string]string{"recorded": recorded, "resolved": resolved}[tc.from]; project != "" {
					require.Equal(t, filepath.Join(project, carryTestID+".jsonl"), inst.accountSwapLaunch.carry.transcript, tc.why)
					require.Equal(t, filepath.Join(project, carryTestID), inst.accountSwapLaunch.carry.aux, tc.why)
				}
				return
			}
			require.Nil(t, inst.accountSwapLaunch.carry, tc.why)
			require.Equal(t, tc.refusal, inst.accountSwapLaunch.carryFallback, tc.why)
		})
	}
}

// A headless workspace's resume check reads the same store, and must find a
// conversation under either spelling too.
func TestClaudeFiledHereReadsEveryProjectSpelling(t *testing.T) {
	configDir := t.TempDir()
	real := filepath.Join(testguard.CanonicalTempDir(t), "real")
	require.NoError(t, os.MkdirAll(real, 0o700))
	link := filepath.Join(testguard.CanonicalTempDir(t), "link")
	require.NoError(t, os.Symlink(real, link))
	projects := claudeProjectCandidates(link)
	require.Len(t, projects, 2)
	program := "CLAUDE_CONFIG_DIR=" + configDir + " claude"

	for _, project := range projects {
		const recordedID = "64ea06ed-ed87-4b12-bc2d-2a2102a81ba5"
		recorded := AgentConversationData{Agent: tmux.ProgramClaude, ID: recordedID}
		path := writeCarryFile(t, configDir, filepath.Join(project, recordedID+".jsonl"), claudeRecordLine(t, link))
		require.True(t, ConversationResumableHere(program, link, recorded),
			"a recorded conversation filed under %s exists", project)
		require.NoError(t, os.Remove(path))
		require.False(t, ConversationResumableHere(program, link, recorded))
	}
}
