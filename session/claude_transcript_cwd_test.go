package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/handoff"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// claudeRecordLine is one Claude transcript record of a conversation launched
// in cwd: the field ownership is judged by (#7208).
func claudeRecordLine(t testing.TB, cwd string) string {
	t.Helper()
	line, err := json.Marshal(map[string]any{
		"type": "user", "cwd": cwd, "sessionId": "x",
		"message": map[string]any{"role": "user", "content": "hi"},
	})
	require.NoError(t, err)
	return string(line) + "\n"
}

func TestClaudeTranscriptCwdReadsTheFirstRecordThatNamesOne(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}
	summary := `{"type":"summary","summary":"earlier work","leafUuid":"x"}` + "\n"

	cwd, ok := claudeTranscriptCwd(write("ordered.jsonl", summary+claudeRecordLine(t, "/work/a")+claudeRecordLine(t, "/work/b")))
	require.True(t, ok)
	require.Equal(t, "/work/a", cwd, "the first record that names a cwd is where the conversation was launched")

	_, ok = claudeTranscriptCwd(write("none.jsonl", summary+"{}\n"))
	require.False(t, ok)
	_, ok = claudeTranscriptCwd(write("relative.jsonl", `{"type":"user","cwd":"work"}`+"\n"))
	require.False(t, ok, "a relative cwd names no directory")
	_, ok = claudeTranscriptCwd(write("torn.jsonl", `{"type":"user","cwd":"/work/a"`))
	require.False(t, ok, "a record still being written names nothing yet")
	far := `{"type":"summary","summary":"` + strings.Repeat("x", claudeTranscriptHeadBudget) + `"}` + "\n"
	_, ok = claudeTranscriptCwd(write("far.jsonl", far+claudeRecordLine(t, "/work/a")))
	require.False(t, ok, "the read is bounded")
	_, ok = claudeTranscriptCwd(filepath.Join(dir, "missing.jsonl"))
	require.False(t, ok)
}

// #7208: Claude's project names collide (a.b and a-b both become a-b), so the
// project directory a worktree's spelling names can hold another directory's
// conversations. Those are neither a restore's substitute nor evidence that
// the user moved on; a conversation that names no directory af can read is not
// a substitute, and as a newer conversation it leaves "current" unconfirmed.
func TestClaudeOwnershipIsTheTranscriptsCwdNotItsProjectName(t *testing.T) {
	const otherID = "7d1c2b3a-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	past := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name string
		// cwd names the newer conversation's launch directory: "own", "own
		// resolved", "foreign", or "" for none.
		cwd string
		// verdict is what the swap and the restore conclude.
		verdict claudeRecordedVerdict
	}{
		{name: "this worktree's conversation", cwd: "own", verdict: claudeRecordedStale},
		{name: "this worktree's conversation, recorded resolved", cwd: "own resolved", verdict: claudeRecordedStale},
		{name: "a colliding directory's conversation", cwd: "foreign", verdict: claudeRecordedCurrent},
		{name: "a conversation that names no directory", cwd: "", verdict: claudeRecordedUncorrelated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := carryAmbientProviderHomes(t)
			inst, recorded, resolved, real := symlinkedClaudeCarryInstanceNamed(t, "a.b")
			worktree := inst.GetWorktreePath()
			foreign := filepath.Join(real, "a-b")
			require.NoError(t, os.MkdirAll(foreign, 0o755))
			require.Equal(t, resolved, claudeProjectCandidates(foreign)[0], "premise: a-b files under a.b's resolved project name")
			recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
			source := filepath.Join(home, ".claude")
			setModTime(t, writeCarryFile(t, source, filepath.Join(recorded, carryTestID+".jsonl"), claudeRecordLine(t, worktree)), past)
			content := map[string]string{
				"own":          claudeRecordLine(t, worktree),
				"own resolved": claudeRecordLine(t, filepath.Join(real, "a.b")),
				"foreign":      claudeRecordLine(t, foreign),
				"":             `{"type":"summary","summary":"s"}` + "\n",
			}[tc.cwd]
			setModTime(t, writeCarryFile(t, source, filepath.Join(resolved, otherID+".jsonl"), content), past.Add(time.Minute))

			require.Equal(t, tc.verdict,
				judgeClaudeRecorded(source, claudeProjectCandidates(worktree), worktree, carryTestID, ""))

			require.NoError(t, inst.ValidateAccountSwap("work"))
			switch tc.verdict {
			case claudeRecordedStale:
				require.Equal(t, staleCarryReason, inst.accountSwapLaunch.carryFallback)
			case claudeRecordedUncorrelated:
				require.Equal(t, carryUncorrelatedReason, inst.accountSwapLaunch.carryFallback)
				require.Equal(t, handoff.CarryUnverified, inst.AccountSwapCarryRefusal())
			default:
				require.NotNil(t, inst.accountSwapLaunch.carry, "another directory's conversation says nothing about this one")
			}

			// The restore refuses only on evidence (#7209 review R4): a newer
			// conversation that names no directory leaves it resuming, as a
			// spelling it cannot list does.
			restore := claudeRestoreInstance(t, worktree, carryTestID)
			t.Setenv("CLAUDE_CONFIG_DIR", source)
			_, ok, decline := prepareExactResumeConversation(restore, "claude")
			require.Equal(t, tc.verdict != claudeRecordedStale, ok, "%+v", decline)
		})
	}
}

// A restore's substitute for a missing recorded conversation is the newest one
// launched in this worktree, never a colliding directory's or one that names
// no directory.
func TestInspectClaudeSubstitutesOnlyThisWorktreesConversation(t *testing.T) {
	configDir := t.TempDir()
	real := filepath.Join(testguard.CanonicalTempDir(t), "real")
	require.NoError(t, os.MkdirAll(filepath.Join(real, "a.b"), 0o755))
	foreign := filepath.Join(real, "a-b")
	require.NoError(t, os.MkdirAll(foreign, 0o755))
	link := filepath.Join(testguard.CanonicalTempDir(t), "link")
	require.NoError(t, os.Symlink(real, link))
	worktree := filepath.Join(link, "a.b")
	projects := claudeProjectCandidates(worktree)
	require.Len(t, projects, 2)
	resolved := projects[1]

	const recordedID = "64ea06ed-ed87-4b12-bc2d-2a2102a81ba5"
	const ownID = "5299e00d-1111-4222-8333-f7045e07a242"
	const foreignID = "7d1c2b3a-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	const unnamedID = "01a1ccd0-1111-7222-8333-444455556666"
	past := time.Now().Add(-time.Hour)
	write := func(id, content string, at time.Time) {
		t.Helper()
		setModTime(t, writeCarryFile(t, configDir, filepath.Join(resolved, id+".jsonl"), content), at)
	}
	inspect := func() ClaudeProjectConversationState {
		t.Helper()
		state, err := InspectClaudeProjectConversations("CLAUDE_CONFIG_DIR="+configDir+" claude", worktree,
			AgentConversationData{Agent: tmux.ProgramClaude, ID: recordedID})
		require.NoError(t, err)
		require.False(t, state.RecordedExists)
		return state
	}

	write(foreignID, claudeRecordLine(t, foreign), past.Add(2*time.Minute))
	write(unnamedID, `{"type":"summary","summary":"s"}`+"\n", past.Add(3*time.Minute))
	require.False(t, inspect().Resume.HasID(), "neither a colliding directory's conversation nor an unnamed one is this worktree's")

	write(ownID, claudeRecordLine(t, worktree), past)
	require.Equal(t, ownID, inspect().Resume.ID, "the newest conversation launched here, though others are newer")
}
